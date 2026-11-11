//go:build integration

package db_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/shared/testenv"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/shared/config"
)

func testPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	path, err := filepath.Abs("../../../.env")
	if err != nil {
		t.Fatal(err)
	}
	values, err := godotenv.Read(path)
	if err != nil {
		t.Fatal("a configured worktree .env is required")
	}
	testenv.Apply(t, values)
	cfg, err := config.LoadCenter()
	if err != nil {
		t.Fatal("invalid test configuration")
	}
	// Fail closed before connecting, then verify the server-side identity/marker.
	if err := db.CheckTestDatabase(cfg.Database.Name); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var name, marker string
	err = pool.QueryRow(ctx, "SELECT current_database(), shobj_description(oid,'pg_database') FROM pg_database WHERE datname=current_database()").Scan(&name, &marker)
	if err != nil || name != cfg.Database.Name || marker != "workspaces-project:speedtest" {
		t.Fatal("test database identity/ownership check failed")
	}
	t.Log("PASS: non-live database name and ownership marker verified")
	return pool, ctx
}

func exec(t *testing.T, ctx context.Context, p *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := p.Exec(ctx, sql); err != nil {
		t.Fatalf("fixture/query failed: %v", err)
	}
}

func TestMigrations(t *testing.T) {
	pool, ctx := testPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	if err := db.Migrate(ctx, pool, "up"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Migrate(context.Background(), pool, "down"); err != nil {
			t.Error(err)
		}
	}()
	verifyRequiredTables(t, pool, ctx)
	verifyMigrationTypes(t, pool, ctx)
	node, resultSQL := migrationFixture(t, pool, ctx)
	t.Run("Constraints", func(t *testing.T) { verifyMigrationConstraints(t, pool, ctx, node, resultSQL) })
	t.Run("UTCAndPrecisionRoundTrip", func(t *testing.T) { verifyUTCAndPrecision(t, pool, ctx) })
	t.Run("FailedIPv6ResultPreservesNulls", func(t *testing.T) { verifyFailedIPv6(t, pool, ctx, node) })
	t.Run("PopulatedRollbackAndReapply", func(t *testing.T) { verifyPopulatedRollback(t, pool, ctx) })
	t.Run("CancelledMigration", func(t *testing.T) { verifyCancelledMigration(t, pool) })
	t.Run("TransactionalRollbackFailure", func(t *testing.T) { verifyTransactionalRollback(t, pool, ctx) })
}

func verifyRequiredTables(t *testing.T, pool *pgxpool.Pool, ctx context.Context) {
	t.Helper()
	var got int
	tables := []string{"users", "sessions", "nodes", "node_credentials", "results", "ip_addresses", "node_ip_history", "schedules", "alert_rules", "alert_events", "alert_state", "settings", "agent_commands", "api_idempotency", "node_ip_addresses", "ip_change_signals", "notification_deliveries", "alert_notification_outbox", "maintenance_jobs", "installer_probes", "installer_probe_idempotency", "installer_jobs", "installer_events", "installer_idempotency", "audit_log", "engine_schedules", "user_two_factor", "api_access_tokens", "usage_monthly_totals", "usage_budgets", "usage_budget_events", "usage_budget_overrides"}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name = ANY($1)", tables).Scan(&got); err != nil || got != 32 {
		t.Fatalf("required tables: got %d want 32: %v", got, err)
	}
	t.Log("PASS: 32 required tables created by embedded migrations")
}

func verifyMigrationTypes(t *testing.T, pool *pgxpool.Pool, ctx context.Context) {
	t.Helper()
	expectedIndexes := []string{"results_node_id_ts_idx", "results_ts_idx", "results_node_timeline_idx"}
	var indexes []string
	if err := pool.QueryRow(ctx, `SELECT array_agg(indexname ORDER BY indexname) FROM pg_indexes WHERE schemaname='public' AND tablename='results' AND indexname=ANY($1)`, expectedIndexes).Scan(&indexes); err != nil || len(indexes) != len(expectedIndexes) {
		t.Fatalf("results indexes: got %v, want %v: %v", indexes, expectedIndexes, err)
	}
	var got int
	queries := []struct{ sql, failure string }{
		{"SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND data_type='timestamp without time zone'", "non-timestamptz timestamp"},
		{"SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND column_name LIKE '%mbps' AND data_type NOT IN ('double precision','numeric')", "non-decimal speed"},
	}
	for _, query := range queries {
		if err := pool.QueryRow(ctx, query.sql).Scan(&got); err != nil || got != 0 {
			t.Fatalf("%s: %d %v", query.failure, got, err)
		}
	}
	var typ string
	var def *string
	if err := pool.QueryRow(ctx, "SELECT data_type,column_default FROM information_schema.columns WHERE table_schema='public' AND table_name='results' AND column_name='id'").Scan(&typ, &def); err != nil || typ != "uuid" || def != nil {
		t.Fatal("result UUID must be client supplied")
	}
	var zone string
	if err := pool.QueryRow(ctx, "SHOW timezone").Scan(&zone); err != nil || zone != "UTC" {
		t.Fatal("pool connection must use UTC")
	}
	t.Log("PASS: UUID without default; both results indexes; decimal Mbps; timestamptz only; UTC pool")
}

