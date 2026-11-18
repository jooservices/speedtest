//go:build integration

package report

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/shared/config"
)

func openDigestPool(t *testing.T) (*pgxpool.Pool, context.Context) {
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

func setDigestTimezone(t *testing.T, ctx context.Context, pool *pgxpool.Pool, zone string) {
	t.Helper()
	raw, err := json.Marshal(zone)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO settings (key, value) VALUES ('timezone', $1::jsonb)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, string(raw)); err != nil {
		t.Fatal("cannot set the digest test time zone")
	}
}

func fixedNow(value time.Time) func() time.Time {
	return func() time.Time { return value }
}

func findDigestPeriod(t *testing.T, periods []Period, kind PeriodKind, date string) Period {
	t.Helper()
	for _, period := range periods {
		if period.Kind() == kind && period.LocalDate() == date {
			return period
		}
	}
	t.Fatalf("period %s %s was not queued: %+v", kind, date, periods)
	return Period{}
}

func digestPeriodState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, kind PeriodKind, date string) (int, bool, string) {
	t.Helper()
	var attempts int
	var completed bool
	var zone string
	err := pool.QueryRow(ctx, `SELECT attempt_count, completed_at IS NOT NULL, timezone
		FROM digest_periods WHERE kind = $1 AND local_date = $2::date`, string(kind), date).Scan(&attempts, &completed, &zone)
	if err != nil {
		t.Fatal("cannot read the digest period state")
	}
	return attempts, completed, zone
}

