//go:build integration

package report_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jooservices/speedtest/internal/center/report"
)

func TestAC1AC2PeriodSummaryUsesDSTHalfOpenPrimaryEngineBoundsIntegration(t *testing.T) {
	pool, ctx := openPool(t)
	primaryOokla := "00000000-0000-4000-8000-000000000123"
	primaryLibre := "00000000-0000-4000-8000-000000000124"
	reportNode(t, ctx, pool, primaryOokla, "alpha")
	reportNode(t, ctx, pool, primaryLibre, "beta")
	mustExec(t, ctx, pool, `UPDATE nodes SET primary_engine='librespeed' WHERE id=$1::uuid`, primaryLibre)
	mustExec(t, ctx, pool, `INSERT INTO ip_addresses(ip) VALUES ('203.0.113.123'::inet) ON CONFLICT DO NOTHING`)

	period, err := report.NewPeriod(report.DailyPeriod, "2026-03-08", "America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	if period.To().Sub(period.From()) != 23*time.Hour {
		t.Fatalf("DST period duration = %s, want 23h", period.To().Sub(period.From()))
	}
	periodResult(t, ctx, pool, 10001, primaryOokla, "2026-03-08T05:00:00Z", "ok", "ookla", 100.0)
	periodResult(t, ctx, pool, 10002, primaryOokla, "2026-03-08T05:15:00Z", "failed", "ookla", nil)
	periodResult(t, ctx, pool, 10003, primaryOokla, "2026-03-09T03:59:00Z", "degraded", "ookla", 300.0)
	periodResult(t, ctx, pool, 10004, primaryOokla, "2026-03-09T04:00:00Z", "ok", "ookla", 900.0)
	periodResult(t, ctx, pool, 10005, primaryOokla, "2026-03-08T06:00:00Z", "ok", "librespeed", 1000.0)
	periodResult(t, ctx, pool, 10006, primaryLibre, "2026-03-08T06:00:00Z", "failed", "librespeed", nil)
	periodResult(t, ctx, pool, 10007, primaryLibre, "2026-03-08T07:00:00Z", "ok", "ookla", 2000.0)

	got, err := (&report.Store{DB: pool}).Summarize(ctx, period)
	if err != nil || got.Nodes == nil || len(got.Nodes) != 2 {
		t.Fatalf("period summary: %v, nodes=%+v", err, got.Nodes)
	}
	alpha, beta := got.Nodes[0], got.Nodes[1]
	if alpha.NodeID != primaryOokla || alpha.Engine != "ookla" || alpha.Successes != 2 || alpha.Failures != 1 ||
		alpha.AvgDown == nil || *alpha.AvgDown != 200 || alpha.AvgUp == nil || *alpha.AvgUp != 50 {
		t.Fatalf("primary Ookla summary includes wrong results or boundary: %+v", alpha)
	}
	if beta.NodeID != primaryLibre || beta.Engine != "librespeed" || beta.Successes != 0 || beta.Failures != 1 ||
		beta.AvgDown != nil || beta.AvgUp != nil {
		t.Fatalf("primary LibreSpeed summary includes another engine: %+v", beta)
	}

	emptyPeriod, err := report.NewPeriod(report.DailyPeriod, "2026-03-10", "America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	empty, err := (&report.Store{DB: pool}).Summarize(ctx, emptyPeriod)
	if err != nil || empty.Nodes == nil || len(empty.Nodes) != 0 {
		t.Fatalf("empty period summary = %+v, %v", empty, err)
	}
}

func TestAC1PeriodSummaryUsesExactLocalDatesAcrossMidnightRollbackIntegration(t *testing.T) {
	pool, ctx := openPool(t)
	node := "00000000-0000-4000-8000-000000000125"
	reportNode(t, ctx, pool, node, "rollback-node")
	mustExec(t, ctx, pool, `INSERT INTO ip_addresses(ip) VALUES ('203.0.113.123'::inet) ON CONFLICT DO NOTHING`)
	for _, fixture := range []struct {
		id   int
		at   string
		down float64
	}{
		{id: 11001, at: "2010-11-07T02:29:59Z", down: 100},
		{id: 11002, at: "2010-11-07T02:30:30Z", down: 200},
		{id: 11003, at: "2010-11-07T02:31:00Z", down: 300},
		{id: 11004, at: "2010-11-07T03:00:00Z", down: 400},
		{id: 11005, at: "2010-11-07T03:29:59Z", down: 500},
		{id: 11006, at: "2010-11-07T03:30:00Z", down: 600},
		{id: 11007, at: "2010-11-08T03:30:00Z", down: 700},
	} {
		periodResult(t, ctx, pool, fixture.id, node, fixture.at, "ok", "ookla", fixture.down)
	}
	for _, tc := range []struct {
		date    string
		success int
		avgDown float64
	}{
		{date: "2010-11-06", success: 4, avgDown: 325},
		{date: "2010-11-07", success: 2, avgDown: 400},
	} {
		period, err := report.NewPeriod(report.DailyPeriod, tc.date, "America/St_Johns")
		if err != nil {
			t.Fatal(err)
		}
		got, err := (&report.Store{DB: pool}).Summarize(ctx, period)
		if err != nil || len(got.Nodes) != 1 {
			t.Fatalf("summary for %s: %v nodes=%+v", tc.date, err, got.Nodes)
		}
		nodeSummary := got.Nodes[0]
		if nodeSummary.Successes != tc.success || nodeSummary.Failures != 0 || nodeSummary.AvgDown == nil || *nodeSummary.AvgDown != tc.avgDown {
			t.Errorf("summary for exact local date %s: %+v", tc.date, nodeSummary)
		}
	}
	week, err := report.NewPeriod(report.WeeklyPeriod, "2010-11-01", "America/St_Johns")
	if err != nil {
		t.Fatal(err)
	}
	weekly, err := (&report.Store{DB: pool}).Summarize(ctx, week)
	if err != nil || len(weekly.Nodes) != 1 || weekly.Nodes[0].Successes != 6 || weekly.Nodes[0].AvgDown == nil || *weekly.Nodes[0].AvgDown != 350 {
		t.Fatalf("summary for weekly period containing rollback: %v nodes=%+v", err, weekly.Nodes)
	}
}

func TestAC1PrimaryEngineSummaryUsesTimestampIndexForLargeFixtureIntegration(t *testing.T) {
	pool, ctx := openPool(t)
	node := "00000000-0000-4000-8000-000000000126"
	reportNode(t, ctx, pool, node, "plan-node")
	mustExec(t, ctx, pool, `INSERT INTO ip_addresses(ip) VALUES ('203.0.113.124'::inet) ON CONFLICT DO NOTHING`)
	insertSummaryPlanFixture(t, ctx, pool, node)
	mustExec(t, ctx, pool, `ANALYZE results`)
	period, err := report.NewPeriod(report.DailyPeriod, "2010-11-06", "America/St_Johns")
	if err != nil {
		t.Fatal(err)
	}
	captured := &summaryPlanPool{pool: pool}
	got, err := (&report.Store{DB: captured}).Summarize(ctx, period)
	if err != nil {
		t.Fatalf("Summarize() error = %v", err)
	}
	if len(got.Nodes) != 1 || got.Nodes[0].Successes != 25 {
		t.Fatalf("summary did not select the 25 exact local-date results: %+v", got.Nodes)
	}
	indexDefinition := resultTimestampIndex(t, ctx, pool)
	if !strings.Contains(indexDefinition, "(ts)") {
		t.Fatalf("results timestamp index has an unexpected definition: %s", indexDefinition)
	}
	plan := explainSummaryQuery(t, ctx, pool, captured)
	assertBoundedTimestampIndexPlan(t, plan)
}

const summaryPlanFixtureRows = 100000

func insertSummaryPlanFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, node string) {
	t.Helper()
	_, err := pool.Exec(ctx, `INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,ip,threads,server,engine)
		SELECT md5('joost-123-summary-plan-' || sequence::text)::uuid,$1::uuid,
			TIMESTAMPTZ '2010-01-01 00:00:00+00' + sequence * INTERVAL '1 hour',
			'ok',100,50,10,1,'203.0.113.124'::inet,4,
			'{"id":1,"name":"plan fixture","city":"test"}'::jsonb,'ookla'
		FROM generate_series(0,$2::integer - 1) AS fixture(sequence)`, node, summaryPlanFixtureRows)
	if err != nil {
		t.Fatal("large summary query-plan fixture insert failed")
	}
}

