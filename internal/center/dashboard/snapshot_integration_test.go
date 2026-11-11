//go:build integration

package dashboard

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/stats"
	"github.com/jooservices/speedtest/internal/shared/config"
)

type concurrentDashboardDB struct {
	*pgxpool.Pool
	insert func() error
	done   bool
}

func (db *concurrentDashboardDB) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	rows, err := db.Pool.Query(ctx, query, args...)
	if err != nil || db.done {
		return rows, err
	}
	db.done = true
	if err := db.insert(); err != nil {
		rows.Close()
		return nil, err
	}
	return rows, nil
}

type dashboardNodeSource struct{ id string }

func (n dashboardNodeSource) List(context.Context, int, int) ([]map[string]any, int, error) {
	return []map[string]any{{"id": n.id, "name": "Concurrent dashboard"}}, 1, nil
}

type summaryOnlyStats struct{ store *stats.Store }

func (s summaryOnlyStats) Summary(ctx context.Context, q stats.Query) ([]stats.SummaryComparison, error) {
	return s.store.Summary(ctx, q)
}
func (summaryOnlyStats) Series(context.Context, stats.Query, bool, int) ([]stats.Series, error) {
	return []stats.Series{}, nil
}

func openDashboardIntegrationDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	values, err := godotenv.Read(filepath.Join("..", "..", "..", ".env"))
	if err != nil {
		t.Fatal("a configured worktree .env is required")
	}
	for key, value := range values {
		t.Setenv(key, value)
	}
	cfg, err := config.LoadCenter()
	if err != nil {
		t.Fatal("invalid worktree test configuration")
	}
	if err := db.CheckTestDatabase(cfg.Database.Name); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal("cannot connect to configured worktree database")
	}
	t.Cleanup(pool.Close)
	var name, marker string
	if err := pool.QueryRow(ctx, `SELECT current_database(), shobj_description(oid, 'pg_database') FROM pg_database WHERE datname=current_database()`).Scan(&name, &marker); err != nil || name != cfg.Database.Name || marker != "workspaces-project:speedtest" {
		t.Fatal("integration database identity and ownership marker verification failed")
	}
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	return pool, ctx
}

func TestSnapshotKeepsDashboardCountsConsistentDuringRateLimitIngestion(t *testing.T) {
	pool, ctx := openDashboardIntegrationDB(t)
	const node = "00000000-0000-4000-8000-000000000080"
	now := time.Now().UTC().Truncate(time.Second)
	from := now.Add(-2 * time.Hour)
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES ($1,'Concurrent dashboard',100,50)`, node); err != nil {
		t.Fatal("could not create dashboard node")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ip_addresses(ip) VALUES ('203.0.113.80')`); err != nil {
		t.Fatal("could not create dashboard IP")
	}
	for i, at := range []time.Time{from.Add(time.Hour), from.Add(90 * time.Minute)} {
		insertDashboardSuccess(t, pool, ctx, node, i+1, at)
	}
	interleaved := &concurrentDashboardDB{Pool: pool, insert: func() error {
		_, err := pool.Exec(ctx, `INSERT INTO results(id,node_id,ts,status,threads,error,engine,failure_class)
			VALUES ('00000000-0000-4000-8000-000000000083',$1,$2,'failed',4,'Too many requests','ookla','rate_limited')`, node, now.Add(-15*time.Minute))
		return err
	}}
	statsStore := &stats.Store{DB: interleaved}
	store := Store{DB: interleaved, Nodes: dashboardNodeSource{id: node}, Stats: summaryOnlyStats{statsStore}, Results: &fakeResults{}, Alerts: &fakeAlerts{}, Now: func() time.Time { return now }}
	query := stats.Query{From: from, To: now, PreviousFrom: from.Add(-2 * time.Hour), PreviousTo: from}
	snapshot, err := store.Snapshot(ctx, query)
	if err != nil {
		t.Fatal("dashboard snapshot failed", err)
	}
	if len(snapshot.Nodes) != 1 || snapshot.Current.Count != 2 || snapshot.Current.RateLimited != 0 || snapshot.Current.Success == nil || *snapshot.Current.Success != 100 || snapshot.Nodes[0].Summary.Count != 2 || snapshot.Nodes[0].Summary.SuccessRate == nil || *snapshot.Nodes[0].Summary.SuccessRate != 100 {
		t.Fatalf("dashboard mixed snapshots = fleet %+v node %+v", snapshot.Current, snapshot.Nodes[0].Summary)
	}
	var inserted int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM results WHERE failure_class='rate_limited'`).Scan(&inserted); err != nil || inserted != 1 {
		t.Fatalf("concurrent rate-limit fixture count = %d, error %v", inserted, err)
	}
}

func insertDashboardSuccess(t *testing.T, pool *pgxpool.Pool, ctx context.Context, node string, index int, at time.Time) {
	t.Helper()
	id := fmt.Sprintf("00000000-0000-4000-8000-%012x", 0x80+index)
	if _, err := pool.Exec(ctx, `INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,threads,engine,ip,server)
		VALUES ($1,$2,$3,'ok',100,50,10,4,'ookla','203.0.113.80','{"name":"Fixture"}')`, id, node, at); err != nil {
		t.Fatal("could not create dashboard result")
	}
}
