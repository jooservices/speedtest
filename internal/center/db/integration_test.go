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

func currentMigrationVersion(t *testing.T, ctx context.Context, p *pgxpool.Pool) int64 {
	t.Helper()
	var version int64
	if err := p.QueryRow(ctx, `SELECT COALESCE(MAX(version_id) FILTER (WHERE is_applied),0) FROM goose_db_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

func TestMigrations(t *testing.T) {
	pool, ctx := testPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	if err := db.Migrate(ctx, pool, "up"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if e := db.Migrate(context.Background(), pool, "down"); e != nil {
			t.Error(e)
		}
	}()
	t.Run("AllRequiredTables", func(t *testing.T) {
		var got int
		tables := []string{"users", "sessions", "nodes", "node_credentials", "results", "ip_addresses", "node_ip_history", "schedules", "alert_rules", "alert_events", "alert_state", "settings", "agent_commands", "api_idempotency", "node_ip_addresses", "ip_change_signals", "notification_deliveries", "alert_notification_outbox", "maintenance_jobs", "installer_probes", "installer_probe_idempotency", "installer_jobs", "installer_events", "installer_idempotency", "audit_log", "engine_schedules", "user_two_factor", "api_access_tokens", "monthly_report_runs", "digest_periods", "digest_schedule_state"} // every table a migration creates may be listed; the count follows the list
		if e := pool.QueryRow(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name = ANY($1)", tables).Scan(&got); e != nil || got != len(tables) {
			t.Fatalf("required tables: got %d want %d: %v", got, len(tables), e)
		}
		var defaults int
		if e := pool.QueryRow(ctx, `SELECT count(*) FROM alert_rules WHERE metric IN ('offline','failed','down_pct','up_pct','ping','jitter','loss','ip_changed')`).Scan(&defaults); e != nil || defaults != 8 {
			t.Fatalf("seeded alert rules: got %d want 8: %v", defaults, e)
		}
		t.Log("PASS: 37 required tables and eight baseline alert rules created by embedded migrations")
	})
	t.Run("IndexesUUIDUnitsAndTimeTypes", func(t *testing.T) {
		var got int
		if e := pool.QueryRow(ctx, "SELECT count(*) FROM pg_indexes WHERE schemaname='public' AND tablename='results' AND (indexdef LIKE '%(node_id, ts)%' OR indexdef LIKE '%(ts)%')").Scan(&got); e != nil || got != 2 {
			t.Fatalf("results indexes: %d %v", got, e)
		}
		if e := pool.QueryRow(ctx, "SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND data_type='timestamp without time zone'").Scan(&got); e != nil || got != 0 {
			t.Fatal("non-timestamptz timestamp")
		}
		if e := pool.QueryRow(ctx, "SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND column_name LIKE '%mbps' AND data_type NOT IN ('double precision','numeric')").Scan(&got); e != nil || got != 0 {
			t.Fatal("non-decimal speed")
		}
		var typ string
		var def *string
		if e := pool.QueryRow(ctx, "SELECT data_type,column_default FROM information_schema.columns WHERE table_schema='public' AND table_name='results' AND column_name='id'").Scan(&typ, &def); e != nil || typ != "uuid" || def != nil {
			t.Fatal("result UUID must be client supplied")
		}
		var zone string
		if e := pool.QueryRow(ctx, "SHOW timezone").Scan(&zone); e != nil || zone != "UTC" {
			t.Fatal("pool connection must use UTC")
		}
		t.Log("PASS: UUID without default; both results indexes; decimal Mbps; timestamptz only; UTC pool")
	})
	const node = "00000000-0000-4000-8000-000000000001"
	exec(t, ctx, pool, "INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES ('"+node+"','fixture',300.5,150.25)")
	exec(t, ctx, pool, "INSERT INTO ip_addresses(ip) VALUES ('203.0.113.1'),('2001:db8::1')")
	resultSQL := "INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,ip,threads,server_id,server) VALUES ('00000000-0000-4000-8000-000000000002','" + node + "','2026-11-03T07:00:00+07:00','ok',123.456,78.125,3.5,'203.0.113.1',4,1,'{\"name\":\"fixture\"}')"
	exec(t, ctx, pool, resultSQL)
	t.Run("Constraints", func(t *testing.T) {
		cases := []struct{ name, sql, code string }{
			{"DuplicateResultUUID", resultSQL, "23505"},
			{"MissingResultUUID", "INSERT INTO results(node_id,ts,status,error,ip,threads) VALUES ('" + node + "',now(),'failed','offline','203.0.113.1',4)", "23502"},
			{"ResultNodeForeignKey", "INSERT INTO results(id,node_id,ts,status,error,ip,threads) VALUES ('00000000-0000-4000-8000-000000000003','00000000-0000-4000-8000-000000000099',now(),'failed','offline','203.0.113.1',4)", "23503"},
			{"SessionUserForeignKey", "INSERT INTO sessions(token_hash,user_id,csrf_token_hash,expires_at) VALUES ('hash','00000000-0000-4000-8000-000000000099','csrf',now()+interval '1 hour')", "23503"},
			{"CredentialNodeForeignKey", "INSERT INTO node_credentials(id,node_id,kind,token_hash) VALUES ('00000000-0000-4000-8000-000000000003','00000000-0000-4000-8000-000000000099','bearer','hash')", "23503"},
			{"HistoryIPForeignKey", "INSERT INTO node_ip_history(id,node_id,ts,to_ip) VALUES ('00000000-0000-4000-8000-000000000003','" + node + "',now(),'203.0.113.99')", "23503"},
			{"ScheduleNodeForeignKey", "INSERT INTO schedules(node_id) VALUES ('00000000-0000-4000-8000-000000000099')", "23503"},
			{"AlertRuleForeignKey", "INSERT INTO alert_state(rule_id,node_id) VALUES ('00000000-0000-4000-8000-000000000099','" + node + "')", "23503"},
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
				_, e := pool.Exec(ctx, tc.sql)
				var pe *pgconn.PgError
				if !errors.As(e, &pe) || pe.Code != tc.code {
					t.Fatalf("expected SQLSTATE %s, got %v", tc.code, e)
				}
			})
		}
	})
	t.Run("UTCAndPrecisionRoundTrip", func(t *testing.T) {
		var ts time.Time
		var down, up float64
		if e := pool.QueryRow(ctx, "SELECT ts,down_mbps,up_mbps FROM results").Scan(&ts, &down, &up); e != nil || !ts.Equal(time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)) || down != 123.456 || up != 78.125 {
			t.Fatal("UTC/Mbps round trip failed")
		}
	})
	t.Run("FailedIPv6ResultPreservesNulls", func(t *testing.T) {
		exec(t, ctx, pool, "INSERT INTO results(id,node_id,ts,status,error,ip,threads) VALUES ('00000000-0000-4000-8000-000000000003','"+node+"',now(),'failed','offline','2001:db8::1',1)")
	})
	t.Run("PopulatedRollbackAndReapply", func(t *testing.T) {
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
		if e := db.MigrateDownTo(ctx, pool, 0); e != nil {
			t.Fatalf("rollback all migrations: %v", e)
		}
		var n int
		if e := pool.QueryRow(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_type='BASE TABLE' AND table_name <> 'goose_db_version'").Scan(&n); e != nil || n != 0 {
			t.Fatal("rollback left application tables")
		}
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for range 2 {
			wg.Go(func() { errs <- db.Migrate(ctx, pool, "up") })
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			if e != nil {
				t.Fatal(e)
			}
		}
		if e := db.Migrate(ctx, pool, "up"); e != nil {
			t.Fatal(e)
		}
		if e := db.Migrate(ctx, pool, "up"); e != nil {
			t.Fatal(e)
		}
		t.Log("PASS: populated rollback, concurrent reapply and idempotent up")
	})
	t.Run("CancelledMigration", func(t *testing.T) {
		c, cancel := context.WithCancel(ctx)
		cancel()
		if e := db.Migrate(c, pool, "up"); e == nil {
			t.Fatal("expected cancellation error")
		}
	})
	t.Run("TransactionalRollbackFailure", func(t *testing.T) {
		if e := db.MigrateDownTo(ctx, pool, 1); e != nil {
			t.Fatal(e)
		}
		exec(t, ctx, pool, "CREATE VIEW rollback_blocker AS SELECT id FROM users")
		t.Cleanup(func() { exec(t, context.Background(), pool, "DROP VIEW IF EXISTS rollback_blocker") })
		if e := db.Migrate(ctx, pool, "down"); e == nil {
			t.Fatal("expected external dependency to stop rollback")
		}
		var n int
		if e := pool.QueryRow(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_type='BASE TABLE' AND table_name <> 'goose_db_version'").Scan(&n); e != nil || n != 13 {
			t.Fatal("failed rollback was not atomic")
		}
		exec(t, ctx, pool, "DROP VIEW rollback_blocker")
	})
}

func TestDevelopV22UpgradeAndRollbackIntegration(t *testing.T) {
	pool, ctx := testPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	if err := db.Migrate(ctx, pool, "up"); err != nil {
		t.Fatal(err)
	}
	if err := db.MigrateDownTo(ctx, pool, 22); err != nil {
		t.Fatal(err)
	}
	if version := currentMigrationVersion(t, ctx, pool); version != 22 {
		t.Fatalf("develop baseline version = %d, want 22", version)
	}
	insertDevelopV20Fixtures(t, ctx, pool)
	if err := db.Migrate(ctx, pool, "up"); err != nil {
		t.Fatal(err)
	}
	assertDevelopV20Rows(t, ctx, pool)
	if err := db.MigrateDownTo(ctx, pool, 22); err != nil {
		t.Fatal(err)
	}
	assertDevelopV20Rows(t, ctx, pool)
	if version := currentMigrationVersion(t, ctx, pool); version != 22 {
		t.Fatalf("rollback version = %d, want 22", version)
	}
	t.Log("PASS: populated develop-v22 upgrade/rollback preserves API token, audit columns and scoped alert rules")
}

func insertDevelopV20Fixtures(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	exec(t, ctx, pool, `INSERT INTO users(id,email,password_hash) VALUES('82000000-0000-4000-8000-000000000001','migration-owner@example.test','test-hash');
INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES('82000000-0000-4000-8000-000000000002','migration-node',100,50);
INSERT INTO api_access_tokens(id,user_id,name,scope,token_hash) VALUES('82000000-0000-4000-8000-000000000003','82000000-0000-4000-8000-000000000001','v20-token','read',decode(repeat('ab',32),'hex'));
INSERT INTO audit_log(id,actor,action,target_type,target_id,auth_method,credential_id)
VALUES('82000000-0000-4000-8000-000000000004','owner@example.test','fixture','token','fixture','bearer','82000000-0000-4000-8000-000000000003');
INSERT INTO alert_rules(id,name,metric,op,value,for_tests,node_ids,severity,channels,engine) VALUES
('82000000-0000-4000-8000-000000000011','Owner global download','down_pct','<',67.5,3,NULL,'warn',ARRAY['telegram'],NULL),
('82000000-0000-4000-8000-000000000012','Node ping override','ping','>',81,4,ARRAY['82000000-0000-4000-8000-000000000002']::uuid[],'crit',ARRAY['email'],'ookla'),
('82000000-0000-4000-8000-000000000013','Engine jitter override','jitter','>',22,5,NULL,'warn',ARRAY['slack'],'librespeed');`)
}

func assertDevelopV20Rows(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var tokens, audits, authMethods, scopedRules int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM api_access_tokens WHERE id='82000000-0000-4000-8000-000000000003'`).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE auth_method='bearer') FROM audit_log WHERE id='82000000-0000-4000-8000-000000000004'`).Scan(&audits, &authMethods); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM alert_rules WHERE id IN ('82000000-0000-4000-8000-000000000011','82000000-0000-4000-8000-000000000012','82000000-0000-4000-8000-000000000013') AND value IN (67.5,81,22)`).Scan(&scopedRules); err != nil {
		t.Fatal(err)
	}
	if tokens != 1 || audits != 1 || authMethods != 1 || scopedRules != 3 {
		t.Fatalf("v20 rows changed: token=%d audit=%d bearer=%d rules=%d", tokens, audits, authMethods, scopedRules)
	}
	if currentMigrationVersion(t, ctx, pool) > 26 {
		var settingsThreshold float64
		if err := pool.QueryRow(ctx, `SELECT (thresholds->>'down_pct')::float8 FROM notification_event_settings WHERE event_key='quality.download_low'`).Scan(&settingsThreshold); err != nil || settingsThreshold != 67.5 {
			t.Fatalf("migrated global threshold = %v: %v", settingsThreshold, err)
		}
	}
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
	if err := db.MigrateDownTo(ctx, pool, 14); err != nil { // Restore the schema before the 60-minute schedule default.
		t.Fatal(err)
	}
	assertScheduleIntervals(t, ctx, pool, "5:0,30:7,60:11")
	exec(t, ctx, pool, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps)