func resultTimestampIndex(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var definition string
	if err := pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE tablename='results' AND indexname='results_ts_idx'`).Scan(&definition); err != nil {
		t.Fatal("results timestamp index is missing")
	}
	return definition
}

func explainSummaryQuery(t *testing.T, ctx context.Context, pool *pgxpool.Pool, captured *summaryPlanPool) summaryExplainNode {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(ctx, `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) `+captured.query, captured.args...).Scan(&raw); err != nil {
		t.Fatal("summary query plan failed")
	}
	var document []summaryExplainDocument
	if err := json.Unmarshal(raw, &document); err != nil || len(document) != 1 {
		t.Fatal("summary query plan was not valid JSON")
	}
	return document[0].Plan
}

func assertBoundedTimestampIndexPlan(t *testing.T, plan summaryExplainNode) {
	t.Helper()
	stats := summaryPlanStats{}
	inspectSummaryPlan(plan, &stats)
	if !stats.timestampIndexRead {
		t.Error("summary plan did not read results through results_ts_idx")
	}
	if stats.resultsSequentialScan {
		t.Error("summary plan sequentially scanned results")
	}
	if stats.resultRowsRead > summaryPlanReadLimit {
		t.Errorf("summary plan read %.0f result rows, want at most %d", stats.resultRowsRead, summaryPlanReadLimit)
	}
}

func inspectSummaryPlan(node summaryExplainNode, stats *summaryPlanStats) {
	if node.IndexName == "results_ts_idx" && node.ActualLoops > 0 &&
		strings.Contains(node.IndexCond, "ts >=") && strings.Contains(node.IndexCond, "ts <") {
		stats.timestampIndexRead = true
	}
	if node.RelationName == "results" {
		if node.NodeType == "Seq Scan" {
			stats.resultsSequentialScan = true
		}
		stats.resultRowsRead += (node.ActualRows + node.RowsRemovedByFilter + node.RowsRemovedByIndexRecheck) * node.ActualLoops
	}
	for _, child := range node.Plans {
		inspectSummaryPlan(child, stats)
	}
}

const summaryPlanReadLimit = 1000

func TestSummaryPlanGuardCountsFilteredRowsAndRequiresBothTimestampBounds(t *testing.T) {
	wide := summaryExplainNode{
		NodeType: "Index Scan", RelationName: "results", IndexName: "results_ts_idx",
		IndexCond:  "(ts >= lower_bound) AND (ts < upper_bound)",
		ActualRows: 25, ActualLoops: 1, RowsRemovedByFilter: 99975,
	}
	stats := summaryPlanStats{}
	inspectSummaryPlan(wide, &stats)
	if !stats.timestampIndexRead || stats.resultRowsRead != 100000 {
		t.Fatalf("wide scan stats = %+v, want bounded index recognized and 100000 rows read", stats)
	}
	wide.IndexCond = "(ts >= lower_bound)"
	stats = summaryPlanStats{}
	inspectSummaryPlan(wide, &stats)
	if stats.timestampIndexRead {
		t.Fatal("timestamp index missing an upper bound was accepted")
	}
}

type summaryPlanPool struct {
	pool  *pgxpool.Pool
	query string
	args  []any
}

func (p *summaryPlanPool) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	p.query = query
	p.args = append([]any(nil), args...)
	return p.pool.Query(ctx, query, args...)
}

type summaryExplainDocument struct {
	Plan summaryExplainNode `json:"Plan"`
}

type summaryExplainNode struct {
	NodeType                  string               `json:"Node Type"`
	RelationName              string               `json:"Relation Name"`
	IndexName                 string               `json:"Index Name"`
	IndexCond                 string               `json:"Index Cond"`
	ActualRows                float64              `json:"Actual Rows"`
	ActualLoops               float64              `json:"Actual Loops"`
	RowsRemovedByFilter       float64              `json:"Rows Removed by Filter"`
	RowsRemovedByIndexRecheck float64              `json:"Rows Removed by Index Recheck"`
	Plans                     []summaryExplainNode `json:"Plans"`
}

type summaryPlanStats struct {
	timestampIndexRead    bool
	resultsSequentialScan bool
	resultRowsRead        float64
}

func periodResult(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int, node, at, status, engine string, down any) {
	t.Helper()
	resultID := fmt.Sprintf("00000000-0000-4000-8000-%012x", id)
	mustExec(t, ctx, pool, `INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,ip,threads,error,server,engine)
		VALUES ($1::uuid,$2::uuid,$3::timestamptz,$4,$5,CASE WHEN $4='failed' THEN NULL ELSE 50 END,10,1,
		'203.0.113.123'::inet,4,CASE WHEN $4='failed' THEN 'timeout' END,
		CASE WHEN $4='failed' THEN NULL ELSE '{"id":1,"name":"fixture","city":"test"}'::jsonb END,$6)`,
		resultID, node, at, status, down, engine)
}
