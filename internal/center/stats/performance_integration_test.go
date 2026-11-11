//go:build integration

package stats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/shared/testenv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/usage"
	"github.com/jooservices/speedtest/internal/shared/config"
)

func openStatsIntegrationStore(t *testing.T) (*pgxpool.Pool, context.Context, *Store) {
	t.Helper()
	values, err := godotenv.Read(filepath.Join("..", "..", "..", ".env"))
	if err != nil {
		t.Fatal("a configured worktree .env is required")
	}
	testenv.Apply(t, values)
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
	return pool, ctx, &Store{DB: pool}
}

func TestStatisticsQueriesMeetLatencyTargetOnOneYearDataset(t *testing.T) {
	pool, ctx, store := openStatsIntegrationStore(t)
	nodeIDs, start, end, totalSamples := seedStatisticsDataset(t, pool, ctx)

	query := Query{NodeIDs: nodeIDs, NodesSet: true, From: start, To: end, PreviousFrom: start.Add(-365 * 24 * time.Hour), PreviousTo: start,
		Metric: Download, Timezone: "UTC", Bucket: "day", Bins: 15}
	verifyStatisticsQueryPlan(t, pool, ctx, nodeIDs, query, totalSamples)

	measure := func(name string, run func() error) {
		t.Helper()
		best, samples, err := bestStatisticsLatency(run, time.Now)
		if err != nil {
			t.Fatalf("%s query failed: %v", name, err)
		}
		t.Logf("PERF %s: best %.2f ms of %d runs on %d results; samples=%v", name, float64(best)/float64(time.Millisecond), len(samples), totalSamples, samples)
		if best >= 300*time.Millisecond {
			t.Fatalf("%s best of %d took %s, target is under 300 ms; samples=%v", name, len(samples), best, samples)
		}
	}
	verifyBandwidthUsageLatency(t, pool, ctx, start, end, totalSamples, measure)
	var summary []SummaryComparison
	measure("summary", func() error { var err error; summary, err = store.Summary(ctx, query); return err })
	if len(summary) != len(nodeIDs) {
		t.Fatalf("summary returned %d nodes, want %d", len(summary), len(nodeIDs))
	}
	trendQuery := query
	trendQuery.From, trendQuery.To = end.Truncate(24*time.Hour).Add(-299*24*time.Hour), end
	var trend []Series
	measure("daily trend (300 buckets)", func() error { var err error; trend, err = store.Trend(ctx, trendQuery); return err })
	if len(trend) != len(nodeIDs) || len(trend[0].Points) > 300 {
		t.Fatalf("trend returned %d series / %d points", len(trend), len(trend[0].Points))
	}
	var hourSeries []HourSeries
	measure("hour-of-day", func() error { var err error; hourSeries, err = store.HourOfDay(ctx, query); return err })
	if len(hourSeries) != len(nodeIDs) || len(hourSeries[0].Hours) != 24 {
		t.Fatalf("hour-of-day returned %d series", len(hourSeries))
	}
	heatQuery := query
	heatQuery.From, heatQuery.To = end.Add(-30*24*time.Hour), end
	var heatmap Heatmap
	measure("heatmap", func() error { var err error; heatmap, err = store.Heatmap(ctx, heatQuery, nodeIDs[0]); return err })
	if len(heatmap.Cells) != 168 {
		t.Fatalf("heatmap returned %d cells", len(heatmap.Cells))
	}
	var distributions []Distribution
	measure("distribution", func() error { var err error; distributions, err = store.Distribution(ctx, query); return err })
	if len(distributions) != len(nodeIDs) || len(distributions[0].Bins) != query.Bins {
		t.Fatalf("distribution returned %d nodes / %d bins", len(distributions), len(distributions[0].Bins))
	}
}

func verifyBandwidthUsageLatency(t *testing.T, pool *pgxpool.Pool, ctx context.Context, start, end time.Time, samples int, measure func(string, func() error)) {
	t.Helper()
	store := &usage.Store{DB: pool, Now: func() time.Time { return end }}
	query := usage.Query{From: start, To: end, Group: "month"}
	var result usage.Response
	measure("bandwidth usage (12 months)", func() error {
		var err error
		result, err = store.Usage(ctx, query)
		return err
	})
	if result.Totals.Total <= 0 || len(result.ByNodeEngine) != 5 || len(result.Series) > 65 {
		t.Fatalf("usage over %d results: bytes=%d node-engine rows=%d month buckets=%d", samples, result.Totals.Total, len(result.ByNodeEngine), len(result.Series))
	}
}

