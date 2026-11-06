//go:build integration

package jobs

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

var now = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// seed inserts one node and n results per day for the given ages in days.
func seed(t *testing.T, ctx context.Context, pool *pgxpool.Pool, perAge int, agesDays ...int) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO nodes (id,name,label,expected_down_mbps,expected_up_mbps,color_slot,host,agent) VALUES ('00000000-0000-4000-8000-000000000a01','n','l',1,1,1,'h','a/1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ip_addresses (ip) VALUES ('203.0.113.5')`); err != nil {
		t.Fatal(err)
	}
	for _, age := range agesDays {
		if _, err := pool.Exec(ctx, `INSERT INTO results (id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,server,ip,threads)
			SELECT gen_random_uuid(),'00000000-0000-4000-8000-000000000a01',$1::timestamptz - (g || ' seconds')::interval,'ok',1,1,1,1,'{"id":1,"name":"s","city":"c","sponsor":"p"}','203.0.113.5',4
			FROM generate_series(1,$2) g`, now.AddDate(0, 0, -age), perAge); err != nil {
			t.Fatal(err)
		}
	}
}

func count(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM results`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func setRetention(t *testing.T, ctx context.Context, pool *pgxpool.Pool, value string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO settings (key,value) VALUES ('retentionDays',$1::jsonb) ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value`, value); err != nil {
		t.Fatal(err)
	}
}

func TestRetentionDeletesOldRowsInSmallBatches(t *testing.T) {
	pool, ctx := openPool(t)
	seed(t, ctx, pool, 25, 1, 40, 100)
	setRetention(t, ctx, pool, "30")
	job := &Retention{DB: pool, Now: func() time.Time { return now }, BatchSize: 10, Pause: time.Millisecond}
	res, err := job.Run(ctx)
	if err != nil || res.Deleted != 50 || res.Batches != 6 || res.RetentionDays != 30 || res.Skipped != "" {
		t.Fatalf("result = %+v err %v (50 old rows in batches of 10 end with a short batch)", res, err)
	}
	if n := count(t, ctx, pool); n != 25 {
		t.Fatalf("%d rows left, want the 25 from yesterday", n)
	}
	if again, err := job.Run(ctx); err != nil || again.Deleted != 0 || again.Batches != 1 {
		t.Fatalf("second run = %+v err %v", again, err)
	}
}

func TestRetentionBoundaryAndSettings(t *testing.T) {
	pool, ctx := openPool(t)
	seed(t, ctx, pool, 3, 364, 366)
	job := &Retention{DB: pool, Now: func() time.Time { return now }, BatchSize: 100}
	res, err := job.Run(ctx)
	if err != nil || res.RetentionDays != DefaultRetentionDays || res.Deleted != 3 || count(t, ctx, pool) != 3 {
		t.Fatalf("default retention: %+v err %v", res, err)
	}
	setRetention(t, ctx, pool, "0")
	if keep, err := job.Run(ctx); err != nil || keep.Deleted != 0 || keep.Skipped == "" || count(t, ctx, pool) != 3 {
		t.Fatalf("0 keeps everything: %+v err %v", keep, err)
	}
	for _, bad := range []string{`-5`, `"thirty"`, `1.5`} {
		setRetention(t, ctx, pool, bad)
		if _, err := job.Run(ctx); err == nil {
			t.Errorf("setting %s accepted", bad)
		}
	}
	if count(t, ctx, pool) != 3 {
		t.Fatal("an invalid setting must delete nothing")
	}
}

func TestRetentionStopsWhenCancelledAndLoopReturns(t *testing.T) {
	pool, ctx := openPool(t)
	seed(t, ctx, pool, 20, 400)
	setRetention(t, ctx, pool, "30")
	cancelled, cancel := context.WithCancel(ctx)
	job := &Retention{DB: pool, Now: func() time.Time { return now }, BatchSize: 5, Pause: time.Hour}
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	res, err := job.Run(cancelled)
	if err == nil || res.Deleted != 5 || count(t, ctx, pool) != 15 {
		t.Fatalf("cancelled run: %+v err %v rows %d", res, err, count(t, ctx, pool))
	}
	done := make(chan struct{})
	loopCtx, stop := context.WithCancel(ctx)
	go func() { job.Loop(loopCtx); close(done) }()
	stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Loop did not return after cancellation")
	}
}

