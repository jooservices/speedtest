package agentapi

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type testRow struct {
	values []any
	err    error
}

func (r testRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, value := range r.values {
		reflect.ValueOf(dest[i]).Elem().Set(reflect.ValueOf(value))
	}
	return nil
}

type testRows struct {
	pgx.Rows
	rows  []testRow
	index int
	err   error
}

func (r *testRows) Next() bool             { r.index++; return r.index <= len(r.rows) }
func (r *testRows) Scan(dest ...any) error { return r.rows[r.index-1].Scan(dest...) }
func (r *testRows) Close()                 {}
func (r *testRows) Err() error             { return r.err }

type testDB struct {
	Database
	beginErr, queryErr error
	tx                 *testTx
	row                pgx.Row
	rows               pgx.Rows
}

func (d testDB) Begin(context.Context) (pgx.Tx, error)                   { return d.tx, d.beginErr }
func (d testDB) QueryRow(context.Context, string, ...any) pgx.Row        { return d.row }
func (d testDB) Query(context.Context, string, ...any) (pgx.Rows, error) { return d.rows, d.queryErr }

type testTx struct {
	pgx.Tx
	execErrors []error
	execs      []string
	execIndex  int
	inserted   bool
	row        pgx.Row
	commitErr  error
	rollback   bool
}

func (t *testTx) Exec(_ context.Context, query string, _ ...any) (pgconn.CommandTag, error) {
	t.execs = append(t.execs, query)
	index := t.execIndex
	t.execIndex++
	if index < len(t.execErrors) && t.execErrors[index] != nil {
		return pgconn.CommandTag{}, t.execErrors[index]
	}
	n := "0"
	if t.inserted {
		n = "1"
	}
	return pgconn.NewCommandTag("INSERT 0 " + n), nil
}
func (t *testTx) QueryRow(context.Context, string, ...any) pgx.Row { return t.row }
func (t *testTx) Commit(context.Context) error                     { return t.commitErr }
func (t *testTx) Rollback(context.Context) error                   { t.rollback = true; return nil }

type testHook struct {
	called int
	err    error
}