func migrationFixture(t *testing.T, pool *pgxpool.Pool, ctx context.Context) (string, string) {
	t.Helper()
	const node = "00000000-0000-4000-8000-000000000001"
	exec(t, ctx, pool, "INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES ('"+node+"','fixture',300.5,150.25)")
	exec(t, ctx, pool, "INSERT INTO ip_addresses(ip) VALUES ('203.0.113.1'),('2001:db8::1')")
	resultSQL := "INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,ip,threads,server_id,server) VALUES ('00000000-0000-4000-8000-000000000002','" + node + "','2026-11-03T07:00:00+07:00','ok',123.456,78.125,3.5,'203.0.113.1',4,1,'{\"name\":\"fixture\"}')"
	exec(t, ctx, pool, resultSQL)
	return node, resultSQL
}

func verifyMigrationConstraints(t *testing.T, pool *pgxpool.Pool, ctx context.Context, node, resultSQL string) {
	t.Helper()
	cases := []struct{ name, sql, code string }{
		{"DuplicateResultUUID", resultSQL, "23505"},
		{"MissingResultUUID", "INSERT INTO results(node_id,ts,status,error,ip,threads) VALUES ('" + node + "',now(),'failed','offline','203.0.113.1',4)", "23502"},
		{"ResultNodeForeignKey", "INSERT INTO results(id,node_id,ts,status,error,ip,threads) VALUES ('00000000-0000-4000-8000-000000000003','00000000-0000-4000-8000-000000000099',now(),'failed','offline','203.0.113.1',4)", "23503"},
		{"SessionUserForeignKey", "INSERT INTO sessions(token_hash,user_id,csrf_token_hash,expires_at) VALUES ('hash','00000000-0000-4000-8000-000000000099','csrf',now()+interval '1 hour')", "23503"},
		{"CredentialNodeForeignKey", "INSERT INTO node_credentials(id,node_id,kind,token_hash) VALUES ('00000000-0000-4000-8000-000000000003','00000000-0000-4000-8000-000000000099','bearer','hash')", "23503"},
		{"HistoryIPForeignKey", "INSERT INTO node_ip_history(id,node_id,ts,to_ip) VALUES ('00000000-0000-4000-8000-000000000003','" + node + "',now(),'203.0.113.99')", "23503"},
		{"ScheduleNodeForeignKey", "INSERT INTO schedules(node_id) VALUES ('00000000-0000-4000-8000-000000000099')", "23503"},
		{"AlertRuleForeignKey", "INSERT INTO alert_state(rule_id,node_id) VALUES ('00000000-0000-4000-8000-000000000099','00000000-0000-4000-8000-000000000099')", "23503"},
		{"CommandNodeForeignKey", "INSERT INTO agent_commands(id,node_id,type) VALUES ('00000000-0000-4000-8000-000000000003','00000000-0000-4000-8000-000000000099','pause')", "23503"},
		{"NegativeSpeed", "UPDATE results SET down_mbps=-1", "23514"},
		{"NaNSpeed", "UPDATE results SET down_mbps='NaN'", "23514"},
		{"InfiniteSpeed", "UPDATE results SET up_mbps='Infinity'", "23514"},
		{"LossAbove100", "UPDATE results SET loss_percent=101", "23514"},
		{"MissingSuccessfulMeasurement", "UPDATE results SET down_mbps=NULL", "23514"},
		{"FailedWithoutError", "UPDATE results SET status='failed',error=''", "23514"},
		{"InvalidOffset", "INSERT INTO schedules(node_id,offset_minutes) VALUES ('" + node + "',60)", "23514"},
		{"NoSecretOrSettingValue", "INSERT INTO settings(key) VALUES ('empty')", "23514"},
		{"MixedSecretAndPublicValue", "INSERT INTO settings(key,value,secret_ciphertext) VALUES ('mixed','{}','ciphertext')", "23514"},
		{"IPv4SubnetNotHost", "INSERT INTO ip_addresses(ip) VALUES ('203.0.113.0/24')", "23514"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pool.Exec(ctx, tc.sql)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != tc.code {
				t.Fatalf("expected SQLSTATE %s, got %v", tc.code, err)
			}
		})
	}
}