VALUES ('00000000-0000-4000-8000-000000000104','before-upgrade',100,100);
INSERT INTO schedules(node_id) VALUES ('00000000-0000-4000-8000-000000000104')`)
	assertScheduleInterval(t, ctx, pool, "00000000-0000-4000-8000-000000000104", 30)
	if err := db.Migrate(ctx, pool, "up"); err != nil {
		t.Fatal(err)
	}
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

func TestNotificationDefaultsKeepDistinctGlobalThresholdsIntegration(t *testing.T) {
	pool, ctx := testPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	if err := db.Migrate(ctx, pool, "up"); err != nil {
		t.Fatal(err)
	}
	if err := db.MigrateDownTo(ctx, pool, 24); err != nil { // before the notification migrations (00026 and later; 00025 is the server catalogs of JOOST-76)
		t.Fatal(err)
	}
	exec(t, ctx, pool, `DELETE FROM alert_rules;
INSERT INTO alert_rules(id,name,metric,op,value,for_tests,node_ids,severity,channels,enabled,engine) VALUES
 ('00000000-0000-4000-8000-000000000201','Download strict','down_pct','<',50,2,NULL,'warn',ARRAY['email'],true,NULL),
 ('00000000-0000-4000-8000-000000000202','Download lenient','down_pct','<',80,3,NULL,'crit',ARRAY['email'],true,NULL),
 ('00000000-0000-4000-8000-000000000203','Ping only','ping','>',40,2,NULL,'warn',ARRAY['email'],true,NULL)`)
	if err := db.Migrate(ctx, pool, "up"); err != nil {
		t.Fatal(err)
	}
	var settings int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notification_event_settings WHERE event_key='quality.download_low'`).Scan(&settings); err != nil || settings != 0 {
		t.Fatalf("two global rules of one metric must not collapse into one event setting: %d %v", settings, err)
	}
	var ping float64
	if err := pool.QueryRow(ctx, `SELECT (thresholds->>'ping')::float8 FROM notification_event_settings WHERE event_key='quality.ping_high'`).Scan(&ping); err != nil || ping != 40 {
		t.Fatalf("a single global rule seeds its event setting: %v %v", ping, err)
	}
	var values string
	if err := pool.QueryRow(ctx, `SELECT string_agg(value::text||'/'||for_tests::text,',' ORDER BY id) FROM alert_rules WHERE metric='down_pct'`).Scan(&values); err != nil || values != "50/2,80/3" {
		t.Fatalf("both download rules keep their own threshold and streak: %q %v", values, err)
	}
}