func TestJobRegistryRecordsAndReadsStatus(t *testing.T) {
	pool, ctx := openPool(t)
	id, err := Start(ctx, pool, "test-job")
	if err != nil {
		t.Fatal(err)
	}
	registry := &Registry{DB: pool}
	job, err := registry.Get(ctx, id)
	if err != nil || job["status"] != StatusRunning || job["error"] != nil || job["id"] != id {
		t.Fatalf("running job = %v %v", job, err)
	}
	if err := Finish(ctx, pool, id, nil); err != nil {
		t.Fatal(err)
	}
	if job, _ = registry.Get(ctx, id); job["status"] != StatusSucceeded {
		t.Fatalf("finished job = %v", job)
	}
	failed, _ := Start(ctx, pool, "test-job")
	if err := Finish(ctx, pool, failed, context.DeadlineExceeded); err != nil {
		t.Fatal(err)
	}
	if job, _ = registry.Get(ctx, failed); job["status"] != StatusFailed || job["error"] == nil || job["error"] == context.DeadlineExceeded.Error() {
		t.Fatalf("failed job must carry a safe message: %v", job)
	}
	if _, err := registry.Get(ctx, "00000000-0000-4000-8000-0000000000ff"); err != ErrNotFound {
		t.Fatalf("unknown job = %v", err)
	}
}

// A result just inside the window is kept and one just outside is deleted: guards against a cutoff
// that is off by a day in either direction.
func TestRetentionCutoffIsExactlyTheConfiguredDays(t *testing.T) {
	pool, ctx := openPool(t)
	seed(t, ctx, pool, 1, 1)
	setRetention(t, ctx, pool, "30")
	for name, age := range map[string]string{"inside": "29 days 23 hours", "outside": "30 days 1 hour"} {
		if _, err := pool.Exec(ctx, `INSERT INTO results (id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,server,ip,threads)
			VALUES (gen_random_uuid(),'00000000-0000-4000-8000-000000000a01',$1::timestamptz - $2::interval,'ok',1,1,1,1,'{"id":1,"name":"s","city":"c","sponsor":"p"}','203.0.113.5',4)`, now, age); err != nil {
			t.Fatal(name, err)
		}
	}
	res, err := (&Retention{DB: pool, Now: func() time.Time { return now }, BatchSize: 100}).Run(ctx)
	if err != nil || res.Deleted != 1 || count(t, ctx, pool) != 2 {
		t.Fatalf("deleted %d, %d left (want only the row older than 30 days gone): err %v", res.Deleted, count(t, ctx, pool), err)
	}
}

// The owner's change log is pruned with the same cut-off as results, and kept with "keep everything".
func TestRetentionAlsoPrunesTheAuditLog(t *testing.T) {
	pool, ctx := openPool(t)
	seed(t, ctx, pool, 1, 1)
	setRetention(t, ctx, pool, "30")
	for name, age := range map[string]string{"inside": "29 days", "outside": "31 days"} {
		if _, err := pool.Exec(ctx, `INSERT INTO audit_log(id,ts,actor,action) VALUES (gen_random_uuid(), $1::timestamptz - $2::interval, 'owner', $3)`, now, age, name); err != nil {
			t.Fatal(err)
		}
	}
	res, err := (&Retention{DB: pool, Now: func() time.Time { return now }, BatchSize: 100}).Run(ctx)
	if err != nil || res.AuditDeleted != 1 {
		t.Fatalf("result %+v err %v", res, err)
	}
	var left []string
	rows, _ := pool.Query(ctx, `SELECT action FROM audit_log`)
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		left = append(left, a)
	}
	if len(left) != 1 || left[0] != "inside" {
		t.Fatalf("audit entries left: %v", left)
	}
	setRetention(t, ctx, pool, "0")
	if res, err := (&Retention{DB: pool, Now: func() time.Time { return now.AddDate(5, 0, 0) }, BatchSize: 100}).Run(ctx); err != nil || res.AuditDeleted != 0 {
		t.Fatalf("keep-everything run: %+v %v", res, err)
	}
}

func TestRetentionObserverIntegration(t *testing.T) {
	pool, ctx := openPool(t)
	var outcomes []bool
	r := &Retention{DB: pool, ObserveRun: func(ok bool) { outcomes = append(outcomes, ok) }}
	if _, err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := r.Run(cancelled); err == nil {
		t.Fatal("cancelled run succeeded")
	}
	if len(outcomes) != 2 || !outcomes[0] || outcomes[1] {
		t.Fatal(outcomes)
	}
}