func seedStatisticsDataset(t *testing.T, pool *pgxpool.Pool, ctx context.Context) ([]string, time.Time, time.Time, int) {
	t.Helper()
	const nodeCount = 5
	const samplesPerNode = 365 * 48
	const totalSamples = nodeCount * samplesPerNode
	const ip = "203.0.113.9"
	if _, err := pool.Exec(ctx, `INSERT INTO ip_addresses(ip) VALUES ($1)`, ip); err != nil {
		t.Fatal(err)
	}
	nodeIDs := make([]string, 0, nodeCount)
	for i := 0; i < nodeCount; i++ {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012x", 0xc001+i)
		nodeIDs = append(nodeIDs, id)
		if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES ($1,$2,1000,300)`, id, fmt.Sprintf("Perf-%d", i+1)); err != nil {
			t.Fatal(err)
		}
	}
	end := time.Now().UTC().Truncate(time.Second)
	start := end.Add(-365 * 24 * time.Hour)
	_, err := pool.Exec(ctx, `WITH generated AS (
 SELECT node_no, sample_no,
        CASE WHEN sample_no % 1000 = 0 THEN 'failed' WHEN sample_no % 10 = 0 THEN 'degraded' ELSE 'ok' END AS status,
        ('00000000-0000-4000-8000-' || lpad(to_hex((node_no - 1) * $2 + sample_no), 12, '0'))::uuid AS result_id
 FROM generate_series(1, $1::int) AS node(node_no)
 CROSS JOIN generate_series(1, $2::int) AS sample(sample_no)
)
INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,jitter_ms,loss_percent,server_id,server,ip,threads,error,bytes_down,bytes_up)
SELECT g.result_id, n.id, $3::timestamptz + (g.sample_no - 1) * interval '30 minutes', g.status,
       CASE WHEN g.status='failed' THEN NULL ELSE 100 + g.node_no * 10 + (g.sample_no % 100) END,
       CASE WHEN g.status='failed' THEN NULL ELSE 20 + g.node_no + (g.sample_no % 30) END,
       CASE WHEN g.status='failed' THEN NULL ELSE 10 + (g.sample_no % 20) END,
       CASE WHEN g.status='failed' THEN NULL ELSE (g.sample_no % 5)::float8 END,
       CASE WHEN g.status='failed' THEN NULL ELSE (g.sample_no % 10)::float8 END,
       CASE WHEN g.status='failed' THEN NULL ELSE 1 END,
       CASE WHEN g.status='failed' THEN NULL ELSE '{"id":1,"name":"Fixture","city":"C","sponsor":"P"}'::jsonb END, $4::inet, 4,
       CASE WHEN g.status='failed' THEN 'performance fixture failure' ELSE NULL END,
       CASE WHEN g.status='failed' THEN NULL ELSE 1000000 + g.sample_no END,
       CASE WHEN g.status='failed' THEN NULL ELSE 500000 + g.node_no END
FROM generated g JOIN nodes n ON n.name = 'Perf-' || g.node_no`, nodeCount, samplesPerNode, start, ip)
	if err != nil {
		t.Fatal("could not generate one-year benchmark data")
	}
	var inserted int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM results`).Scan(&inserted); err != nil || inserted != totalSamples {
		t.Fatalf("generated result count = %d err=%v, want %d", inserted, err, totalSamples)
	}
	return nodeIDs, start, end, totalSamples
}

func verifyStatisticsQueryPlan(t *testing.T, pool *pgxpool.Pool, ctx context.Context, nodeIDs []string, query Query, totalSamples int) {
	t.Helper()
	var explainJSON []byte
	if err := pool.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+summarySQL(), nodeIDs, query.From, query.To, query.PreviousFrom, query.PreviousTo).Scan(&explainJSON); err != nil {
		t.Fatal("could not explain the summary query")
	}
	var explained []struct {
		Plan struct {
			NodeType     string       `json:"Node Type"`
			RelationName string       `json:"Relation Name"`
			IndexName    string       `json:"Index Name"`
			Plans        []planDetail `json:"Plans"`
		} `json:"Plan"`
		ExecutionTime float64 `json:"Execution Time"`
	}
	if err := json.Unmarshal(explainJSON, &explained); err != nil || len(explained) != 1 {
		t.Fatalf("invalid EXPLAIN JSON: %v", err)
	}
	var nodesSeen []string
	var visit func(planDetail)
	visit = func(node planDetail) {
		label := node.NodeType
		if node.RelationName != "" {
			label += " on " + node.RelationName
		}
		if node.IndexName != "" {
			label += " using " + node.IndexName
		}
		nodesSeen = append(nodesSeen, label)
		for _, child := range node.Plans {
			visit(child)
		}
	}
	visit(planDetail{NodeType: explained[0].Plan.NodeType, RelationName: explained[0].Plan.RelationName, IndexName: explained[0].Plan.IndexName, Plans: explained[0].Plan.Plans})
	if !containsRelation(nodesSeen, "results") {
		t.Fatalf("EXPLAIN did not scan results: %v", nodesSeen)
	}
	t.Logf("EXPLAIN summary on %d results: %.2f ms; %v", totalSamples, explained[0].ExecutionTime, nodesSeen)

}

