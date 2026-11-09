//go:build integration

package report_test

import (
	"context"
	"fmt"
	"path/filepath"
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

func TestMonthlyReportAndMailerIntegration(t *testing.T) {
	pool, ctx := openPool(t)
	node := "00000000-0000-4000-8000-0000000000c1"
	must := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	must(`INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES ($1,'report-node',300,100)`, node)
	must(`INSERT INTO ip_addresses(ip) VALUES ('203.0.113.90'),('203.0.113.91')`)
	e, err := isp.Ensure(ctx, pool, 7552, "Viettel Group")
	if err != nil || e == nil {
		t.Fatalf("ensure: %v", err)
	}
	store := &isp.Store{DB: pool}
	plan, err := store.CreatePlan(ctx, e.ID, "Fast 300", 300, 100)
	if err != nil {
		t.Fatal(err)
	}
	must(`UPDATE nodes SET isp_plan_id = $2::uuid WHERE id = $1`, node, plan["id"])
	n := 0
	result := func(ts, status string, down any) {
		n++
		must(`INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,ip,threads,server,engine,isp_id,error)
			VALUES ($1,$2,$3::timestamptz,$4,$5,50,10,1,'203.0.113.90',4,'{"id":1,"name":"x","city":"y","host":"h"}'::jsonb,'ookla',$6,$7)`,
			fmt.Sprintf("00000000-0000-4000-8000-%012d", n), node, ts, status, down, e.ID, map[string]any{"failed": "timeout"}[status])
	}
	result("2026-10-05T03:10:00Z", "ok", 100.0)
	result("2026-10-05T14:10:00Z", "ok", 300.0)
	result("2026-10-06T14:10:00Z", "ok", 250.0)
	result("2026-10-07T09:00:00Z", "failed", nil)
	result("2026-10-07T10:00:00Z", "ok", 280.0)
	result("2026-09-30T23:59:00Z", "ok", 1.0) // outside the month
	must(`INSERT INTO node_ip_history(id,node_id,ts,from_ip,to_ip) VALUES (gen_random_uuid(),$1,'2026-10-10T00:00:00Z','203.0.113.90','203.0.113.91')`, node)

	now := time.Date(2026, 11, 1, 2, 0, 0, 0, time.UTC)
	reports := &report.Store{DB: pool, Now: func() time.Time { return now }}
	got, err := reports.Build(ctx, "2026-10", "", "")
	if err != nil || len(got.Rows) != 1 {
		t.Fatalf("build: %v %#v", err, got)
	}
	row := got.Rows[0]
	if row.Tests != 5 || row.Failures != 1 || row.DowntimeMin != 60 || row.IPChanges != 1 || row.Plan == nil || *row.Plan != "Fast 300" {
		t.Fatalf("row %#v", row)
	}
	if row.AvgDown == nil || int(*row.AvgDown) != 232 || row.DeliveredPct == nil || int(*row.DeliveredPct) != 77 {
		t.Fatalf("speeds %v %v", row.AvgDown, row.DeliveredPct)
	}
	if len(row.WorstHours) != 3 || row.WorstHours[0] != 3 {
		t.Fatalf("worst hours %v", row.WorstHours)
	}
	if one, err := reports.Build(ctx, "2026-10", "librespeed", ""); err != nil || len(one.Rows) != 0 {
		t.Fatalf("engines must not mix: %v %#v", err, one)
	}
	if one, err := reports.Build(ctx, "2026-10", "", "00000000-0000-4000-8000-000000000999"); err != nil || len(one.Rows) != 0 {
		t.Fatalf("other ISP: %v %#v", err, one)
	}

	var bodies []string
	mailer := &report.Mailer{Reports: reports, DB: pool, Now: func() time.Time { return now }, Send: func(_ context.Context, build func(string) string) error {
		bodies = append(bodies, build(""))
		return nil
	}}
	for i := 0; i < 2; i++ {
		if err := mailer.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var sent int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM monthly_report_runs WHERE month = '2026-10' AND sent_at IS NOT NULL`).Scan(&sent); err != nil || sent != 1 || len(bodies) != 1 {
		t.Fatalf("sent once: %d %d %v", sent, len(bodies), err)
	}
}