func (h *testHook) AfterResultStored(context.Context, pgx.Tx, string, Result) error {
	h.called++
	return h.err
}
func commandRow(status string) testRow {
	return testRow{values: []any{"00000000-0000-4000-8000-000000000001", "node", "run_now", (*string)(nil), status, time.Now(), (*time.Time)(nil), "", float64(0), (*string)(nil), (*string)(nil)}}
}
func TestPostgresSaveFailuresAndHook(t *testing.T) {
	ctx := t.Context()
	failure := errors.New("database unavailable")
	r, err := ParseResult([]byte(validResult))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		db        testDB
		hook      *testHook
		want      error
		duplicate bool
	}{
		{"begin", testDB{beginErr: failure}, nil, failure, false},
		{"node lock", testDB{tx: &testTx{execErrors: []error{failure}}}, nil, failure, false},
		{"ip insert", testDB{tx: &testTx{execErrors: []error{nil, failure}}}, nil, failure, false},
		{"result insert", testDB{tx: &testTx{execErrors: []error{nil, nil, failure}}}, nil, failure, false},
		{"compare query", testDB{tx: &testTx{row: testRow{err: failure}}}, nil, failure, false},
		{"conflict", testDB{tx: &testTx{row: testRow{values: []any{false}}}}, nil, ErrConflict, false},
		{"duplicate", testDB{tx: &testTx{row: testRow{values: []any{true}}}}, &testHook{}, nil, true},
		{"insert and hook", testDB{tx: &testTx{inserted: true}}, &testHook{}, nil, false},
		{"hook rollback", testDB{tx: &testTx{inserted: true}}, &testHook{err: failure}, failure, false},
		{"commit", testDB{tx: &testTx{inserted: true, commitErr: failure}}, nil, failure, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Postgres{Pool: tc.db}
			if tc.hook != nil {
				s.Hook = tc.hook
			}
			duplicate, err := s.SaveResult(ctx, "node", r)
			if !errors.Is(err, tc.want) || duplicate != tc.duplicate {
				t.Fatal(duplicate, err)
			}
			if tc.hook != nil {
				want := 0
				if tc.db.tx.inserted {
					want = 1
				}
				if tc.hook.called != want {
					t.Fatal("hook invocation", tc.hook.called)
				}
			}
		})
	}
	if jsonValue(nil) != nil {
		t.Fatal("nil JSON")
	}
}
func TestPostgresSaveNullIPSkipsIPHistoryWork(t *testing.T) {
	errorMessage := "speedtest: no_route"
	tx := &testTx{inserted: true}
	hook := &testHook{}
	store := &Postgres{Pool: testDB{tx: tx}, Hook: hook}
	r := Result{ID: "00000000-0000-4000-8000-000000000001", TS: "2026-11-03T10:00:00Z", Status: "failed", Error: &errorMessage, Threads: 1, Raw: []byte(`{}`)}
	duplicate, err := store.SaveResult(t.Context(), "node", r)
	if err != nil || duplicate {
		t.Fatal(duplicate, err)
	}
	if len(tx.execs) != 1 || !strings.HasPrefix(tx.execs[0], "INSERT INTO results") {
		t.Fatal("NULL IP performed address or node-lock work", tx.execs)
	}
	if hook.called != 0 {
		t.Fatal("NULL IP invoked the history hook", hook.called)
	}
}
func TestPostgresConfigAndHeartbeat(t *testing.T) {
	base := []any{"node", "edge", float64(300), float64(100), 4, (*int)(nil), "", (*string)(nil), false, int64(1), 30, 0, true, int64(1), []byte(`{}`)}
	s := &Postgres{Pool: testDB{row: testRow{values: base}}}
	cfg, err := s.Config(t.Context(), "node")
	if err != nil || cfg["server"] != "auto" || cfg["engine"] != "ookla" {
		t.Fatal(cfg, err)
	}
	pinned := 42
	base[5] = &pinned
	base[14] = []byte(`{"engine":"librespeed"}`)
	cfg, err = s.Config(t.Context(), "node")
	if err != nil || cfg["server"] != 42 || cfg["engine"] != "librespeed" {
		t.Fatal(cfg, err)
	}
	base[14] = []byte(`not JSON`)
	if _, err = s.Config(t.Context(), "node"); err == nil {
		t.Fatal("bad settings accepted")
	}
	for _, failure := range []error{pgx.ErrNoRows, errors.New("database unavailable")} {
		s.Pool = testDB{row: testRow{err: failure}}
		want := failure
		if errors.Is(failure, pgx.ErrNoRows) {
			want = ErrNotFound
		}
		if _, err = s.Config(t.Context(), "node"); !errors.Is(err, want) {
			t.Fatal(err)
		}
		if _, err = s.Heartbeat(t.Context(), "node", Heartbeat{}); !errors.Is(err, want) {
			t.Fatal(err)
		}
	}
	now := time.Now()
	s.Pool = testDB{row: testRow{values: []any{now}}}
	if received, err := s.Heartbeat(t.Context(), "node", Heartbeat{Agent: "v1"}); err != nil || !received.Equal(now) {
		t.Fatal(received, err)
	}
}
func TestPostgresCommandsAndAckFailures(t *testing.T) {
	failure := errors.New("database unavailable")
	for _, tc := range []struct {
		db     testDB
		want   error
		length int
	}{
		{testDB{queryErr: failure}, failure, 0},
		{testDB{rows: &testRows{}}, nil, 0},
		{testDB{rows: &testRows{rows: []testRow{commandRow("pending")}}}, nil, 1},
		{testDB{rows: &testRows{rows: []testRow{{err: failure}}}}, failure, 0},
		{testDB{rows: &testRows{err: failure}}, failure, 0},
	} {
		commands, err := (&Postgres{Pool: tc.db}).Commands(t.Context(), "node")
		if !errors.Is(err, tc.want) || len(commands) != tc.length {
			t.Fatal(commands, err)
		}
	}
	for _, tc := range []struct {
		db   testDB
		ack  Ack
		want error
	}{
		{testDB{beginErr: failure}, Ack{}, failure},
		{testDB{tx: &testTx{row: testRow{err: pgx.ErrNoRows}}}, Ack{}, ErrNotFound},
		{testDB{tx: &testTx{row: testRow{err: failure}}}, Ack{}, failure},
		{testDB{tx: &testTx{row: commandRow("succeeded")}}, Ack{Status: "failed"}, ErrConflict},
		{testDB{tx: &testTx{row: commandRow("succeeded")}}, Ack{Status: "succeeded"}, nil},
		{testDB{tx: &testTx{row: commandRow("pending")}}, Ack{Status: "running"}, nil},
		{testDB{tx: &testTx{row: commandRow("pending"), commitErr: failure}}, Ack{Status: "running"}, failure},
	} {
		_, err := (&Postgres{Pool: tc.db}).Ack(t.Context(), "node", "id", tc.ack)
		if !errors.Is(err, tc.want) {
			t.Fatal(err)
		}
	}
	// The first row succeeds and the UPDATE can fail independently.
	for _, failure := range []error{&pgconn.PgError{Code: "23503"}, errors.New("update failed")} {
		tx := &sequencedTx{testTx: testTx{}, rows: []pgx.Row{commandRow("pending"), testRow{err: failure}}}
		_, err := (&Postgres{Pool: sequenceDB{tx: tx}}).Ack(t.Context(), "node", "id", Ack{Status: "running"})
		want := failure
		var pgErr *pgconn.PgError
		if errors.As(failure, &pgErr) {
			want = ErrInvalid
		}
		if !errors.Is(err, want) {
			t.Fatal(err)
		}
	}
	now := time.Now()
	row := commandRow("succeeded")
	row.values[6] = &now
	if c, err := scanCommand(row); err != nil || c.AckedAt.Location() != time.UTC {
		t.Fatal(c, err)
	}
}

type sequencedTx struct {
	testTx
	rows  []pgx.Row
	index int
}

func (t *sequencedTx) QueryRow(context.Context, string, ...any) pgx.Row {
	row := t.rows[t.index]
	t.index++
	return row
}

type sequenceDB struct {
	Database
	tx pgx.Tx
}

func (d sequenceDB) Begin(context.Context) (pgx.Tx, error) { return d.tx, nil }
