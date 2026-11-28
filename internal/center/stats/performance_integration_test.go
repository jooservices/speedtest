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
	if err := db.CheckDestructiveAllowed(cfg.AppEnv); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal("cannot connect to configured worktree database")
	}
	t.Cleanup(pool.Close)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	return pool, ctx, &Store{DB: pool}
}

func TestStatisticsQueriesMeetLatencyTargetOnOneYearDataset(t *testing.T) {
	if testing.CoverMode() != "" {
		t.Skip("latency measurements are not representative with coverage instrumentation")
	}
	pool, ctx, store := openStatsIntegrationStore(t)
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
INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,jitter_ms,loss_percent,server_id,server,ip,threads,error,engine,engine_isp,bytes_down,bytes_up)
SELECT g.result_id, n.id, $3::timestamptz + (g.sample_no - 1) * interval '30 minutes', g.status,
       CASE WHEN g.status='failed' THEN NULL ELSE 100 + g.node_no * 10 + (g.sample_no % 100) END,
       CASE WHEN g.status='failed' THEN NULL ELSE 20 + g.node_no + (g.sample_no % 30) END,
       CASE WHEN g.status='failed' THEN NULL ELSE 10 + (g.sample_no % 20) END,
       CASE WHEN g.status='failed' THEN NULL ELSE (g.sample_no % 5)::float8 END,
       CASE WHEN g.status='failed' THEN NULL ELSE (g.sample_no % 10)::float8 END,
       CASE WHEN g.status='failed' THEN NULL ELSE 1 END,
       CASE WHEN g.status='failed' THEN NULL ELSE '{"id":1,"name":"Fixture","city":"C","sponsor":"P"}'::jsonb END, $4::inet, 4,
       CASE WHEN g.status='failed' THEN 'performance fixture failure' ELSE NULL END,
       CASE WHEN g.sample_no % 2=0 THEN 'librespeed' ELSE 'ookla' END,
       CASE WHEN g.sample_no % 4 < 2 THEN 'Carrier A' ELSE 'Carrier B' END,
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
	if _, err := pool.Exec(ctx, `INSERT INTO alert_events(id,node_id,severity,ts,title,detail,trigger_result_id)
SELECT gen_random_uuid(),node_id,'info',ts + interval '1 second','Performance fixture alert','Bounded event query',id
FROM results WHERE engine='ookla' AND engine_isp='Carrier A' ORDER BY ts,id LIMIT 600`); err != nil {
		t.Fatal("could not generate 600 linked benchmark events")
	}

	baseQuery := Query{NodeIDs: nodeIDs, NodesSet: true, From: start, To: end, PreviousFrom: start.Add(-365 * 24 * time.Hour), PreviousTo: start,
		Metric: Download, Timezone: "UTC", Bucket: "day", Bins: 15}
	filteredQuery := baseQuery
	filteredQuery.Engine, filteredQuery.ISP = "ookla", "Carrier A"
	for _, benchmark := range []struct {
		name  string
		query Query
	}{{"primary engine, all ISPs", baseQuery}, {"Ookla, Carrier A", filteredQuery}} {
		t.Run(benchmark.name, func(t *testing.T) {
			logStatisticsPlans(t, pool, ctx, benchmark.query, totalSamples)
			measureStatisticsQueries(t, store, ctx, benchmark.query, nodeIDs[0], nodeCount, totalSamples, end)
		})
	}
	verifyBandwidthUsageLatency(t, pool, ctx, baseQuery.From, end, totalSamples, func(name string, run func() error) {
		t.Helper()
		p50, p95, _, err := statisticsLatency(run, time.Now)
		if err != nil {
			t.Fatalf("%s query failed: %v", name, err)
		}
		if p50 >= 300*time.Millisecond || p95 >= 300*time.Millisecond {
			t.Fatalf("%s p50=%s p95=%s; both must stay under 300 ms", name, p50, p95)
		}
	})
}

func logStatisticsPlans(t *testing.T, pool *pgxpool.Pool, ctx context.Context, q Query, totalSamples int) {
	t.Helper()
	summarySQLText, summaryArgs := scopedQuery(q, summarySQL(), q.NodeIDs, q.From, q.To, q.PreviousFrom, q.PreviousTo)
	logStatisticsPlan(t, pool, ctx, "summary", summarySQLText, summaryArgs, totalSamples)
	logStatisticsPlan(t, pool, ctx, "events", eventsSQL, []any{q.NodeIDs, q.From, q.To, q.Engine, q.ISP}, totalSamples)
}