func verifyUTCAndPrecision(t *testing.T, pool *pgxpool.Pool, ctx context.Context) {
	t.Helper()
	var ts time.Time
	var down, up float64
	if err := pool.QueryRow(ctx, "SELECT ts,down_mbps,up_mbps FROM results").Scan(&ts, &down, &up); err != nil || !ts.Equal(time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)) || down != 123.456 || up != 78.125 {
		t.Fatal("UTC/Mbps round trip failed")
	}
}

func verifyFailedIPv6(t *testing.T, pool *pgxpool.Pool, ctx context.Context, node string) {
	t.Helper()
	exec(t, ctx, pool, "INSERT INTO results(id,node_id,ts,status,error,ip,threads) VALUES ('00000000-0000-4000-8000-000000000003','"+node+"',now(),'failed','offline','2001:db8::1',1)")
}

func verifyPopulatedRollback(t *testing.T, pool *pgxpool.Pool, ctx context.Context) {
	t.Helper()
	exec(t, ctx, pool, `INSERT INTO users(id,email,password_hash) VALUES ('00000000-0000-4000-8000-000000000010','test@example.com','argon2id-hash');
INSERT INTO sessions(token_hash,user_id,csrf_token_hash,expires_at) VALUES ('hash','00000000-0000-4000-8000-000000000010','csrf',now()+interval '1 hour');
INSERT INTO node_credentials(id,node_id,kind,token_hash) SELECT '00000000-0000-4000-8000-000000000011',id,'bearer','bearer-hash' FROM nodes;
INSERT INTO schedules(node_id) SELECT id FROM nodes;
INSERT INTO node_ip_history(id,node_id,ts,to_ip) SELECT '00000000-0000-4000-8000-000000000012',id,now(),'203.0.113.1' FROM nodes;
INSERT INTO alert_rules(id,name,metric,op,value,for_tests,severity,channels) VALUES ('00000000-0000-4000-8000-000000000013','slow','down_pct','<',80,3,'warn',ARRAY['email']);
INSERT INTO alert_events(id,rule_id,node_id,severity,ts,title,trigger_result_id) SELECT '00000000-0000-4000-8000-000000000014','00000000-0000-4000-8000-000000000013',id,'warn',now(),'Slow','00000000-0000-4000-8000-000000000002' FROM nodes;
INSERT INTO alert_state(rule_id,node_id,active,active_event_id) SELECT '00000000-0000-4000-8000-000000000013',id,true,'00000000-0000-4000-8000-000000000014' FROM nodes;
INSERT INTO alert_notification_outbox(event_id,channels) VALUES ('00000000-0000-4000-8000-000000000014',ARRAY['email']);
INSERT INTO notification_deliveries(id,event_id,channel) VALUES ('00000000-0000-4000-8000-000000000016','00000000-0000-4000-8000-000000000014','email');
INSERT INTO settings(key,value) VALUES ('display','{"theme":"dark"}');
INSERT INTO settings(key,secret_ciphertext) VALUES ('integration',decode('aabb','hex'));
INSERT INTO api_idempotency(principal,operation,key_hash,request_hash,response_ciphertext,expires_at) VALUES ('owner','create-node',repeat('a',64),repeat('b',64),decode(repeat('00',28),'hex'),now()+interval '1 hour');
INSERT INTO api_access_tokens(id,user_id,name,scope,token_hash) VALUES ('00000000-0000-4000-8000-000000000017','00000000-0000-4000-8000-000000000010','rollback-fixture','read',decode(repeat('aa',32),'hex'));
INSERT INTO agent_commands(id,node_id,type,result_id) SELECT '00000000-0000-4000-8000-000000000015',id,'run_now','00000000-0000-4000-8000-000000000002' FROM nodes;`)
	for range 14 {
		if err := db.Migrate(ctx, pool, "down"); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_type='BASE TABLE' AND table_name <> 'goose_db_version'").Scan(&count); err != nil || count != 0 {
		t.Fatal("rollback left application tables")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() { errs <- db.Migrate(ctx, pool, "up") })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := db.Migrate(ctx, pool, "up"); err != nil {
			t.Fatal(err)
		}
	}
	t.Log("PASS: populated rollback, concurrent reapply and idempotent up")
}

