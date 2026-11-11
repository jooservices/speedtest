package report

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ClaimDB coordinates monthly report claims across center instances.
type ClaimDB interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Mailer sends the previous month's report on the first days of each month through a notification channel.
type Mailer struct {
	Reports *Store
	DB      ClaimDB
	// Send delivers the message on the e-mail channel; it returns an error when the channel is not configured.
	Send  func(ctx context.Context, build func(centerURL string) string) error
	Log   *slog.Logger
	Now   func() time.Time
	Every time.Duration

	leaseDuration time.Duration
}

const (
	claimLease  = 15 * time.Minute
	retryDelay  = 5 * time.Minute
	cleanupTime = 5 * time.Second
)

var errLeaseLost = errors.New("monthly report lease is no longer owned")

func (m *Mailer) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

// Due reports whether the previous month should be sent now: the first three days of a month, after 01:00 UTC on day 1.
func Due(now time.Time) bool {
	now = now.UTC()
	return now.Day() <= 3 && (now.Day() > 1 || now.Hour() >= 1)
}

// RunOnce sends an eligible queued report, or the previous month when the send window is open.
func (m *Mailer) RunOnce(ctx context.Context) error {
	now := m.now()
	month, found, leaseExpired, retryReady, err := m.pendingMonth(ctx, now)
	if err != nil {
		return err
	}
	if found && (!retryReady || (!Due(now) && !leaseExpired)) {
		return nil
	}
	if !found {
		if !Due(now) {
			return nil
		}
		month = PreviousMonth(now)
	}
	leaseID := uuid.NewString()
	ttl := m.leaseTTL()
	tag, err := m.DB.Exec(ctx, claimSQL, month, leaseID, now.Add(ttl), now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	workCtx, finishLease := m.maintainLease(ctx, month, leaseID, ttl)
	data, err := m.Reports.Build(workCtx, month, "", "")
	if err == nil {
		err = m.Send(workCtx, func(centerURL string) string { return Summary(data, centerURL) })
	}
	if leaseErr := finishLease(); leaseErr != nil {
		return errors.Join(err, leaseErr)
	}
	if err != nil {
		return m.release(ctx, month, leaseID, m.now().Add(retryDelay), err)
	}
	return m.markSent(ctx, month, leaseID, m.now())
}

const pendingSQL = `SELECT month,
	lease_until IS NOT NULL AND lease_until <= $1,
	next_attempt_at <= $1
FROM monthly_report_runs WHERE sent_at IS NULL ORDER BY month LIMIT 1`

func (m *Mailer) pendingMonth(ctx context.Context, now time.Time) (string, bool, bool, bool, error) {
	var month string
	var expired, ready bool
	err := m.DB.QueryRow(ctx, pendingSQL, now).Scan(&month, &expired, &ready)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, false, false, nil
	}
	if err != nil {
		return "", false, false, false, err
	}
	return month, true, expired, ready, nil
}

func (m *Mailer) leaseTTL() time.Duration {
	if m.leaseDuration > 0 {
		return m.leaseDuration
	}
	return claimLease
}

func (m *Mailer) maintainLease(ctx context.Context, month, leaseID string, ttl time.Duration) (context.Context, func() error) {
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
				err := m.renewLease(ctx, month, leaseID, m.now().Add(ttl))
				if err != nil {
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

func (m *Mailer) renewLease(ctx context.Context, month, leaseID string, until time.Time) error {
	renewCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTime)
	defer cancel()
	tag, err := m.DB.Exec(renewCtx, renewSQL, month, leaseID, until)
	if err != nil {
		return fmt.Errorf("could not renew monthly report lease: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errLeaseLost
	}
	return nil
}

const claimSQL = `INSERT INTO monthly_report_runs (month, claimed_at, lease_id, lease_until, attempt_count, next_attempt_at)
VALUES ($1, $4, $2, $3, 1, $4)
ON CONFLICT (month) DO UPDATE SET claimed_at = EXCLUDED.claimed_at, lease_id = EXCLUDED.lease_id,
	lease_until = EXCLUDED.lease_until, attempt_count = monthly_report_runs.attempt_count + 1
WHERE monthly_report_runs.sent_at IS NULL AND monthly_report_runs.next_attempt_at <= EXCLUDED.claimed_at
	AND (monthly_report_runs.lease_until IS NULL OR monthly_report_runs.lease_until <= EXCLUDED.claimed_at)`

const renewSQL = `UPDATE monthly_report_runs SET lease_until = $3
	WHERE month = $1 AND lease_id = $2 AND sent_at IS NULL`

func (m *Mailer) release(ctx context.Context, month, leaseID string, retryAt time.Time, cause error) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTime)
	defer cancel()
	tag, err := m.DB.Exec(cleanup, `UPDATE monthly_report_runs SET lease_id = NULL, lease_until = NULL, next_attempt_at = $3
		WHERE month = $1 AND lease_id = $2 AND sent_at IS NULL`, month, leaseID, retryAt)
	if err != nil {
		return errors.Join(cause, fmt.Errorf("could not release monthly report lease: %w", err))
	}
	if tag.RowsAffected() != 1 {
		return errors.Join(cause, errLeaseLost)
	}
	return cause
}

func (m *Mailer) markSent(ctx context.Context, month, leaseID string, at time.Time) error {
	tag, err := m.DB.Exec(ctx, `UPDATE monthly_report_runs SET sent_at = $3, lease_id = NULL, lease_until = NULL
		WHERE month = $1 AND lease_id = $2 AND sent_at IS NULL`, month, leaseID, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errLeaseLost
	}
	return nil
}

// Loop checks hourly until cancelled.
func (m *Mailer) Loop(ctx context.Context) {
	every := m.Every
	if every <= 0 {
		every = time.Hour
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if err := m.RunOnce(ctx); err != nil && ctx.Err() == nil && m.Log != nil {
			m.Log.Warn("monthly report e-mail not sent; it will be retried", "error", errors.New("see the notification settings"))
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Summary is the plain-text body: one line per ISP and node, with a link to the full PDF and CSV.
func Summary(r Report, centerURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Monthly ISP and plan report %s (UTC)\n\n", r.Month)
	if len(r.Rows) == 0 {
		b.WriteString("No tests reported this month.\n")
	}
	for _, row := range r.Rows {
		fmt.Fprintf(&b, "%s / %s: %d tests, %d failed, avg down %s Mbps, delivered %s%%, downtime %d min, IP changes %d\n",
			row.ISP, row.Node, row.Tests, row.Failures, num(row.AvgDown), num(row.DeliveredPct), row.DowntimeMin, row.IPChanges)
	}
	if base := strings.TrimRight(centerURL, "/"); base != "" {
		fmt.Fprintf(&b, "\nFull report (sign in first): %s/api/reports/monthly?month=%s&format=pdf\n", base, r.Month)
	}
	return b.String()
}
