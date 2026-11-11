//go:build integration

package telemetry

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/shared/config"
	"github.com/jooservices/speedtest/internal/shared/testenv"
)

func openPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	values, err := godotenv.Read(filepath.Join("..", "..", "..", ".env"))
	if err != nil {
		t.Fatal("a configured worktree .env is required")
	}
	testenv.Apply(t, values)
	cfg, err := config.LoadCenter()
	if err != nil {
		t.Fatal("invalid test configuration")
	}
	if err := db.CheckTestDatabase(cfg.Database.Name); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	return pool, ctx
}

func TestScrapeDatabaseAgesCapAndPoolIntegration(t *testing.T) {
	pool, ctx := openPool(t)
	meter := New("integration")
	meter.RegisterDatabase(pool)
	f := scrape(t, meter)
	if f["speedtest_node_heartbeat_age_seconds"] != nil {
		t.Fatal("empty fleet has node series")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps,last_seen_at)
 SELECT ('00000000-0000-4000-8000-' || lpad(g::text,12,'0'))::uuid,'node-'||g,100,50,statement_timestamp()-interval '30 seconds'
 FROM generate_series(1,201) g`); err != nil {
		t.Fatal(err)
	}
	f = scrape(t, meter)
	for _, name := range []string{"speedtest_node_heartbeat_age_seconds", "speedtest_node_last_result_age_seconds"} {
		if len(f[name].Metric) != 200 {
			t.Fatal("node cap", name, len(f[name].Metric))
		}
	}
	for _, m := range f["speedtest_node_heartbeat_age_seconds"].Metric {
		if age := m.GetGauge().GetValue(); age < 30 || age > 35 {
			t.Fatal(age)
		}
	}
	for _, m := range f["speedtest_node_last_result_age_seconds"].Metric {
		if m.GetGauge().GetValue() != -1 {
			t.Fatal("missing result sentinel")
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ip_addresses(ip) VALUES('203.0.113.5');
 INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,server,ip,threads)
 VALUES('10000000-0000-4000-8000-000000000001','00000000-0000-4000-8000-000000000001',statement_timestamp()-interval '60 seconds','ok',1,1,1,1,'{"id":1,"name":"s","city":"c","sponsor":"p"}','203.0.113.5',4)`); err != nil {
		t.Fatal(err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f = scrape(t, meter)
	conn.Release()
	age := f["speedtest_node_last_result_age_seconds"].Metric[0].GetGauge().GetValue()
	if age < 60 || age > 65 {
		t.Fatal(age)
	}
	if f["speedtest_db_pool_acquired_connections"].Metric[0].GetGauge().GetValue() < 1 {
		t.Fatal("pool snapshot ignored acquired connection")
	}
	if _, err := pool.Exec(ctx, `UPDATE nodes SET last_seen_at=NULL WHERE id='00000000-0000-4000-8000-000000000001'; DELETE FROM nodes WHERE id='00000000-0000-4000-8000-000000000201'`); err != nil {
		t.Fatal(err)
	}
	f = scrape(t, meter)
	if f["speedtest_node_heartbeat_age_seconds"].Metric[0].GetGauge().GetValue() != -1 {
		t.Fatal("missing heartbeat sentinel")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM nodes WHERE id='00000000-0000-4000-8000-000000000200'`); err != nil {
		t.Fatal(err)
	}
	if len(scrape(t, meter)["speedtest_node_heartbeat_age_seconds"].Metric) != 199 {
		t.Fatal("deleted node series retained")
	}
}