func TestDeliveryFilterIndexMigrationRollbackIntegration(t *testing.T) {
	pool, ctx := testPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	if err := db.Migrate(ctx, pool, "up"); err != nil {
		t.Fatal(err)
	}
	latestVersion := currentMigrationVersion(t, ctx, pool)
	exec(t, ctx, pool, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps)
      VALUES('10000000-0000-4000-8000-000000000124','filter-rollback',100,50)`)
	exec(t, ctx, pool, `INSERT INTO alert_events(id,node_id,severity,ts,title)
      VALUES('a0000000-0000-4000-8000-000000000124','10000000-0000-4000-8000-000000000124','warn',now(),'Filter rollback')`)
	exec(t, ctx, pool, `INSERT INTO notification_deliveries(id,event_id,channel,status)
      VALUES('f0000000-0000-4000-8000-000000000124','a0000000-0000-4000-8000-000000000124','email','sent')`)
	assertDeliveryFilterMigrationState(t, ctx, pool, latestVersion, 3)
	if err := db.MigrateDownTo(ctx, pool, 35); err != nil {
		t.Fatal(err)
	}
	assertDeliveryFilterMigrationState(t, ctx, pool, 35, 0)
	if err := db.Migrate(ctx, pool, "up"); err != nil {
		t.Fatal(err)
	}
	assertDeliveryFilterMigrationState(t, ctx, pool, latestVersion, 3)
}

func assertDeliveryFilterMigrationState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, version int64, indexes int) {
	t.Helper()
	if got := currentMigrationVersion(t, ctx, pool); got != version {
		t.Fatalf("migration version = %d, want %d", got, version)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notification_deliveries WHERE status='sent' AND channel='email'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("delivery rows after migration = %d, error=%v", count, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname='public' AND tablename='notification_deliveries'
      AND indexname = ANY(ARRAY['notification_deliveries_status_created_idx','notification_deliveries_channel_created_idx','notification_deliveries_status_channel_created_idx'])`).Scan(&count); err != nil || count != indexes {
		t.Fatalf("delivery filter indexes = %d, want %d, error=%v", count, indexes, err)
	}
}