func logStatisticsPlan(t *testing.T, pool *pgxpool.Pool, ctx context.Context, name, sql string, args []any, totalSamples int) {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+sql, args...).Scan(&raw); err != nil {
		t.Fatalf("could not explain the %s query: %v", name, err)
	}
	var plans []struct {
		ExecutionTime float64    `json:"Execution Time"`
		Plan          planDetail `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("invalid %s EXPLAIN JSON: %v", name, err)
	}
	var scans []string
	appendPlanScans(plans[0].Plan, &scans)
	if name == "summary" && !containsRelation(scans, "results") {
		t.Fatalf("summary EXPLAIN did not scan results: %v", scans)
	}
	t.Logf("EXPLAIN %s (ANALYZE, BUFFERS) on %d results: %.2f ms; hits=%d reads=%d; scans=%v",
		name, totalSamples, plans[0].ExecutionTime, plans[0].Plan.SharedHitBlocks, plans[0].Plan.SharedReadBlocks, scans)
}

func measureStatisticsQueries(t *testing.T, store *Store, ctx context.Context, q Query, heatNode string, nodeCount, totalSamples int, end time.Time) {
	t.Helper()
	measure := func(name string, run func() error) {
		t.Helper()
		p50, p95, samples, err := statisticsLatency(run, time.Now)
		if err != nil {
			t.Fatalf("%s query failed: %v", name, err)
		}
		t.Logf("PERF %s: p50 %.2f ms, p95 %.2f ms of %d runs on %d results; samples=%v",
			name, float64(p50)/float64(time.Millisecond), float64(p95)/float64(time.Millisecond), len(samples), totalSamples, samples)
		if p50 >= 300*time.Millisecond || p95 >= 300*time.Millisecond {
			t.Fatalf("%s p50=%s p95=%s; both must stay under 300 ms", name, p50, p95)
		}
	}
	var summary []SummaryComparison
	measure("summary", func() error { var err error; summary, err = store.Summary(ctx, q); return err })
	if len(summary) != nodeCount {
		t.Fatalf("summary returned %d nodes, want %d", len(summary), nodeCount)
	}
	trendQuery := q
	trendQuery.From, trendQuery.To = end.Truncate(24*time.Hour).Add(-299*24*time.Hour), end
	var trend []Series
	measure("daily trend (300 buckets)", func() error { var err error; trend, err = store.Trend(ctx, trendQuery); return err })
	if len(trend) != nodeCount || len(trend[0].Points) > 300 {
		t.Fatalf("trend returned %d series / %d points", len(trend), len(trend[0].Points))
	}
	var hours []HourSeries
	measure("hour-of-day", func() error { var err error; hours, err = store.HourOfDay(ctx, q); return err })
	if len(hours) != nodeCount || len(hours[0].Hours) != 24 {
		t.Fatalf("hour-of-day returned %d series", len(hours))
	}
	heatQuery := q
	heatQuery.From, heatQuery.To = end.Add(-30*24*time.Hour), end
	var heatmap Heatmap
	measure("heatmap", func() error { var err error; heatmap, err = store.Heatmap(ctx, heatQuery, heatNode); return err })
	if len(heatmap.Cells) != 168 {
		t.Fatalf("heatmap returned %d cells", len(heatmap.Cells))
	}
	var distributions []Distribution
	measure("distribution", func() error { var err error; distributions, err = store.Distribution(ctx, q); return err })
	if len(distributions) != nodeCount || len(distributions[0].Bins) != q.Bins {
		t.Fatalf("distribution returned %d nodes / %d bins", len(distributions), len(distributions[0].Bins))
	}
	var events EventsResponse
	measure("events (600 linked alerts)", func() error { var err error; events, err = store.Events(ctx, q); return err })
	if len(events.Items) != MaxEvents || !events.Truncated {
		t.Fatalf("events returned %d items, truncated=%t; want %d and true", len(events.Items), events.Truncated, MaxEvents)
	}
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
		"events":       func() error { _, err := store.Events(ctx, query); return err },
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
		{name: "events query", method: "events", queryFail: 1},
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
	NodeType         string       `json:"Node Type"`
	RelationName     string       `json:"Relation Name"`
	IndexName        string       `json:"Index Name"`
	ActualTime       float64      `json:"Actual Total Time"`
	ActualRows       float64      `json:"Actual Rows"`
	ActualLoops      float64      `json:"Actual Loops"`
	SharedHitBlocks  int          `json:"Shared Hit Blocks"`
	SharedReadBlocks int          `json:"Shared Read Blocks"`
	Plans            []planDetail `json:"Plans"`
}

func appendPlanScans(plan planDetail, scans *[]string) {
	label := plan.NodeType
	if plan.RelationName != "" {
		label += " on " + plan.RelationName
	}
	if plan.IndexName != "" {
		label += " via " + plan.IndexName
	}
	*scans = append(*scans, fmt.Sprintf("%s (%.2f ms rows=%.0f loops=%.0f hits=%d reads=%d)",
		label, plan.ActualTime, plan.ActualRows, plan.ActualLoops, plan.SharedHitBlocks, plan.SharedReadBlocks))
	for _, child := range plan.Plans {
		appendPlanScans(child, scans)
	}
}

func containsRelation(nodes []string, relation string) bool {
	for _, node := range nodes {
		if strings.HasPrefix(node, "Seq Scan on "+relation) || strings.HasPrefix(node, "Index Scan on "+relation) || strings.HasPrefix(node, "Bitmap Heap Scan on "+relation) {
			return true
		}
	}
	return false
}

// The bandwidth usage query over a year of results stays under the same 300 ms target as the statistics queries.
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
	if result.Totals.Total <= 0 || len(result.ByNodeEngine) < 5 || len(result.Series) > 13*len(result.ByNodeEngine) {
		t.Fatalf("usage over %d results: bytes=%d node-engine rows=%d month buckets=%d", samples, result.Totals.Total, len(result.ByNodeEngine), len(result.Series))
	}
}
