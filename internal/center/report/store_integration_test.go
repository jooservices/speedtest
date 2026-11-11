//go:build integration

package report_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/isp"
	"github.com/jooservices/speedtest/internal/center/report"
	"github.com/jooservices/speedtest/internal/shared/config"
)

func openPool(t *testing.T) (*pgxpool.Pool, context.Context) {
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
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal("cannot connect to configured worktree database")
	}
	t.Cleanup(pool.Close)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	return pool, ctx
}

func mustExec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, query, args...); err != nil {
		t.Fatal("report fixture failed")
	}
}

func reportNode(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id, name string) {
	t.Helper()
	mustExec(t, ctx, pool, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES ($1,$2,300,100)`, id, name)
}

func reportIP(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ip, ispID string) {
	t.Helper()
	mustExec(t, ctx, pool, `INSERT INTO ip_addresses(ip,isp_id) VALUES ($1::inet,$2::uuid)`, ip, ispID)
}

func reportHistory(t *testing.T, ctx context.Context, pool *pgxpool.Pool, node, at string, fromIP, toIP, fromISP, toISP any) {
	t.Helper()
	mustExec(t, ctx, pool, `INSERT INTO node_ip_history(id,node_id,ts,from_ip,to_ip,from_isp_id,to_isp_id)
		VALUES (gen_random_uuid(),$1,$2::timestamptz,$3::inet,$4::inet,$5::uuid,$6::uuid)`, node, at, fromIP, toIP, fromISP, toISP)
}

func reportResult(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int, node, at, status string, ip any, ipText string, down any) {
	t.Helper()
	var up any
	if status != "failed" {
		up = 50.0
	}
	resultID := fmt.Sprintf("00000000-0000-4000-8000-%012x", id)
	mustExec(t, ctx, pool, `INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,ip,threads,server,engine,error)
		VALUES ($1::uuid,$2::uuid,$3::timestamptz,$4,$5,$6,10,1,$7::inet,4,
		CASE WHEN $4='failed' THEN NULL ELSE '{"id":1,"name":"fixture","city":"test"}'::jsonb END,
		'ookla',CASE WHEN $4='failed' THEN 'timeout' END)`, resultID, node, at, status, down, up, ip)
	if err := isp.LinkResult(ctx, pool, resultID, node, ipText); err != nil {
		t.Fatal("result ISP link failed")
	}
}

func reportFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (string, string, string, string, string) {
	t.Helper()
	a, err := isp.Ensure(ctx, pool, 7552, "Viettel Network")
	if err != nil {
		t.Fatal("ISP fixture failed")
	}
	b, err := isp.Ensure(ctx, pool, 18403, "FPT Telecom")
	if err != nil {
		t.Fatal("ISP fixture failed")
	}
	node, outage, unknown := "00000000-0000-4000-8000-0000000000c1", "00000000-0000-4000-8000-0000000000c2", "00000000-0000-4000-8000-0000000000c3"
	for _, item := range []struct{ id, name string }{{node, "moving-node"}, {outage, "outage-node"}, {unknown, "unknown-node"}} {
		reportNode(t, ctx, pool, item.id, item.name)
	}
	plan, err := (&isp.Store{DB: pool}).CreatePlan(ctx, b.ID, "FPT 300", 300, 100)
	if err != nil {
		t.Fatal("plan fixture failed")
	}
	mustExec(t, ctx, pool, `UPDATE nodes SET isp_plan_id=$2::uuid WHERE id=$1::uuid`, node, plan["id"])
	reportIP(t, ctx, pool, "203.0.113.90", a.ID)
	reportIP(t, ctx, pool, "203.0.113.91", b.ID)
	reportHistory(t, ctx, pool, node, "2026-09-20T00:00:00Z", nil, "203.0.113.90", nil, a.ID)
	reportHistory(t, ctx, pool, node, "2026-10-10T00:00:00Z", "203.0.113.90", "203.0.113.91", a.ID, b.ID)
	reportHistory(t, ctx, pool, outage, "2026-09-20T00:00:00Z", nil, "203.0.113.90", nil, a.ID)
	reportResult(t, ctx, pool, 1001, node, "2026-10-05T03:10:00Z", "ok", "203.0.113.90", "203.0.113.90", 100.0)
	reportResult(t, ctx, pool, 1002, node, "2026-10-07T09:00:00Z", "failed", nil, "", nil)
	reportResult(t, ctx, pool, 1003, node, "2026-10-07T10:00:00Z", "ok", "203.0.113.90", "203.0.113.90", 200.0)
	reportResult(t, ctx, pool, 1004, node, "2026-10-10T14:10:00Z", "ok", "203.0.113.91", "203.0.113.91", 300.0)
	reportResult(t, ctx, pool, 1005, node, "2026-10-11T09:00:00Z", "failed", nil, "", nil)
	reportResult(t, ctx, pool, 1006, node, "2026-10-11T10:00:00Z", "ok", "203.0.113.91", "203.0.113.91", 400.0)
	reportResult(t, ctx, pool, 2001, outage, "2026-10-20T10:00:00Z", "failed", nil, "", nil)
	reportResult(t, ctx, pool, 3001, unknown, "2026-10-21T10:00:00Z", "failed", nil, "", nil)
	return node, outage, unknown, a.ID, b.ID
}

func rowFor(t *testing.T, got report.Report, ispID *string, node string) report.Row {
	t.Helper()
	for _, row := range got.Rows {
		if stringPtrEqual(row.ISPID, ispID) && row.NodeID == node {
			return row
		}
	}
	t.Fatalf("row not found for ISP %v node %s", ispID, node)
	return report.Row{}
}

func stringPtrEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func TestMonthlyReportMetricsIntegration(t *testing.T) {
	pool, ctx := openPool(t)
	node, outage, unknown, aID, bID := reportFixture(t, ctx, pool)
	got, err := (&report.Store{DB: pool, Now: func() time.Time { return time.Date(2026, 11, 1, 2, 0, 0, 0, time.UTC) }}).Build(ctx, "2026-10", "", "")
	if err != nil || len(got.Rows) != 4 {
		t.Fatalf("build: %v rows=%d", err, len(got.Rows))
	}
	a, b := &aID, &bID
	rowA, rowB := rowFor(t, got, a, node), rowFor(t, got, b, node)
	if rowA.Tests != 3 || rowA.Failures != 1 || rowA.DowntimeMin != 60 || rowA.IPChanges != 0 || rowA.Plan != nil || rowA.AvgDown == nil || *rowA.AvgDown != 150 || len(rowA.WorstHours) != 2 || rowA.WorstHours[0] != 3 {
		t.Fatalf("ISP A metrics: %+v", rowA)
	}
	if rowB.Tests != 3 || rowB.Failures != 1 || rowB.DowntimeMin != 60 || rowB.IPChanges != 1 || rowB.Plan == nil || *rowB.Plan != "FPT 300" || rowB.AvgDown == nil || *rowB.AvgDown != 350 || rowB.DeliveredPct == nil || *rowB.DeliveredPct < 116 || len(rowB.WorstHours) != 2 || rowB.WorstHours[0] != 14 {
		t.Fatalf("ISP B metrics: %+v", rowB)
	}
	outageRow, unknownRow := rowFor(t, got, a, outage), rowFor(t, got, nil, unknown)
	if outageRow.Tests != 1 || outageRow.Failures != 1 || outageRow.AvgDown != nil || outageRow.DowntimeMin != 1440 {
		t.Fatalf("outage-only row: %+v", outageRow)
	}
	if unknownRow.ISP != "Unknown ISP" || unknownRow.ASN != 0 || unknownRow.Tests != 1 || unknownRow.Failures != 1 {
		t.Fatalf("unknown attribution: %+v", unknownRow)
	}
	if filtered, err := (&report.Store{DB: pool, Now: func() time.Time { return got.Generated }}).Build(ctx, "2026-10", "", aID); err != nil || len(filtered.Rows) != 2 || filtered.Rows[0].IPChanges != 0 {
		t.Fatalf("ISP-filtered report: %v %+v", err, filtered.Rows)
	}
	var unlinked int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM results WHERE status='failed' AND ip IS NULL AND isp_id IS NULL`).Scan(&unlinked); err != nil || unlinked != 4 {
		t.Fatalf("no-IP ingestion fixture was not preserved: %d %v", unlinked, err)
	}
}

