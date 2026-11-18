package report

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const digestHour = 8

const digestDateLayout = "2006-01-02"

// digestPeriodKey is one local calendar period that a digest may claim.
type digestPeriodKey struct {
	kind      PeriodKind
	localDate string
}

// dueDigestPeriods returns the latest daily period and, once Monday 08:00 has
// passed, the preceding Monday-Sunday week. Before 08:00 the previous day is
// still waiting for its own run.
func dueDigestPeriods(now time.Time, location *time.Location) []digestPeriodKey {
	local := now.In(location)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
	daily := day.AddDate(0, 0, -1)
	if local.Hour() < digestHour {
		daily = daily.AddDate(0, 0, -1)
	}
	out := []digestPeriodKey{{kind: DailyPeriod, localDate: daily.Format(digestDateLayout)}}
	weekStart := day.AddDate(0, 0, -daysSinceMonday(day.Weekday()))
	if !now.Before(digestHourOf(weekStart)) {
		out = append(out, digestPeriodKey{kind: WeeklyPeriod, localDate: weekStart.AddDate(0, 0, -7).Format(digestDateLayout)})
	}
	return out
}

func daysSinceMonday(weekday time.Weekday) int {
	return (int(weekday) + 6) % 7
}

func digestHourOf(day time.Time) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), digestHour, 0, 0, 0, day.Location())
}

// nextDigestRun returns the next owner-local 08:00 strictly after now.
func nextDigestRun(now time.Time, location *time.Location) time.Time {
	local := now.In(location)
	next := digestHourOf(time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location))
	if !next.After(now) {
		tomorrow := local.AddDate(0, 0, 1)
		next = digestHourOf(time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 0, 0, 0, 0, location))
	}
	return next
}

// digestTimezone turns the owner display preference into an IANA zone the
// scheduler can use. A browser-only or invalid value falls back to UTC.
func digestTimezone(zone string) string {
	zone = strings.TrimSpace(zone)
	switch zone {
	case "", "browser", "Local":
		return "UTC"
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return "UTC"
	}
	return zone
}

// DigestDB is the part of the PostgreSQL pool the digest scheduler needs.
type DigestDB interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Begin(context.Context) (pgx.Tx, error)
}

// DigestEnqueue publishes one due digest and marks its period complete in the
// caller's transaction, so a failed enqueue never completes or duplicates it.
type DigestEnqueue func(ctx context.Context, tx pgx.Tx, period Period) error

// DigestScheduler identifies due daily and weekly periods and claims each one
// durably across restarts and multiple center instances.
type DigestScheduler struct {
	DB       DigestDB
	Enqueue  DigestEnqueue
	Now      func() time.Time
	Timezone func(context.Context) (string, error)
	Log      *slog.Logger

	leaseDuration time.Duration
	retryDelay    time.Duration
}

const (
	digestClaimLease  = 15 * time.Minute
	digestRetryDelay  = 5 * time.Minute
	digestCleanupTime = 5 * time.Second
)

var errDigestLeaseLost = errors.New("digest period lease is no longer owned")

func (s *DigestScheduler) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *DigestScheduler) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *DigestScheduler) leaseTTL() time.Duration {
	if s.leaseDuration > 0 {
		return s.leaseDuration
	}
	return digestClaimLease
}

func (s *DigestScheduler) retry() time.Duration {
	if s.retryDelay > 0 {
		return s.retryDelay
	}
	return digestRetryDelay
}

