//go:build integration

package settings

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var errInjected = errors.New("injected failure")

// failingPool fails the Nth database call (counting statements run on the pool and on its transactions).
type failingPool struct {
	pool
	calls  *atomic.Int64
	failAt int64
}

func (f failingPool) hit() bool { return f.calls.Add(1) == f.failAt }

func (f failingPool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if f.hit() {
		return nil, errInjected
	}
	return f.pool.Query(ctx, sql, args...)
}
func (f failingPool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if f.hit() {
		return errRow{}
	}
	return f.pool.QueryRow(ctx, sql, args...)
}
func (f failingPool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if f.hit() {
		return pgconn.CommandTag{}, errInjected
	}
	return f.pool.Exec(ctx, sql, args...)
}
func (f failingPool) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return failingTx{Tx: tx, f: f}, nil
}

type errRow struct{}

func (errRow) Scan(...any) error { return errInjected }

type failingTx struct {
	pgx.Tx
	f failingPool
}

func (t failingTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if t.f.hit() {
		return nil, errInjected
	}
	return t.Tx.Query(ctx, sql, args...)
}
func (t failingTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if t.f.hit() {
		return errRow{}
	}
	return t.Tx.QueryRow(ctx, sql, args...)
}
func (t failingTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if t.f.hit() {
		return pgconn.CommandTag{}, errInjected
	}
	return t.Tx.Exec(ctx, sql, args...)
}

// TestEveryDatabaseFailureIsReportedAndLeavesNoPartialWrite fails each database call of Get, Replace and the key
// rotation in turn.
func TestEveryDatabaseFailureIsReportedAndLeavesNoPartialWrite(t *testing.T) {
	real := settingsPool(t)
	ctx := t.Context()
	base := NewPostgresStore(real, integrationCipher(t), Defaults(""))
	_, etag, err := base.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Saved once so that the revision under test is stable and the secrets exist.
	if _, etag, err = base.Replace(ctx, etag, validInputWithSecrets()); err != nil {
		t.Fatal(err)
	}
	if _, err := base.RotateEnrollmentKey(ctx); err != nil {
		t.Fatal(err)
	}
	if _, etag, err = base.Get(ctx); err != nil {
		t.Fatal(err)
	}
	ops := map[string]func(*PostgresStore) error{
		"get":     func(s *PostgresStore) error { _, _, err := s.Get(ctx); return err },
		"replace": func(s *PostgresStore) error { _, _, err := s.Replace(ctx, etag, validInputWithSecrets()); return err },
		"rotate":  func(s *PostgresStore) error { _, err := s.RotateEnrollmentKey(ctx); return err },
	}
	for name, op := range ops {
		for n := int64(1); n < 60; n++ {
			var calls atomic.Int64
			store := &PostgresStore{pool: failingPool{pool: real, calls: &calls, failAt: n}, cipher: base.cipher, defaults: base.defaults}
			err := op(store)
			if calls.Load() < n {
				// The operation has fewer than n database calls, so every call has been failed once and this
				// run succeeded; a successful replace moved the revision.
				if _, current, getErr := base.Get(ctx); getErr == nil {
					etag = current
				}
				break
			}
			if err == nil {
				t.Fatalf("%s: failing database call %d was swallowed", name, n)
			}
			if _, current, getErr := base.Get(ctx); getErr != nil || current != etag {
				t.Fatalf("%s: failing database call %d left the revision changed (%q -> %q, %v)", name, n, etag, current, getErr)
			}
		}
	}
}