func TestStatisticsStoreReportsDatabaseFailures(t *testing.T) {
	pool, ctx, store := openStatsIntegrationStore(t)
	const nodeID = "00000000-0000-4000-8000-00000000c001"
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES ($1,'Error fixture',100,20)`, nodeID); err != nil {
		t.Fatal("could not create error-path fixture node")
	}
	query := Query{From: time.Now().UTC().Add(-time.Hour), To: time.Now().UTC(),
		PreviousFrom: time.Now().UTC().Add(-2 * time.Hour), PreviousTo: time.Now().UTC().Add(-time.Hour),
		Metric: Download, Bucket: "hour", Timezone: "UTC", Bins: 3}
	type runFunc func() error
	methods := map[string]runFunc{
		"summary":      func() error { _, err := store.Summary(ctx, query); return err },
		"trend":        func() error { _, err := store.Trend(ctx, query); return err },
		"hour-of-day":  func() error { _, err := store.HourOfDay(ctx, query); return err },
		"heatmap":      func() error { _, err := store.Heatmap(ctx, query, nodeID); return err },
		"distribution": func() error { _, err := store.Distribution(ctx, query); return err },
	}
	tests := []struct {
		name      string
		method    string
		queryFail int
		scanFail  int
		rowsErr   int
	}{
		{name: "summary query", method: "summary", queryFail: 1},
		{name: "summary scan", method: "summary", scanFail: 1},
		{name: "summary rows", method: "summary", rowsErr: 1},
		{name: "trend query", method: "trend", queryFail: 1},
		{name: "trend scan", method: "trend", scanFail: 1},
		{name: "trend rows", method: "trend", rowsErr: 1},
		{name: "trend summary query", method: "trend", queryFail: 2},
		{name: "trend summary scan", method: "trend", scanFail: 2},
		{name: "trend summary rows", method: "trend", rowsErr: 2},
		{name: "hour-of-day query", method: "hour-of-day", queryFail: 1},
		{name: "hour-of-day scan", method: "hour-of-day", scanFail: 1},
		{name: "hour-of-day rows", method: "hour-of-day", rowsErr: 1},
		{name: "heatmap query", method: "heatmap", queryFail: 1},
		{name: "heatmap scan", method: "heatmap", scanFail: 1},
		{name: "heatmap rows", method: "heatmap", rowsErr: 1},
		{name: "distribution query", method: "distribution", queryFail: 1},
		{name: "distribution scan", method: "distribution", scanFail: 1},
		{name: "distribution rows", method: "distribution", rowsErr: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store.DB = &failingStatsDB{DB: pool, queryFail: tc.queryFail, scanFail: tc.scanFail, rowsErr: tc.rowsErr}
			if err := methods[tc.method](); err == nil {
				t.Fatal("database failure was not returned")
			}
		})
	}
}

type failingStatsDB struct {
	DB
	calls     int
	queryFail int
	scanFail  int
	rowsErr   int
}

func (db *failingStatsDB) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	db.calls++
	if db.calls == db.queryFail {
		return nil, errors.New("injected statistics query failure")
	}
	rows, err := db.DB.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return failingStatsRows{Rows: rows, scanFail: db.calls == db.scanFail, rowsErr: db.calls == db.rowsErr}, nil
}

type failingStatsRows struct {
	pgx.Rows
	scanFail bool
	rowsErr  bool
}

func (rows failingStatsRows) Scan(dest ...any) error {
	if rows.scanFail {
		return errors.New("injected statistics scan failure")
	}
	return rows.Rows.Scan(dest...)
}

func (rows failingStatsRows) Err() error {
	if rows.rowsErr {
		return errors.New("injected statistics rows failure")
	}
	return rows.Rows.Err()
}

type planDetail struct {
	NodeType     string       `json:"Node Type"`
	RelationName string       `json:"Relation Name"`
	IndexName    string       `json:"Index Name"`
	Plans        []planDetail `json:"Plans"`
}

func containsRelation(nodes []string, relation string) bool {
	for _, node := range nodes {
		if strings.HasPrefix(node, "Seq Scan on "+relation) || strings.HasPrefix(node, "Index Scan on "+relation) || strings.HasPrefix(node, "Bitmap Heap Scan on "+relation) {
			return true
		}
	}
	return false
}
