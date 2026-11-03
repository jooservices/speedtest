package report

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// DefaultBatchSize is the largest batch sent at once; the center accepts at
// most 100 results per request.
const DefaultBatchSize = 100

// Reporter writes each result to the spool before sending, then flushes the
// backlog to the command center in order and in batches, retrying transient
// failures with exponential backoff and jitter. It is safe for concurrent use.
type Reporter struct {
	mu      sync.Mutex
	spool   *Spool
	sender  Sender
	backoff Backoff
	batch   int
	logger  *slog.Logger
	sleep   func(context.Context, time.Duration) error
}

// Option customizes a Reporter.
type Option func(*Reporter)

// WithBatchSize sets the maximum results sent per request (1..100).
func WithBatchSize(n int) Option {
	return func(r *Reporter) { r.batch = n }
}

// WithBackoff replaces the retry schedule.
func WithBackoff(b Backoff) Option {
	return func(r *Reporter) { r.backoff = b }
}

// WithLogger sets the structured logger used for warnings.
func WithLogger(l *slog.Logger) Option {
	return func(r *Reporter) { r.logger = l }
}

// NewReporter builds a reporter over a spool and a center sender.
func NewReporter(spool *Spool, sender Sender, opts ...Option) *Reporter {
	r := &Reporter{
		spool:   spool,
		sender:  sender,
		backoff: DefaultBackoff(),
		batch:   DefaultBatchSize,
		logger:  slog.Default(),
		sleep:   sleepContext,
	}
	for _, o := range opts {
		o(r)
	}
	if r.logger == nil {
		r.logger = slog.Default()
	}
	if r.batch < 1 {
		r.batch = DefaultBatchSize
	}
	if r.batch > DefaultBatchSize {
		r.batch = DefaultBatchSize
	}
	return r
}

// Report durably spools one result, then flushes the backlog. The result is on
// disk before any network attempt, so a crash cannot lose it.
func (r *Reporter) Report(ctx context.Context, res Result) error {
	if _, err := r.Enqueue(res); err != nil {
		return err
	}
	return r.Flush(ctx)
}

// Enqueue durably records a result without waiting for the network. The returned
// UUID identifies it even when a later Flush is canceled. Prefer FromEngine to
// allocate the UUID before invoking this method and retain it on enqueue errors.
func (r *Reporter) Enqueue(res Result) (string, error) {
	if res.ID == "" {
		id, err := newID()
		if err != nil {
			return "", err
		}
		res.ID = id
	}
	return res.ID, r.spool.Append(res)
}

// Flush sends every pending result in order until the spool is empty or a
// fatal (non-retryable) failure occurs. On a fatal failure the unsent results
// stay in the spool for the next attempt.
func (r *Reporter) Flush(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.flush(ctx)
}

func (r *Reporter) flush(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.spool.Prune(); err != nil {
			return err
		}
		batch, peekErr := r.spool.Peek(r.batch)
		if peekErr != nil {
			return peekErr
		}
		if len(batch) == 0 {
			return nil
		}

		batch, err := boundedBatch(batch)
		if err != nil {
			return err
		}
		outcomes, err := r.sender.SendBatch(ctx, batch)
		if err != nil {
			if !Retryable(err) {
				return err
			}
			r.logger.Warn("result delivery failed; retrying",
				"retry", true)
			if sleepErr := r.sleep(ctx, r.backoff.Next()); sleepErr != nil {
				return sleepErr
			}
			continue
		}
		r.backoff.Reset()

		byIndex, err := validateOutcomes(batch, outcomes)
		if err != nil {
			return err
		}

		for i, res := range batch {
			out := byIndex[i]
			switch out.Outcome {
			case "accepted", "duplicate":
				if err := r.spool.Ack(res.ID); err != nil {
					return err
				}
			default:
				if err := r.spool.Quarantine(res.ID, fmt.Sprintf("HTTP %d", out.Status)); err != nil {
					return err
				}
			}
		}
	}
}

// validateOutcomes checks the entire response before acknowledging any item.
func validateOutcomes(batch []Result, outcomes []Outcome) (map[int]Outcome, error) {
	if len(batch) != len(outcomes) {
		return nil, errors.New("center outcome count mismatch")
	}
	byIndex := make(map[int]Outcome, len(outcomes))
	for _, o := range outcomes {
		if o.Index < 0 || o.Index >= len(batch) {
			return nil, errors.New("center outcome index out of range")
		}
		if _, ok := byIndex[o.Index]; ok {
			return nil, errors.New("center repeated outcome index")
		}
		if o.ID == nil || *o.ID != batch[o.Index].ID {
			return nil, errors.New("center outcome UUID mismatch")
		}
		switch o.Outcome {
		case "accepted":
			if o.Status != 201 || o.Problem != nil {
				return nil, errors.New("invalid accepted outcome")
			}
		case "duplicate":
			if o.Status != 200 || o.Problem != nil {
				return nil, errors.New("invalid duplicate outcome")
			}
		case "rejected":
			if (o.Status != 422 && o.Status != 409) || o.Problem == nil {
				return nil, errors.New("invalid rejected outcome")
			}
		default:
			return nil, errors.New("unknown center outcome")
		}
		byIndex[o.Index] = o
	}
	return byIndex, nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// boundedBatch accounts for JSON escaping, commas and the request envelope.
func boundedBatch(batch []Result) ([]Result, error) {
	size := len(`{"results":[]}`)
	for i, r := range batch {
		data, err := json.Marshal(r)
		if err != nil {
			return nil, err
		}
		extra := len(data)
		if i > 0 {
			extra++
		}
		if size+extra > maxBatchBytes {
			if i == 0 {
				return nil, errors.New("spooled result exceeds 8 MiB batch limit")
			}
			return batch[:i], nil
		}
		size += extra
	}
	return batch, nil
}
