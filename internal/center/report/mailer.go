package report

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// ClaimDB claims a month so only one center sends it, once.
type ClaimDB interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
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
}

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

// RunOnce sends the previous month's report if it is due and not yet sent. A failed send releases the claim so the
// next pass retries, until the window closes.
func (m *Mailer) RunOnce(ctx context.Context) error {
	now := m.now()
	if !Due(now) {
		return nil
	}
	month := PreviousMonth(now)
	tag, err := m.DB.Exec(ctx, `INSERT INTO monthly_report_runs (month) VALUES ($1) ON CONFLICT (month) DO NOTHING`, month)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	data, err := m.Reports.Build(ctx, month, "", "")
	if err == nil {
		err = m.Send(ctx, func(centerURL string) string { return Summary(data, centerURL) })
	}
	if err != nil {
		_, _ = m.DB.Exec(ctx, `DELETE FROM monthly_report_runs WHERE month = $1 AND sent_at IS NULL`, month)
		return err
	}
	_, err = m.DB.Exec(ctx, `UPDATE monthly_report_runs SET sent_at = now() WHERE month = $1`, month)
	return err
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
		b.WriteString("No tests with a known ISP this month.\n")
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
