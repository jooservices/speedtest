package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

type storeExecOutcome struct {
	rows int64
	err  error
}

type storeTestPool struct {
	queryValues []any
	queryErr    error
	exec        []storeExecOutcome
	beginErr    error
	tx          *storeTestTx
}

func (p *storeTestPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return runStoreExec(&p.exec)
}

func (p *storeTestPool) QueryRow(context.Context, string, ...any) pgx.Row {
	return storeTestRow{values: p.queryValues, err: p.queryErr}
}

func (p *storeTestPool) Begin(context.Context) (pgx.Tx, error) {
	if p.beginErr != nil {
		return nil, p.beginErr
	}
	if p.tx == nil {
		p.tx = &storeTestTx{}
	}
	return p.tx, nil
}

type storeTestTx struct {
	pgx.Tx
	exec      []storeExecOutcome
	commitErr error
}

func (tx *storeTestTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return runStoreExec(&tx.exec)
}

func (tx *storeTestTx) Commit(context.Context) error   { return tx.commitErr }
func (tx *storeTestTx) Rollback(context.Context) error { return nil }

func runStoreExec(outcomes *[]storeExecOutcome) (pgconn.CommandTag, error) {
	if len(*outcomes) == 0 {
		return pgconn.NewCommandTag("UPDATE 1"), nil
	}
	outcome := (*outcomes)[0]
	*outcomes = (*outcomes)[1:]
	if outcome.err != nil {
		return pgconn.CommandTag{}, outcome.err
	}
	return pgconn.NewCommandTag(fmt.Sprintf("UPDATE %d", outcome.rows)), nil
}

type storeTestRow struct {
	values []any
	err    error
}

func (row storeTestRow) Scan(destinations ...any) error {
	if row.err != nil {
		return row.err
	}
	if len(destinations) != len(row.values) {
		return fmt.Errorf("got %d destinations for %d values", len(destinations), len(row.values))
	}
	for index, destination := range destinations {
		pointer := reflect.ValueOf(destination)
		if pointer.Kind() != reflect.Pointer || !pointer.Elem().CanSet() {
			return fmt.Errorf("destination %d is not a settable pointer", index)
		}
		value := reflect.ValueOf(row.values[index])
		if !value.IsValid() {
			pointer.Elem().Set(reflect.Zero(pointer.Elem().Type()))
			continue
		}
		if !value.Type().AssignableTo(pointer.Elem().Type()) {
			return fmt.Errorf("value %d has type %s, destination has type %s", index, value.Type(), pointer.Elem().Type())
		}
		pointer.Elem().Set(value)
	}
	return nil
}

func TestPostgresStoreCreateAndReadSessionResults(t *testing.T) {
	failure := errors.New("query failed")
	if err := (&PostgresStore{pool: &storeTestPool{exec: []storeExecOutcome{{err: failure}}}}).CreateSession(context.Background(), "token", nil, "csrf", time.Now().Add(time.Hour)); !errors.Is(err, failure) {
		t.Fatalf("CreateSession cleanup error = %v", err)
	}
	if err := (&PostgresStore{pool: &storeTestPool{exec: []storeExecOutcome{{rows: 0}, {err: failure}}}}).CreateSession(context.Background(), "token", nil, "csrf", time.Now().Add(time.Hour)); !errors.Is(err, failure) {
		t.Fatalf("CreateSession insert error = %v", err)
	}
	if err := (&PostgresStore{pool: &storeTestPool{}}).CreateSession(context.Background(), "token", nil, "csrf", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CreateSession success error = %v", err)
	}

	store := func(values []any, err error) *PostgresStore {
		return &PostgresStore{pool: &storeTestPool{queryValues: values, queryErr: err}}
	}
	if _, err := store(nil, pgx.ErrNoRows).GetSession(context.Background(), "token", time.Now()); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("GetSession missing error = %v", err)
	}
	if _, err := store(nil, failure).GetSession(context.Background(), "token", time.Now()); !errors.Is(err, failure) {
		t.Fatalf("GetSession query error = %v", err)
	}
	expires := time.Now().Add(time.Hour)
	anonymous, err := store([]any{"csrf-hash", expires, pgtype.Text{}, pgtype.Text{}}, nil).GetSession(context.Background(), "token", time.Now())
	if err != nil || anonymous.Identity != nil || anonymous.CSRFTokenHash != "csrf-hash" {
		t.Fatalf("GetSession anonymous result = %+v, %v", anonymous, err)
	}
	owner, err := store([]any{"csrf-hash", expires, pgtype.Text{String: "owner-id", Valid: true}, pgtype.Text{String: "owner@example.com", Valid: true}}, nil).GetSession(context.Background(), "token", time.Now())
	if err != nil || owner.Identity == nil || owner.Identity.ID != "owner-id" || owner.Identity.Email != "owner@example.com" {
		t.Fatalf("GetSession owner result = %+v, %v", owner, err)
	}
}