// RunOnce processes every ready pending period and every period due now. A
// failure in one period does not stop the others; the failed one stays ready
// for a later attempt.
func (s *DigestScheduler) RunOnce(ctx context.Context) error {
	now := s.now()
	location, err := s.location(ctx)
	if err != nil {
		return err
	}
	keys, err := s.periodKeys(ctx, now, location)
	if err != nil {
		return err
	}
	var errs []error
	for _, key := range keys {
		if err := s.process(ctx, key, now); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *DigestScheduler) location(ctx context.Context) (*time.Location, error) {
	zone, err := s.ownerTimezone(ctx)
	if err != nil {
		return nil, err
	}
	return time.LoadLocation(zone)
}

func (s *DigestScheduler) ownerTimezone(ctx context.Context) (string, error) {
	if s.Timezone != nil {
		zone, err := s.Timezone(ctx)
		return digestTimezone(zone), err
	}
	var raw []byte
	err := s.DB.QueryRow(ctx, `SELECT value FROM settings WHERE key = 'timezone' AND value IS NOT NULL`).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return "UTC", nil
	}
	if err != nil {
		return "", err
	}
	var zone string
	if json.Unmarshal(raw, &zone) != nil {
		return "UTC", nil
	}
	return digestTimezone(zone), nil
}

func (s *DigestScheduler) periodKeys(ctx context.Context, now time.Time, location *time.Location) ([]digestPeriodKey, error) {
	pending, err := s.pending(ctx, now)
	if err != nil {
		return nil, err
	}
	return mergeDigestPeriods(pending, dueDigestPeriods(now, location)), nil
}

func (s *DigestScheduler) pending(ctx context.Context, now time.Time) ([]digestPeriodKey, error) {
	rows, err := s.DB.Query(ctx, `SELECT kind, local_date::text FROM digest_periods
		WHERE completed_at IS NULL AND next_attempt_at <= $1
			AND (lease_until IS NULL OR lease_until <= $1)
		ORDER BY local_date, kind`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []digestPeriodKey
	for rows.Next() {
		var kind, localDate string
		if err := rows.Scan(&kind, &localDate); err != nil {
			return nil, err
		}
		keys = append(keys, digestPeriodKey{kind: PeriodKind(kind), localDate: localDate})
	}
	return keys, rows.Err()
}

func mergeDigestPeriods(groups ...[]digestPeriodKey) []digestPeriodKey {
	seen := make(map[digestPeriodKey]bool)
	var keys []digestPeriodKey
	for _, group := range groups {
		for _, key := range group {
			if !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
	}
	return keys
}

func (s *DigestScheduler) process(ctx context.Context, key digestPeriodKey, now time.Time) error {
	leaseID, zone, claimed, err := s.claim(ctx, key, now)
	if err != nil || !claimed {
		return err
	}
	period, err := NewPeriod(key.kind, key.localDate, zone)
	if err != nil {
		return s.release(ctx, key, leaseID, s.now().Add(s.retry()), err)
	}
	workCtx, finishLease := s.maintainLease(ctx, key, leaseID, s.leaseTTL())
	enqueueErr := s.enqueue(workCtx, key, leaseID, period)
	leaseErr := finishLease()
	if leaseErr != nil {
		return errors.Join(enqueueErr, leaseErr)
	}
	if enqueueErr != nil {
		return s.release(ctx, key, leaseID, s.now().Add(s.retry()), enqueueErr)
	}
	return nil
}

func (s *DigestScheduler) claim(ctx context.Context, key digestPeriodKey, now time.Time) (string, string, bool, error) {
	zone, err := s.ownerTimezone(ctx)
	if err != nil {
		return "", "", false, err
	}
	leaseID := uuid.NewString()
	var storedZone string
	err = s.DB.QueryRow(ctx, claimDigestSQL, string(key.kind), key.localDate, zone, now, leaseID, now.Add(s.leaseTTL())).Scan(&storedZone)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return leaseID, storedZone, true, nil
}

const claimDigestSQL = `INSERT INTO digest_periods (kind, local_date, timezone, claimed_at, lease_id, lease_until, attempt_count, next_attempt_at)
VALUES ($1, $2::date, $3, $4, $5, $6, 1, $4)
ON CONFLICT (kind, local_date) DO UPDATE SET claimed_at = EXCLUDED.claimed_at, lease_id = EXCLUDED.lease_id,
	lease_until = EXCLUDED.lease_until, attempt_count = digest_periods.attempt_count + 1,
	next_attempt_at = EXCLUDED.next_attempt_at
WHERE digest_periods.completed_at IS NULL AND digest_periods.next_attempt_at <= EXCLUDED.claimed_at
	AND (digest_periods.lease_until IS NULL OR digest_periods.lease_until <= EXCLUDED.claimed_at)
RETURNING timezone`

func (s *DigestScheduler) enqueue(ctx context.Context, key digestPeriodKey, leaseID string, period Period) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if s.Enqueue == nil {
		return errors.New("digest enqueue callback is not configured")
	}
	if err := s.Enqueue(ctx, tx, period); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, completeDigestSQL, string(key.kind), key.localDate, leaseID, s.now())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errDigestLeaseLost
	}
	return tx.Commit(ctx)
}