func completedDigestPeriods(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM digest_periods WHERE completed_at IS NOT NULL`).Scan(&count); err != nil {
		t.Fatal("cannot count completed digest periods")
	}
	return count
}

func TestAC1DigestSchedulerClaimsDueDayAndWeekAtOwnerLocalEightIntegration(t *testing.T) {
	pool, ctx := openDigestPool(t)
	setDigestTimezone(t, ctx, pool, "America/New_York")
	now := time.Date(2026, 3, 9, 12, 0, 0, 0, time.UTC) // Monday 08:00 EDT
	var periods []Period
	scheduler := &DigestScheduler{
		DB:  pool,
		Now: fixedNow(now),
		Enqueue: func(_ context.Context, tx pgx.Tx, period Period) error {
			if tx == nil {
				return errors.New("digest enqueue has no transaction")
			}
			periods = append(periods, period)
			return nil
		},
	}
	if err := scheduler.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	daily := findDigestPeriod(t, periods, DailyPeriod, "2026-03-08")
	weekly := findDigestPeriod(t, periods, WeeklyPeriod, "2026-03-02")
	if span := daily.To().Sub(daily.From()); span != 23*time.Hour {
		t.Errorf("daily period span = %s, want 23h", span)
	}
	if span := weekly.To().Sub(weekly.From()); span != 167*time.Hour {
		t.Errorf("weekly period span = %s, want 167h", span)
	}
	if count := completedDigestPeriods(t, ctx, pool); count != 2 {
		t.Fatalf("completed periods = %d, want 2", count)
	}
	if attempts, completed, zone := digestPeriodState(t, ctx, pool, DailyPeriod, "2026-03-08"); attempts != 1 || !completed || zone != "America/New_York" {
		t.Fatalf("daily state attempts=%d completed=%v zone=%q", attempts, completed, zone)
	}
}

func TestAC2ConcurrentDigestSchedulersCompleteEachPeriodOnceIntegration(t *testing.T) {
	pool, ctx := openDigestPool(t)
	setDigestTimezone(t, ctx, pool, "UTC")
	now := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	started, release := make(chan struct{}, 4), make(chan struct{})
	enqueue := func(enqueueCtx context.Context, _ pgx.Tx, _ Period) error {
		calls.Add(1)
		started <- struct{}{}
		select {
		case <-release:
			return nil
		case <-enqueueCtx.Done():
			return enqueueCtx.Err()
		}
	}
	first := &DigestScheduler{DB: pool, Now: fixedNow(now), Enqueue: enqueue}
	second := &DigestScheduler{DB: pool, Now: fixedNow(now), Enqueue: enqueue}
	errs := make(chan error, 2)
	go func() { errs <- first.RunOnce(ctx) }()
	go func() { errs <- second.RunOnce(ctx) }()
	for range 2 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("both concurrent centers did not claim a due period")
		}
	}
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("concurrent enqueues = %d, want one for each due period", calls.Load())
	}
	if err := first.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("a completed period was queued again: %d calls", calls.Load())
	}
	if count := completedDigestPeriods(t, ctx, pool); count != 2 {
		t.Fatalf("completed periods = %d, want 2", count)
	}
}

func TestAC2DigestSchedulerRetriesFailedEnqueueAfterRestartIntegration(t *testing.T) {
	pool, ctx := openDigestPool(t)
	setDigestTimezone(t, ctx, pool, "UTC")
	now := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	failure := errors.New("enqueue failed")
	failed := &DigestScheduler{
		DB:  pool,
		Now: fixedNow(now),
		Enqueue: func(context.Context, pgx.Tx, Period) error {
			return failure
		},
	}
	if err := failed.RunOnce(ctx); !errors.Is(err, failure) {
		t.Fatalf("failed enqueue error = %v, want the enqueue error", err)
	}
	if count := completedDigestPeriods(t, ctx, pool); count != 0 {
		t.Fatalf("failed enqueue completed %d periods", count)
	}
	for _, key := range []digestPeriodKey{{kind: DailyPeriod, localDate: "2026-03-09"}, {kind: WeeklyPeriod, localDate: "2026-03-02"}} {
		if attempts, completed, _ := digestPeriodState(t, ctx, pool, key.kind, key.localDate); attempts != 1 || completed {
			t.Fatalf("%s %s after failure attempts=%d completed=%v", key.kind, key.localDate, attempts, completed)
		}
	}

	retryAt := now.Add(10 * time.Minute)
	var successes atomic.Int32
	restarted := &DigestScheduler{
		DB:  pool,
		Now: fixedNow(retryAt),
		Enqueue: func(context.Context, pgx.Tx, Period) error {
			successes.Add(1)
			return nil
		},
	}
	if err := restarted.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if successes.Load() != 2 || completedDigestPeriods(t, ctx, pool) != 2 {
		t.Fatalf("restart successes=%d completed=%d, want 2", successes.Load(), completedDigestPeriods(t, ctx, pool))
	}
	for _, key := range []digestPeriodKey{{kind: DailyPeriod, localDate: "2026-03-09"}, {kind: WeeklyPeriod, localDate: "2026-03-02"}} {
		if attempts, completed, _ := digestPeriodState(t, ctx, pool, key.kind, key.localDate); attempts != 2 || !completed {
			t.Fatalf("%s %s after retry attempts=%d completed=%v", key.kind, key.localDate, attempts, completed)
		}
	}
	if err := restarted.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if successes.Load() != 2 {
		t.Fatalf("completed periods were queued again after restart: %d", successes.Load())
	}
}

func TestAC2DigestSchedulerRecoversExpiredLeaseAfterRestartIntegration(t *testing.T) {
	pool, ctx := openDigestPool(t)
	setDigestTimezone(t, ctx, pool, "UTC")
	now := time.Date(2026, 3, 12, 9, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `INSERT INTO digest_periods (kind, local_date, timezone, claimed_at, lease_id, lease_until, attempt_count, next_attempt_at)
		VALUES ('daily', '2026-03-09', 'UTC', $1, gen_random_uuid(), $1, 1, $1)`, now.Add(-time.Hour)); err != nil {
		t.Fatal("cannot insert the expired digest lease fixture")
	}
	queued := map[string]bool{}
	scheduler := &DigestScheduler{
		DB:  pool,
		Now: fixedNow(now),
		Enqueue: func(_ context.Context, _ pgx.Tx, period Period) error {
			queued[string(period.Kind())+period.LocalDate()] = true
			return nil
		},
	}
	if err := scheduler.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if !queued[string(DailyPeriod)+"2026-03-09"] {
		t.Fatal("the expired pending period was not recovered")
	}
	if attempts, completed, _ := digestPeriodState(t, ctx, pool, DailyPeriod, "2026-03-09"); attempts != 2 || !completed {
		t.Fatalf("recovered period attempts=%d completed=%v", attempts, completed)
	}
}

func TestAC2DigestSchedulerDoesNotCompleteAfterLeaseLossIntegration(t *testing.T) {
	pool, ctx := openDigestPool(t)
	setDigestTimezone(t, ctx, pool, "UTC")
	now := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `INSERT INTO digest_periods (kind, local_date, timezone, completed_at)
		VALUES ('weekly', '2026-03-02', 'UTC', $1)`, now); err != nil {
		t.Fatal("cannot complete the weekly test fixture")
	}
	scheduler := &DigestScheduler{
		DB:            pool,
		Now:           fixedNow(now),
		leaseDuration: 300 * time.Millisecond,
		Enqueue: func(enqueueCtx context.Context, _ pgx.Tx, period Period) error {
			_, err := pool.Exec(context.WithoutCancel(enqueueCtx), `UPDATE digest_periods SET lease_id = gen_random_uuid(), lease_until = $3
				WHERE kind = $1 AND local_date = $2::date`, string(period.Kind()), period.LocalDate(), now.Add(time.Hour))
			if err != nil {
				return err
			}
			<-enqueueCtx.Done()
			return enqueueCtx.Err()
		},
	}
	if err := scheduler.RunOnce(ctx); !errors.Is(err, errDigestLeaseLost) {
		t.Fatalf("lease-loss error = %v, want errDigestLeaseLost", err)
	}
	if attempts, completed, _ := digestPeriodState(t, ctx, pool, DailyPeriod, "2026-03-09"); attempts != 1 || completed {
		t.Fatalf("lease-loss period attempts=%d completed=%v", attempts, completed)
	}
}

func TestAC2DigestSchedulerKeepsOneCompletedPeriodAfterTimezoneChangeIntegration(t *testing.T) {
	pool, ctx := openDigestPool(t)
	setDigestTimezone(t, ctx, pool, "UTC")
	now := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	var mutex sync.Mutex
	var calls int
	scheduler := &DigestScheduler{
		DB:  pool,
		Now: fixedNow(now),
		Enqueue: func(context.Context, pgx.Tx, Period) error {
			mutex.Lock()
			calls++
			mutex.Unlock()
			return nil
		},
	}
	if err := scheduler.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	setDigestTimezone(t, ctx, pool, "Asia/Ho_Chi_Minh")
	if err := scheduler.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	mutex.Lock()
	enqueues := calls
	mutex.Unlock()
	if enqueues != 2 {
		t.Fatalf("time-zone change queued a completed period again: %d enqueues", enqueues)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM digest_periods WHERE kind = 'daily' AND local_date = '2026-03-09'`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("daily 2026-03-09 rows = %d, err=%v", rows, err)
	}
	if count := completedDigestPeriods(t, ctx, pool); count != 2 {
		t.Fatalf("completed periods = %d, want 2", count)
	}
}