func verifyCancelledMigration(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := db.Migrate(ctx, pool, "up"); err == nil {
		t.Fatal("expected cancellation error")
	}
}

func verifyTransactionalRollback(t *testing.T, pool *pgxpool.Pool, ctx context.Context) {
	t.Helper()
	for range 13 {
		if err := db.Migrate(ctx, pool, "down"); err != nil {
			t.Fatal(err)
		}
	}
	exec(t, ctx, pool, "CREATE VIEW rollback_blocker AS SELECT id FROM users")
	t.Cleanup(func() { exec(t, context.Background(), pool, "DROP VIEW IF EXISTS rollback_blocker") })
	if err := db.Migrate(ctx, pool, "down"); err == nil {
		t.Fatal("expected external dependency to stop rollback")
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_type='BASE TABLE' AND table_name <> 'goose_db_version'").Scan(&count); err != nil || count != 13 {
		t.Fatal("failed rollback was not atomic")
	}
	exec(t, ctx, pool, "DROP VIEW rollback_blocker")
}

func TestScheduleDefaultMigrationPreservesIntervalsIntegration(t *testing.T) {
	pool, ctx := testPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	if err := db.Migrate(ctx, pool, "up"); err != nil {
		t.Fatal(err)
	}
	const nodeIDs = "'00000000-0000-4000-8000-000000000101','00000000-0000-4000-8000-000000000102','00000000-0000-4000-8000-000000000103'"
	exec(t, ctx, pool, "INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) SELECT id,'schedule-'||row_number() OVER (),100,100 FROM unnest(ARRAY["+nodeIDs+"]::uuid[]) AS items(id)")
	exec(t, ctx, pool, `INSERT INTO schedules(node_id,interval_minutes,offset_minutes)
VALUES ('00000000-0000-4000-8000-000000000101',5,0),
       ('00000000-0000-4000-8000-000000000102',30,7),
       ('00000000-0000-4000-8000-000000000103',60,11)`)
	for range 4 { // Roll back usage, audit-source, API-token and 60-minute-default migrations.
		if err := db.Migrate(ctx, pool, "down"); err != nil {
			t.Fatal(err)
		}
	}
	assertScheduleIntervals(t, ctx, pool, "5:0,30:7,60:11")
	exec(t, ctx, pool, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps)
VALUES ('00000000-0000-4000-8000-000000000104','before-upgrade',100,100);
INSERT INTO schedules(node_id) VALUES ('00000000-0000-4000-8000-000000000104')`)
	assertScheduleInterval(t, ctx, pool, "00000000-0000-4000-8000-000000000104", 30)
	if err := db.Migrate(ctx, pool, "up"); err != nil {
		t.Fatal(err)
	}
	assertScheduleIntervals(t, ctx, pool, "5:0,30:7,60:11,30:0")
	exec(t, ctx, pool, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps)
VALUES ('00000000-0000-4000-8000-000000000105','after-upgrade',100,100);
INSERT INTO schedules(node_id) VALUES ('00000000-0000-4000-8000-000000000105')`)
	assertScheduleInterval(t, ctx, pool, "00000000-0000-4000-8000-000000000105", 60)
	assertScheduleIntervals(t, ctx, pool, "5:0,30:7,60:11,30:0,60:0")
	t.Log("PASS: migration up/down preserves 5/30/60-minute rows; rollback default is 30; upgraded default is 60")
}

func assertScheduleIntervals(t *testing.T, ctx context.Context, pool *pgxpool.Pool, want string) {
	t.Helper()
	var got string
	err := pool.QueryRow(ctx, `SELECT string_agg(interval_minutes||':'||offset_minutes,',' ORDER BY node_id)
FROM schedules WHERE node_id::text >= '00000000-0000-4000-8000-000000000101'
  AND node_id::text <= '00000000-0000-4000-8000-000000000105'`).Scan(&got)
	if err != nil || got != want {
		t.Fatalf("schedule intervals = %q, want %q: %v", got, want, err)
	}
}

func assertScheduleInterval(t *testing.T, ctx context.Context, pool *pgxpool.Pool, nodeID string, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(ctx, "SELECT interval_minutes FROM schedules WHERE node_id=$1", nodeID).Scan(&got); err != nil || got != want {
		t.Fatalf("schedule default for %s = %d, want %d: %v", nodeID, got, want, err)
	}
}