const completeDigestSQL = `UPDATE digest_periods SET completed_at = $4, lease_id = NULL, lease_until = NULL, next_attempt_at = $4
	WHERE kind = $1 AND local_date = $2::date AND lease_id = $3 AND completed_at IS NULL`

func (s *DigestScheduler) maintainLease(ctx context.Context, key digestPeriodKey, leaseID string, ttl time.Duration) (context.Context, func() error) {
	workCtx, cancel := context.WithCancel(ctx)
	interval := ttl / 3
	if interval <= 0 {
		interval = time.Second
	}
	done, failures := make(chan struct{}), make(chan error, 1)
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-workCtx.Done():
				return
			case <-ticker.C:
				if err := s.renewLease(ctx, key, leaseID, s.now().Add(ttl)); err != nil {
					failures <- err
					cancel()
					return
				}
			}
		}
	}()
	return workCtx, func() error {
		cancel()
		<-done
		select {
		case err := <-failures:
			return err
		default:
			return nil
		}
	}
}

func (s *DigestScheduler) renewLease(ctx context.Context, key digestPeriodKey, leaseID string, until time.Time) error {
	renewCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), digestCleanupTime)
	defer cancel()
	tag, err := s.DB.Exec(renewCtx, `UPDATE digest_periods SET lease_until = $4
		WHERE kind = $1 AND local_date = $2::date AND lease_id = $3 AND completed_at IS NULL`,
		string(key.kind), key.localDate, leaseID, until)
	if err != nil {
		return fmt.Errorf("could not renew digest period lease: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errDigestLeaseLost
	}
	return nil
}

func (s *DigestScheduler) release(ctx context.Context, key digestPeriodKey, leaseID string, retryAt time.Time, cause error) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), digestCleanupTime)
	defer cancel()
	tag, err := s.DB.Exec(cleanup, `UPDATE digest_periods SET lease_id = NULL, lease_until = NULL, next_attempt_at = $4
		WHERE kind = $1 AND local_date = $2::date AND lease_id = $3 AND completed_at IS NULL`,
		string(key.kind), key.localDate, leaseID, retryAt)
	if err != nil {
		return errors.Join(cause, fmt.Errorf("could not release digest period lease: %w", err))
	}
	if tag.RowsAffected() != 1 {
		return errors.Join(cause, errDigestLeaseLost)
	}
	return cause
}

// Loop runs immediately and then at the next owner-local 08:00. A failed run is
// retried after the short retry delay instead of waiting for the next day.
func (s *DigestScheduler) Loop(ctx context.Context) {
	for {
		runErr := s.RunOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		wait := s.loopWait(ctx, runErr)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (s *DigestScheduler) loopWait(ctx context.Context, runErr error) time.Duration {
	if runErr != nil {
		s.logger().Warn("digest schedule run failed; it will be retried", "error", "see the center log")
		return s.retry()
	}
	location, err := s.location(ctx)
	if err != nil {
		s.logger().Warn("digest schedule time zone is not readable; it will be retried", "error", "see the center log")
		return s.retry()
	}
	now := s.now()
	if wait := nextDigestRun(now, location).Sub(now); wait > 0 {
		return wait
	}
	return s.retry()
}
