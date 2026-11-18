//go:build integration

package report

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/settings"
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
	raw, err := json.Marshal(map[string]string{"timezone": zone})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO settings (key, value) VALUES ('display', $1::jsonb)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, string(raw)); err != nil {
		t.Fatal("cannot set the digest test time zone")
	}
}

func fixedNow(value time.Time) func() time.Time {
	return func() time.Time { return value }
}

func movingDigestNow(start time.Time) func() time.Time {
	wallStart := time.Now()
	return func() time.Time { return start.Add(time.Since(wallStart)) }
}

func waitForDigestCompletion(t *testing.T, ctx context.Context, pool *pgxpool.Pool, kind PeriodKind, date string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var completed bool
		err := pool.QueryRow(ctx, `SELECT completed_at IS NOT NULL FROM digest_periods WHERE kind=$1 AND local_date=$2::date`, string(kind), date).Scan(&completed)
		if err != nil {
			t.Fatal("cannot read digest completion state")
		}
		if completed {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("period %s %s was not completed before its next daily run", kind, date)
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

func TestAC1DigestSchedulerRecoversMissedPeriodsFromWatermarkAfterMondayRestartIntegration(t *testing.T) {
	pool, ctx := openDigestPool(t)
	setDigestTimezone(t, ctx, pool, "UTC")
	now := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	var queued []Period
	scheduler := &DigestScheduler{
		DB:  pool,
		Now: func() time.Time { return now },
		Enqueue: func(_ context.Context, _ pgx.Tx, period Period) error {
			queued = append(queued, period)
			return nil
		},
	}
	if err := scheduler.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(queued) != 2 || completedDigestPeriods(t, ctx, pool) != 2 {
		t.Fatalf("first schedule initialization queued %d periods, completed %d; want only the two currently due periods", len(queued), completedDigestPeriods(t, ctx, pool))
	}

	now = time.Date(2026, 3, 16, 7, 59, 0, 0, time.UTC)
	if err := scheduler.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if completedDigestPeriods(t, ctx, pool) != 7 {
		t.Fatalf("Monday before 08:00 completed %d periods, want the two initial and five missed daily periods", completedDigestPeriods(t, ctx, pool))
	}
	var earlyWeeklyRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM digest_periods WHERE kind='weekly' AND local_date='2026-03-09'`).Scan(&earlyWeeklyRows); err != nil || earlyWeeklyRows != 0 {
		t.Fatalf("the week ending Sunday has %d rows before Monday 08:00, err=%v", earlyWeeklyRows, err)
	}

	now = time.Date(2026, 3, 16, 8, 0, 0, 0, time.UTC)
	if err := scheduler.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if completedDigestPeriods(t, ctx, pool) != 9 {
		t.Fatalf("after Monday 08:00 completed %d periods, want seven daily and two weekly periods", completedDigestPeriods(t, ctx, pool))
	}
	for _, date := range []string{"2026-03-09", "2026-03-10", "2026-03-11", "2026-03-12", "2026-03-13", "2026-03-14", "2026-03-15"} {
		if _, completed, _ := digestPeriodState(t, ctx, pool, DailyPeriod, date); !completed {
			t.Errorf("missed daily period %s was not completed", date)
		}
	}
	for _, date := range []string{"2026-03-02", "2026-03-09"} {
		if _, completed, _ := digestPeriodState(t, ctx, pool, WeeklyPeriod, date); !completed {
			t.Errorf("missed weekly period %s was not completed", date)
		}
	}
	var dailyWatermark, weeklyWatermark string
	if err := pool.QueryRow(ctx, `SELECT scheduled_through::text FROM digest_schedule_state WHERE kind='daily'`).Scan(&dailyWatermark); err != nil {
		t.Fatal("daily schedule watermark was not persisted")
	}
	if err := pool.QueryRow(ctx, `SELECT scheduled_through::text FROM digest_schedule_state WHERE kind='weekly'`).Scan(&weeklyWatermark); err != nil {
		t.Fatal("weekly schedule watermark was not persisted")
	}
	if dailyWatermark != "2026-03-15" || weeklyWatermark != "2026-03-09" {
		t.Fatalf("schedule watermarks daily=%s weekly=%s", dailyWatermark, weeklyWatermark)
	}
}

func TestAC1DigestSchedulerUsesDisplayPreferenceBrowserFallbackIntegration(t *testing.T) {
	pool, ctx := openDigestPool(t)
	store := settings.NewPostgresStore(pool, nil, settings.Defaults(""))
	view, etag, err := store.Get(ctx)
	if err != nil {
		t.Fatal("cannot read owner display settings")
	}
	input := settings.InputOf(view)
	input.Timezone = "America/New_York"
	view, etag, err = store.Replace(ctx, etag, input)
	if err != nil {
		t.Fatal("cannot save the owner IANA display time zone")
	}
	input = settings.InputOf(view)
	input.Timezone = "browser"
	if _, _, err = store.Replace(ctx, etag, input); err != nil {
		t.Fatal("cannot switch the owner display preference to browser")
	}
	var retainedZone string
	var flat []byte
	if err := pool.QueryRow(ctx, `SELECT value FROM settings WHERE key='timezone'`).Scan(&flat); err != nil {
		t.Fatal("agent quiet-hours time zone was not retained")
	}
	if err := json.Unmarshal(flat, &retainedZone); err != nil || retainedZone != "America/New_York" {
		t.Fatalf("retained agent time zone=%q, err=%v", retainedZone, err)
	}

	now := time.Date(2026, 3, 10, 1, 0, 0, 0, time.UTC)
	var periods []Period
	scheduler := &DigestScheduler{
		DB:  pool,
		Now: fixedNow(now),
		Enqueue: func(_ context.Context, _ pgx.Tx, period Period) error {
			periods = append(periods, period)
			return nil
		},
	}
	if err := scheduler.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	daily := findDigestPeriod(t, periods, DailyPeriod, "2026-03-08")
	if daily.Timezone() != "UTC" {
		t.Fatalf("browser display preference was scheduled in %q, want UTC", daily.Timezone())
	}
}

func TestAC2DigestLoopWakesAtRetryEligibilityBeforeNextDailyRunIntegration(t *testing.T) {
	pool, ctx := openDigestPool(t)
	setDigestTimezone(t, ctx, pool, "UTC")
	start := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	now := movingDigestNow(start)
	retryAt := now().Add(250 * time.Millisecond)
	if _, err := pool.Exec(ctx, `INSERT INTO digest_periods (kind, local_date, timezone, claimed_at, next_attempt_at)
		VALUES ('daily', '2026-03-09', 'UTC', $1, $2)`, now(), retryAt); err != nil {
		t.Fatal("cannot insert the retry-delayed digest fixture")
	}
	queued := make(chan struct{}, 1)
	scheduler := &DigestScheduler{
		DB:                   pool,
		Now:                  now,
		retryDelay:           40 * time.Millisecond,
		schedulePollInterval: 5 * time.Second,
		Enqueue: func(_ context.Context, _ pgx.Tx, period Period) error {
			if period.Kind() == DailyPeriod && period.LocalDate() == "2026-03-09" {
				queued <- struct{}{}
			}
			return nil
		},
	}
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { scheduler.Loop(loopCtx); close(done) }()
	select {
	case <-queued:
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatal("retry-delayed period did not run after becoming eligible")
	}
	waitForDigestCompletion(t, ctx, pool, DailyPeriod, "2026-03-09")
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("digest loop did not stop after cancellation")
	}
}

func TestAC2DigestLoopWakesAtLeaseExpiryBeforeNextDailyRunIntegration(t *testing.T) {
	pool, ctx := openDigestPool(t)
	setDigestTimezone(t, ctx, pool, "UTC")
	start := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	now := movingDigestNow(start)
	claimedAt := now()
	leaseUntil := claimedAt.Add(250 * time.Millisecond)
	if _, err := pool.Exec(ctx, `INSERT INTO digest_periods (kind, local_date, timezone, claimed_at, lease_id, lease_until, attempt_count, next_attempt_at)
		VALUES ('daily', '2026-03-09', 'UTC', $1, gen_random_uuid(), $2, 1, $1)`, claimedAt, leaseUntil); err != nil {
		t.Fatal("cannot insert the leased digest fixture")
	}
	queued := make(chan struct{}, 1)
	scheduler := &DigestScheduler{
		DB:                   pool,
		Now:                  now,
		retryDelay:           40 * time.Millisecond,
		schedulePollInterval: 5 * time.Second,
		Enqueue: func(_ context.Context, _ pgx.Tx, period Period) error {
			if period.Kind() == DailyPeriod && period.LocalDate() == "2026-03-09" {
				queued <- struct{}{}
			}
			return nil
		},
	}
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { scheduler.Loop(loopCtx); close(done) }()
	select {
	case <-queued:
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatal("expired lease was not recovered before the next daily run")
	}
	waitForDigestCompletion(t, ctx, pool, DailyPeriod, "2026-03-09")
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("digest loop did not stop after cancellation")
	}
}

func TestAC1DigestLoopRechecksOwnerTimezoneWhileWaitingIntegration(t *testing.T) {
	pool, ctx := openDigestPool(t)
	setDigestTimezone(t, ctx, pool, "UTC")
	clockMu := sync.Mutex{}
	nowValue := time.Date(2026, 3, 10, 0, 59, 59, 0, time.UTC)
	now := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return nowValue
	}
	reads := atomic.Int32{}
	sleeping, resume := make(chan struct{}), make(chan struct{})
	queued := make(chan struct{}, 1)
	scheduler := &DigestScheduler{
		DB:                   pool,
		Now:                  now,
		schedulePollInterval: 25 * time.Millisecond,
		Timezone: func(readCtx context.Context) (string, error) {
			var raw []byte
			if err := pool.QueryRow(readCtx, `SELECT value FROM settings WHERE key='display'`).Scan(&raw); err != nil {
				return "", err
			}
			var preferences struct {
				Timezone string `json:"timezone"`
			}
			if err := json.Unmarshal(raw, &preferences); err != nil {
				return "", err
			}
			if reads.Add(1) == 2 {
				close(sleeping)
				<-resume
			}
			return preferences.Timezone, nil
		},
		Enqueue: func(_ context.Context, _ pgx.Tx, period Period) error {
			if period.Kind() == DailyPeriod && period.LocalDate() == "2026-03-09" {
				queued <- struct{}{}
			}
			return nil
		},
	}
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { scheduler.Loop(loopCtx); close(done) }()
	select {
	case <-sleeping:
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatal("digest loop did not enter its scheduled wait")
	}
	setDigestTimezone(t, ctx, pool, "Asia/Ho_Chi_Minh")
	clockMu.Lock()
	nowValue = time.Date(2026, 3, 10, 1, 0, 0, 0, time.UTC)
	clockMu.Unlock()
	close(resume)
	select {
	case <-queued:
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatal("digest loop did not claim the new owner-local 08:00 period")
	}
	waitForDigestCompletion(t, ctx, pool, DailyPeriod, "2026-03-09")
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("digest loop did not stop after cancellation")
	}
}

func TestAC1DigestSchedulerKeepsSelectionAndClaimOnOneTimezoneSnapshotIntegration(t *testing.T) {
	pool, ctx := openDigestPool(t)
	now := time.Date(2026, 3, 10, 1, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	var periods []Period
	scheduler := &DigestScheduler{
		DB:  pool,
		Now: fixedNow(now),
		Timezone: func(context.Context) (string, error) {
			if calls.Add(1) == 1 {
				return "Asia/Ho_Chi_Minh", nil
			}
			return "UTC", nil
		},
		Enqueue: func(_ context.Context, _ pgx.Tx, period Period) error {
			periods = append(periods, period)
			return nil
		},
	}
	if err := scheduler.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("owner time zone was read %d times in one scheduling decision, want 1", calls.Load())
	}
	findDigestPeriod(t, periods, DailyPeriod, "2026-03-09")
	for _, period := range periods {
		if period.Timezone() != "Asia/Ho_Chi_Minh" {
			t.Errorf("period %s %s stored zone %q, want the selection snapshot", period.Kind(), period.LocalDate(), period.Timezone())
		}
	}
}

func TestAC2DigestSchedulerUsesFreshLeaseForEachSequentialClaimIntegration(t *testing.T) {
	pool, ctx := openDigestPool(t)
	setDigestTimezone(t, ctx, pool, "UTC")
	now := movingDigestNow(time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC))
	lease := 1200 * time.Millisecond
	weeklyStarted, releaseWeekly := make(chan struct{}), make(chan struct{})
	dailyStarted, releaseDaily := make(chan struct{}), make(chan struct{})
	secondStarted, releaseSecond := make(chan struct{}), make(chan struct{})
	first := &DigestScheduler{
		DB:            pool,
		Now:           now,
		leaseDuration: lease,
		Enqueue: func(_ context.Context, _ pgx.Tx, period Period) error {
			if period.Kind() == WeeklyPeriod {
				close(weeklyStarted)
				<-releaseWeekly
				return nil
			}
			close(dailyStarted)
			<-releaseDaily
			return nil
		},
	}
	firstDone := make(chan error, 1)
	go func() { firstDone <- first.RunOnce(ctx) }()
	select {
	case <-weeklyStarted:
	case <-time.After(time.Second):
		t.Fatal("first weekly enqueue did not start")
	}
	timer := time.NewTimer(lease + 150*time.Millisecond)
	<-timer.C
	close(releaseWeekly)
	select {
	case <-dailyStarted:
	case <-time.After(time.Second):
		close(releaseDaily)
		t.Fatal("second daily enqueue did not start")
	}
	var leaseUntil time.Time
	if err := pool.QueryRow(ctx, `SELECT lease_until FROM digest_periods WHERE kind='daily' AND local_date='2026-03-09'`).Scan(&leaseUntil); err != nil {
		close(releaseDaily)
		t.Fatal("cannot read second period lease")
	}
	if !leaseUntil.After(now()) {
		close(releaseDaily)
		t.Fatalf("second period lease expired while its callback was active: %s", leaseUntil)
	}
	second := &DigestScheduler{
		DB:  pool,
		Now: now,
		Enqueue: func(context.Context, pgx.Tx, Period) error {
			close(secondStarted)
			<-releaseSecond
			return nil
		},
	}
	secondDone := make(chan error, 1)
	go func() { secondDone <- second.RunOnce(ctx) }()
	select {
	case <-secondStarted:
		close(releaseSecond)
		close(releaseDaily)
		<-firstDone
		<-secondDone
		t.Fatal("another center reclaimed the active second period")
	case err := <-secondDone:
		if err != nil {
			close(releaseDaily)
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		close(releaseDaily)
		t.Fatal("second center did not finish its claim check")
	}
	close(releaseDaily)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
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
	if _, err := pool.Exec(ctx, `CREATE TABLE digest_test_enqueues (kind text NOT NULL, local_date date NOT NULL, PRIMARY KEY (kind, local_date))`); err != nil {
		t.Fatal("cannot create transactional enqueue test table")
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DROP TABLE IF EXISTS digest_test_enqueues`); err != nil {
			t.Error("cannot drop transactional enqueue test table")
		}
	})
	if _, err := pool.Exec(ctx, `INSERT INTO digest_periods (kind, local_date, timezone, completed_at)
		VALUES ('weekly', '2026-03-02', 'UTC', $1)`, now); err != nil {
		t.Fatal("cannot seed the unrelated weekly period")
	}
	failure := errors.New("enqueue failed")
	failed := &DigestScheduler{
		DB:  pool,
		Now: fixedNow(now),
		Enqueue: func(ctx context.Context, tx pgx.Tx, period Period) error {
			if _, err := tx.Exec(ctx, `INSERT INTO digest_test_enqueues (kind, local_date) VALUES ($1, $2::date)`, string(period.Kind()), period.LocalDate()); err != nil {
				return err
			}
			return failure
		},
	}
	if err := failed.RunOnce(ctx); !errors.Is(err, failure) {
		t.Fatalf("failed enqueue error = %v, want the enqueue error", err)
	}
	var enqueues int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM digest_test_enqueues`).Scan(&enqueues); err != nil || enqueues != 0 {
		t.Fatalf("failed enqueue committed %d transactional records, err=%v", enqueues, err)
	}
	if count := completedDigestPeriods(t, ctx, pool); count != 1 {
		t.Fatalf("failed enqueue changed %d completed periods, want only the unrelated weekly fixture", count)
	}
	if attempts, completed, _ := digestPeriodState(t, ctx, pool, DailyPeriod, "2026-03-09"); attempts != 1 || completed {
		t.Fatalf("daily period after failure attempts=%d completed=%v", attempts, completed)
	}

	retryAt := now.Add(10 * time.Minute)
	var successes atomic.Int32
	restarted := &DigestScheduler{
		DB:  pool,
		Now: fixedNow(retryAt),
		Enqueue: func(ctx context.Context, tx pgx.Tx, period Period) error {
			if _, err := tx.Exec(ctx, `INSERT INTO digest_test_enqueues (kind, local_date) VALUES ($1, $2::date)`, string(period.Kind()), period.LocalDate()); err != nil {
				return err
			}
			successes.Add(1)
			return nil
		},
	}
	if err := restarted.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if successes.Load() != 1 || completedDigestPeriods(t, ctx, pool) != 2 {
		t.Fatalf("restart successes=%d completed=%d, want one new completion", successes.Load(), completedDigestPeriods(t, ctx, pool))
	}
	if attempts, completed, _ := digestPeriodState(t, ctx, pool, DailyPeriod, "2026-03-09"); attempts != 2 || !completed {
		t.Fatalf("daily period after retry attempts=%d completed=%v", attempts, completed)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM digest_test_enqueues`).Scan(&enqueues); err != nil || enqueues != 1 {
		t.Fatalf("successful retry committed %d enqueue records, err=%v", enqueues, err)
	}
	if err := restarted.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if successes.Load() != 1 {
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

func TestAC1DigestSchedulerRecoversDowntimeAcrossTimezoneChangeIntegration(t *testing.T) {
	pool, ctx := openDigestPool(t)
	setDigestTimezone(t, ctx, pool, "UTC")
	now := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	scheduler := &DigestScheduler{DB: pool, Now: func() time.Time { return now }, Enqueue: func(context.Context, pgx.Tx, Period) error { return nil }}
	if err := scheduler.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	setDigestTimezone(t, ctx, pool, "Asia/Ho_Chi_Minh")
	now = time.Date(2026, 3, 16, 9, 0, 0, 0, time.UTC)
	if err := scheduler.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := completedDigestPeriods(t, ctx, pool); got != 9 {
		t.Fatalf("completed periods after time-zone change and downtime = %d, want 9", got)
	}
	if _, completed, zone := digestPeriodState(t, ctx, pool, DailyPeriod, "2026-03-11"); !completed || zone != "Asia/Ho_Chi_Minh" {
		t.Fatalf("missed daily period: completed=%v zone=%q", completed, zone)
	}
	if _, completed, zone := digestPeriodState(t, ctx, pool, WeeklyPeriod, "2026-03-09"); !completed || zone != "Asia/Ho_Chi_Minh" {
		t.Fatalf("missed weekly period: completed=%v zone=%q", completed, zone)
	}
	if _, completed, zone := digestPeriodState(t, ctx, pool, DailyPeriod, "2026-03-09"); !completed || zone != "UTC" {
		t.Fatalf("original period snapshot changed: completed=%v zone=%q", completed, zone)
	}
}

func TestAC1DigestSchedulerPersistsFallDSTEnvelopesIntegration(t *testing.T) {
	pool, ctx := openDigestPool(t)
	setDigestTimezone(t, ctx, pool, "America/New_York")
	now := time.Date(2026, 11, 2, 13, 0, 0, 0, time.UTC)
	var periods []Period
	scheduler := &DigestScheduler{DB: pool, Now: fixedNow(now), Enqueue: func(_ context.Context, _ pgx.Tx, period Period) error {
		periods = append(periods, period)
		return nil
	}}
	if err := scheduler.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		kind PeriodKind
		date string
		span time.Duration
	}{
		{DailyPeriod, "2026-11-01", 25 * time.Hour},
		{WeeklyPeriod, "2026-10-26", 169 * time.Hour},
	} {
		period := findDigestPeriod(t, periods, tc.kind, tc.date)
		if got := period.To().Sub(period.From()); got != tc.span {
			t.Errorf("%s envelope = %s, want %s", tc.kind, got, tc.span)
		}
		if _, completed, zone := digestPeriodState(t, ctx, pool, tc.kind, tc.date); !completed || zone != "America/New_York" {
			t.Errorf("persisted %s: completed=%v zone=%q", tc.kind, completed, zone)
		}
	}
}

