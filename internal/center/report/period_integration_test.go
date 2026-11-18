//go:build integration

package report_test

import (
	"context"
	"fmt"
	"testing"
	"time"

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
