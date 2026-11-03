package jobs

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Job statuses, as in the Job contract.
const (
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// ErrNotFound reports an unknown job.
var ErrNotFound = errors.New("job not found")

// Execer is what a job needs from a connection or transaction.
type Execer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Start records a new running job and returns its ID.
func Start(ctx context.Context, q Execer, kind string) (string, error) {
	var id string
	err := q.QueryRow(ctx, `INSERT INTO maintenance_jobs (id, kind, status) VALUES (gen_random_uuid(), $1, 'running') RETURNING id::text`, kind).Scan(&id)
	return id, err
}

// Finish marks a job succeeded, or failed with a short message when err is not nil.
func Finish(ctx context.Context, q Execer, id string, err error) error {
	status, msg := StatusSucceeded, (*string)(nil)
	if err != nil {
		status = StatusFailed
		text := "the job failed; see the center log"
		msg = &text
	}
	_, execErr := q.Exec(ctx, `UPDATE maintenance_jobs SET status=$2, error=$3, finished_at=now() WHERE id=$1`, id, status, msg)
	return execErr
}

// Registry reads job status for the dashboard.
type Registry struct {
	DB interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	}
}

// Get returns the Job resource of the contract: id, status and error.
func (r *Registry) Get(ctx context.Context, id string) (map[string]any, error) {
	var status string
	var msg *string
	err := r.DB.QueryRow(ctx, `SELECT status, error FROM maintenance_jobs WHERE id = $1::uuid`, id).Scan(&status, &msg)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var errValue any
	if msg != nil {
		errValue = *msg
	}
	return map[string]any{"id": id, "status": status, "error": errValue}, nil
}