func TestAC2DigestLoopKeepsEarlierPendingWakeupAfterRunErrorIntegration(t *testing.T) {
	pool, ctx := openDigestPool(t)
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `INSERT INTO digest_periods (kind, local_date, timezone, next_attempt_at)
		VALUES ('daily', '2026-03-09', 'UTC', $1)`, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	scheduler := &DigestScheduler{DB: pool, Now: fixedNow(now), Timezone: func(context.Context) (string, error) { return "UTC", nil },
		retryDelay: 5 * time.Minute, schedulePollInterval: 10 * time.Minute}
	if got := scheduler.loopWait(ctx, withDigestCode("enqueue", errors.New("credential=private-value"))); got != time.Minute {
		t.Fatalf("loop wait after failed run = %s, want pending eligibility in one minute", got)
	}
}

type delayedDigestRenewDB struct {
	*pgxpool.Pool
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (db *delayedDigestRenewDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "UPDATE digest_periods SET lease_until") {
		db.once.Do(func() { close(db.started) })
		<-db.release
	}
	return db.Pool.Exec(ctx, sql, args...)
}

func TestAC2DigestSchedulerIgnoresRenewalAfterCommittedCompletionIntegration(t *testing.T) {
	pool, ctx := openDigestPool(t)
	setDigestTimezone(t, ctx, pool, "UTC")
	now := movingDigestNow(time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC))
	if _, err := pool.Exec(ctx, `INSERT INTO digest_periods (kind, local_date, timezone, completed_at)
		VALUES ('weekly', '2026-03-02', 'UTC', $1)`, now()); err != nil {
		t.Fatal(err)
	}
	db := &delayedDigestRenewDB{Pool: pool, started: make(chan struct{}), release: make(chan struct{})}
	scheduler := &DigestScheduler{DB: db, Now: now, leaseDuration: 90 * time.Millisecond,
		Enqueue: func(ctx context.Context, _ pgx.Tx, _ Period) error {
			select {
			case <-db.started:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}
	done := make(chan error, 1)
	go func() { done <- scheduler.RunOnce(ctx) }()
	select {
	case <-db.started:
	case <-time.After(time.Second):
		close(db.release)
		t.Fatal("lease renewal did not begin")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		var completed bool
		if err := pool.QueryRow(ctx, `SELECT completed_at IS NOT NULL FROM digest_periods WHERE kind='daily' AND local_date='2026-03-09'`).Scan(&completed); err != nil {
			close(db.release)
			t.Fatal(err)
		}
		if completed {
			break
		}
		if time.Now().After(deadline) {
			close(db.release)
			t.Fatal("digest completion did not commit while renewal was delayed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(db.release)
	if err := <-done; err != nil {
		t.Fatalf("committed digest was reported as a lease failure: %v", err)
	}
}