func TestMonthlyReportBoundsCurrentDowntimeIntegration(t *testing.T) {
	pool, ctx := openPool(t)
	e, err := isp.Ensure(ctx, pool, 7552, "Viettel Network")
	if err != nil {
		t.Fatal("ISP fixture failed")
	}
	node := "00000000-0000-4000-8000-0000000000d1"
	reportNode(t, ctx, pool, node, "partial-outage")
	reportIP(t, ctx, pool, "203.0.113.92", e.ID)
	reportHistory(t, ctx, pool, node, "2026-11-01T00:00:00Z", nil, "203.0.113.92", nil, e.ID)
	reportResult(t, ctx, pool, 4001, node, "2026-11-12T10:00:00Z", "failed", nil, "", nil)
	now := time.Date(2026, 11, 12, 10, 5, 0, 0, time.UTC)
	got, err := (&report.Store{DB: pool, Now: func() time.Time { return now }}).Build(ctx, "2026-11", "", "")
	if err != nil || !got.Partial || len(got.Rows) != 1 || got.Rows[0].DowntimeMin != 5 {
		t.Fatalf("ongoing downtime: %v %+v", err, got.Rows)
	}
}

func TestMonthlyReportMailerRecoversLeaseWithoutConcurrentSendIntegration(t *testing.T) {
	pool, ctx := openPool(t)
	mustExec(t, ctx, pool, `INSERT INTO monthly_report_runs(month,lease_id,lease_until,attempt_count,next_attempt_at)
		VALUES ('2026-11',gen_random_uuid(),now()-interval '1 minute',1,now()-interval '1 minute')`)
	now := time.Date(2026, 12, 1, 2, 0, 0, 0, time.UTC)
	var sends atomic.Int32
	entered, release := make(chan struct{}, 1), make(chan struct{})
	mailer := &report.Mailer{Reports: &report.Store{DB: pool, Now: func() time.Time { return now }}, DB: pool, Now: func() time.Time { return now },
		Send: func(context.Context, func(string) string) error {
			sends.Add(1)
			entered <- struct{}{}
			<-release
			return nil
		}}
	first := make(chan error, 1)
	go func() { first <- mailer.RunOnce(ctx) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("recovered report did not start sending")
	}
	if err := mailer.RunOnce(ctx); err != nil || sends.Load() != 1 {
		t.Fatalf("concurrent claim sent more than once: %d %v", sends.Load(), err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal("recovered report send failed")
	}
	var attempts int
	var sent bool
	if err := pool.QueryRow(ctx, `SELECT attempt_count,sent_at IS NOT NULL FROM monthly_report_runs WHERE month='2026-11'`).Scan(&attempts, &sent); err != nil || attempts != 2 || !sent || sends.Load() != 1 {
		t.Fatalf("durable recovery state: attempts=%d sent=%v calls=%d err=%v", attempts, sent, sends.Load(), err)
	}
}
