// Package jobs holds the command center's background jobs.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	// DefaultRetentionDays applies when the owner has not changed the setting.
	DefaultRetentionDays = 365
	defaultBatch         = 5000
	defaultPause         = 100 * time.Millisecond
	// RunHour and RunMinute are when the daily job starts, in UTC.
	RunHour, RunMinute = 3, 15
)

// DB is the subset of pgxpool.Pool the jobs use.
type DB interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Result describes one run of the retention job.
type Result struct {
	RetentionDays int
	Deleted       int64
	// AuditDeleted counts change-log entries removed with the same cut-off.
	AuditDeleted int64
	Batches      int
	Duration     time.Duration
	// Skipped is set when nothing was deleted on purpose, for example because retention is "keep forever".
	Skipped string
}

// Retention deletes results older than the retention setting, in small batches so that ingestion is never blocked.
type Retention struct {
	// ObserveRun reports pass success without changing worker behavior.
	ObserveRun func(bool)
	DB         DB
	Logger     *slog.Logger
	Now        func() time.Time
	BatchSize  int
	// Pause is the rest between two full batches that lets other transactions in.
	Pause time.Duration
}

func (r *Retention) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *Retention) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

// days reads the retentionDays setting; 0 means keep everything.
func (r *Retention) days(ctx context.Context) (int, error) {
	var raw []byte
	err := r.DB.QueryRow(ctx, `SELECT value FROM settings WHERE key = 'retentionDays' AND value IS NOT NULL`).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultRetentionDays, nil
	}
	if err != nil {
		return 0, err
	}
	var days int
	if err := json.Unmarshal(raw, &days); err != nil || days < 0 {
		return 0, errors.New("retentionDays setting is not a non-negative integer")
	}
	return days, nil
}

// Run performs one pass and logs its outcome.
func (r *Retention) Run(ctx context.Context) (Result, error) {
	start := time.Now()
	res, err := r.run(ctx)
	if r.ObserveRun != nil {
		r.ObserveRun(err == nil)
	}
	res.Duration = time.Since(start)
	log := r.logger().With("job", "retention", "retention_days", res.RetentionDays, "rows_deleted", res.Deleted,
		"batches", res.Batches, "duration_ms", res.Duration.Milliseconds())
	switch {
	case err != nil:
		log.Error("retention job failed", "error", err.Error())
	case res.Skipped != "":
		log.Info("retention job skipped", "reason", res.Skipped)
	default:
		log.Info("retention job finished")
	}
	return res, err
}

func (r *Retention) run(ctx context.Context) (Result, error) {
	var res Result
	days, err := r.days(ctx)
	if err != nil {
		return res, err
	}
	res.RetentionDays = days
	if days == 0 {
		res.Skipped = "retention is set to keep everything"
		return res, nil
	}
	cutoff := r.now().AddDate(0, 0, -days)
	batch, pause := r.BatchSize, r.Pause
	if batch <= 0 {
		batch = defaultBatch
	}
	if pause <= 0 {
		pause = defaultPause
	}
	// The owner's change log follows the same retention period.
	audit, err := r.DB.Exec(ctx, `DELETE FROM audit_log WHERE ts < $1`, cutoff)
	if err != nil {
		return res, err
	}
	res.AuditDeleted = audit.RowsAffected()
	for {
		tag, err := r.DB.Exec(ctx, `DELETE FROM results WHERE id IN (SELECT id FROM results WHERE ts < $1 ORDER BY ts, id LIMIT $2)`, cutoff, batch)
		if err != nil {
			return res, err
		}
		n := tag.RowsAffected()
		res.Deleted += n
		res.Batches++
		if n < int64(batch) {
			return res, nil
		}
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(pause):
		}
	}
}

// NextRun returns the next occurrence of hour:minute UTC strictly after now.
func NextRun(now time.Time, hour, minute int) time.Time {
	now = now.UTC()
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, time.UTC)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// Loop runs the job every day until ctx ends. A failed run is logged and retried the next day.
func (r *Retention) Loop(ctx context.Context) {
	for {
		wait := NextRun(r.now(), RunHour, RunMinute).Sub(r.now())
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		_, _ = r.Run(ctx)
	}
}
