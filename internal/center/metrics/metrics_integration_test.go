//go:build integration

package metrics_test

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/metrics"
	"github.com/jooservices/speedtest/internal/center/usage"
	"github.com/jooservices/speedtest/internal/shared/config"
)

func openPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	values, err := godotenv.Read(filepath.Join("..", "..", "..", ".env"))
	if err != nil {
		t.Fatal("a configured worktree .env is required")
	}
	for k, v := range values {
		t.Setenv(k, v)
	}
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

func TestCollectAgainstSchema(t *testing.T) {
	pool, ctx := openPool(t)
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES
	 ('00000000-0000-4000-8000-0000000000b1','n1',100,50)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ip_addresses(ip) VALUES ('203.0.113.1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,server,ip,threads,bytes_down,bytes_up,engine)
	 VALUES ('00000000-0000-4000-8000-0000000000b2','00000000-0000-4000-8000-0000000000b1',$1,'ok',10,5,2,1,'{"name":"Metrics fixture"}','203.0.113.1',4,1234,456,'ookla')`, now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	store := &usage.Store{DB: pool, Now: func() time.Time { return now }}
	_, tag, err := store.Budgets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, tag, err = store.ReplaceBudget(ctx, usage.Budget{MonthlyBytes: 10000, Action: "pause"}, tag); err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.ReplaceBudget(ctx, usage.Budget{NodeID: "00000000-0000-4000-8000-0000000000b1", MonthlyBytes: 20000, Action: "widen", WidenMultiplier: 2}, tag); err != nil {
		t.Fatal(err)
	}
	s, err := metrics.Collector{DB: pool, Now: func() time.Time { return now }}.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.NodesEnabled != 1 || s.NodesOffline != 1 || s.LastResultAgeSeconds < 0 || len(s.Bandwidth) != 1 || s.Bandwidth[0].Bytes != 1690 || len(s.BandwidthBudgetRatios) != 2 {
		t.Fatalf("%+v", s)
	}
	var b bytes.Buffer
	s.Write(&b)
	if !strings.Contains(b.String(), "speedtest_nodes_offline 1") {
		t.Fatal(b.String())
	}
	for _, want := range []string{
		`speedtest_bandwidth_month_to_date_bytes{node_id="00000000-0000-4000-8000-0000000000b1",engine="ookla"} 1690`,
		`speedtest_bandwidth_budget_ratio{scope="global",node_id=""} 0.169`,
		`speedtest_bandwidth_budget_ratio{scope="node",node_id="00000000-0000-4000-8000-0000000000b1"} 0.0845`,
	} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("missing metric %q in:\n%s", want, b.String())
		}
	}
}