func TestPostgresStoreFindAndDeleteResults(t *testing.T) {
	failure := errors.New("query failed")
	storeForRow := func(values []any, err error) *PostgresStore {
		return &PostgresStore{pool: &storeTestPool{queryValues: values, queryErr: err}}
	}
	if _, err := storeForRow(nil, pgx.ErrNoRows).FindUserByEmail(context.Background(), "missing@example.test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("FindUser missing email error = %v", err)
	}
	if _, err := storeForRow(nil, failure).FindUserByID(context.Background(), "owner-id"); !errors.Is(err, failure) {
		t.Fatalf("FindUser query error = %v", err)
	}
	credential, err := storeForRow([]any{"owner-id", "owner@example.com", "argon2id-hash"}, nil).FindUserByEmail(context.Background(), "owner@example.com")
	if err != nil || credential.ID != "owner-id" || credential.Email != "owner@example.com" || credential.PasswordHash != "argon2id-hash" {
		t.Fatalf("FindUser result = %+v, %v", credential, err)
	}

	if err := (&PostgresStore{pool: &storeTestPool{exec: []storeExecOutcome{{err: failure}}}}).DeleteSession(context.Background(), "token"); !errors.Is(err, failure) {
		t.Fatalf("DeleteSession query error = %v", err)
	}
	if err := (&PostgresStore{pool: &storeTestPool{exec: []storeExecOutcome{{rows: 0}}}}).DeleteSession(context.Background(), "token"); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("DeleteSession missing error = %v", err)
	}
	if err := (&PostgresStore{pool: &storeTestPool{exec: []storeExecOutcome{{rows: 1}}}}).DeleteSession(context.Background(), "token"); err != nil {
		t.Fatalf("DeleteSession success error = %v", err)
	}
}

func TestPostgresStoreRotationTransactionOutcomes(t *testing.T) {
	failure := errors.New("query failed")
	tests := []struct {
		name       string
		pool       *storeTestPool
		want       error
		wantCommit bool
	}{
		{name: "begin error", pool: &storeTestPool{beginErr: failure}, want: failure},
		{name: "delete error", pool: &storeTestPool{tx: &storeTestTx{exec: []storeExecOutcome{{err: failure}}}}, want: failure},
		{name: "stale anonymous session", pool: &storeTestPool{tx: &storeTestTx{exec: []storeExecOutcome{{rows: 0}}}}, want: ErrInvalidSession},
		{name: "insert error", pool: &storeTestPool{tx: &storeTestTx{exec: []storeExecOutcome{{rows: 1}, {err: failure}}}}, want: failure},
		{name: "commit error", pool: &storeTestPool{tx: &storeTestTx{exec: []storeExecOutcome{{rows: 1}, {rows: 1}}, commitErr: failure}}, want: failure, wantCommit: true},
		{name: "success", pool: &storeTestPool{tx: &storeTestTx{exec: []storeExecOutcome{{rows: 1}, {rows: 1}}}}, wantCommit: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := (&PostgresStore{pool: test.pool}).RotateAnonymousSession(context.Background(), "old", "owner-id", "new", "csrf", time.Now().Add(time.Hour))
			if !errors.Is(err, test.want) {
				t.Fatalf("RotateAnonymousSession() error = %v, want %v", err, test.want)
			}
			if test.wantCommit && test.pool.tx.commitErr == nil && err != nil {
				t.Fatalf("successful transaction returned %v", err)
			}
		})
	}
}

func TestPostgresStorePasswordTransactionOutcomes(t *testing.T) {
	failure := errors.New("query failed")
	base := func(exec []storeExecOutcome, beginErr, commitErr error) *storeTestPool {
		return &storeTestPool{beginErr: beginErr, tx: &storeTestTx{exec: exec, commitErr: commitErr}}
	}
	tests := []struct {
		name      string
		pool      *storeTestPool
		want      error
		wantError bool
	}{
		{name: "begin error", pool: base(nil, failure, nil), want: failure, wantError: true},
		{name: "update error", pool: base([]storeExecOutcome{{err: failure}}, nil, nil), want: failure, wantError: true},
		{name: "stale password hash", pool: base([]storeExecOutcome{{rows: 0}}, nil, nil), want: ErrInvalidCurrentPass, wantError: true},
		{name: "delete other sessions error", pool: base([]storeExecOutcome{{rows: 1}, {err: failure}}, nil, nil), want: failure, wantError: true},
		{name: "session rotation error", pool: base([]storeExecOutcome{{rows: 1}, {rows: 1}, {err: failure}}, nil, nil), want: failure, wantError: true},
		{name: "stale session", pool: base([]storeExecOutcome{{rows: 1}, {rows: 1}, {rows: 0}}, nil, nil), want: ErrInvalidSession, wantError: true},
		{name: "commit error", pool: base([]storeExecOutcome{{rows: 1}, {rows: 1}, {rows: 1}}, nil, failure), want: failure, wantError: true},
		{name: "success", pool: base([]storeExecOutcome{{rows: 1}, {rows: 1}, {rows: 1}}, nil, nil)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := (&PostgresStore{pool: test.pool}).ChangePassword(context.Background(), "owner-id", "old", "new", "old-session", "new-session", "csrf", time.Now().Add(time.Hour))
			if test.wantError && !errors.Is(err, test.want) {
				t.Fatalf("ChangePassword() error = %v, want %v", err, test.want)
			}
			if !test.wantError && err != nil {
				t.Fatalf("ChangePassword() error = %v", err)
			}
		})
	}
}

func TestPostgresStoreSeedAdminReportsRandomAndDatabaseFailures(t *testing.T) {
	original := rand.Reader
	defer func() { rand.Reader = original }()
	store := &PostgresStore{pool: &storeTestPool{}}
	rand.Reader = failingRandomReader{}
	if err := store.SeedAdmin(context.Background(), DefaultAdminEmail, "hash"); err == nil {
		t.Fatal("SeedAdmin accepted an unavailable random source")
	}
	rand.Reader = original
	failure := errors.New("insert failed")
	if err := (&PostgresStore{pool: &storeTestPool{exec: []storeExecOutcome{{err: failure}}}}).SeedAdmin(context.Background(), DefaultAdminEmail, "hash"); !errors.Is(err, failure) {
		t.Fatalf("SeedAdmin insert error = %v", err)
	}
	if err := (&PostgresStore{pool: &storeTestPool{exec: []storeExecOutcome{{rows: 1}}}}).SeedAdmin(context.Background(), DefaultAdminEmail, "hash"); err != nil {
		t.Fatalf("SeedAdmin success error = %v", err)
	}
}
