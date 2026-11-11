package audit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestClassify(t *testing.T) {
	const id = "10000000-0000-4000-8000-000000000001"
	cases := []struct {
		method, path, action, target string
		ok                           bool
	}{
		{"POST", "/api/nodes", "node.create", "", true},
		{"PATCH", "/api/nodes/" + id, "node.update", id, true},
		{"DELETE", "/api/nodes/" + id, "node.delete", id, true},
		{"POST", "/api/nodes/" + id + "/run", "node.run", id, true},
		{"PUT", "/api/schedules/rules", "schedule.rules", "", true}, // the literal route wins over /api/schedules/{id}
		{"PUT", "/api/schedules/" + id, "schedule.update", id, true},
		{"POST", "/api/schedules/stagger", "schedule.stagger", "", true},
		{"PUT", "/api/settings", "settings.update", "", true},
		{"POST", "/api/settings/enrolment-key", "settings.enrolment_key", "", true},
		{"POST", "/api/auth/password", "auth.password", "", true},
		{"PUT", "/api/alerts/events/" + id + "/ack", "alert.ack", id, true},
		{"POST", "/api/notifications/test/webhook", "notification.test", "webhook", true},
		{"POST", "/api/nodes/", "node.create", "", true}, // trailing slash
		{"GET", "/api/nodes", "", "", false},
		{"POST", "/api/agent/results", "", "", false},
		{"POST", "/api/auth/login", "", "", false},
		{"POST", "/somewhere/else", "", "", false},
		{"POST", "/api/unknown", "", "", false},
	}
	for _, c := range cases {
		got, ok := Classify(c.method, c.path)
		if ok != c.ok || got.Action != c.action || got.TargetID != c.target {
			t.Errorf("Classify(%s %s) = %+v, %v; want %s %q %v", c.method, c.path, got, ok, c.action, c.target, c.ok)
		}
		if ok && (got.Summary == "" || got.TargetType == "") {
			t.Errorf("Classify(%s %s) needs a summary and target type: %+v", c.method, c.path, got)
		}
	}
}

func TestEscapeLikeAndID(t *testing.T) {
	if got := escapeLike(`a%b_c\d`); got != `a\%b\_c\\d` {
		t.Fatalf("escapeLike = %q", got)
	}
	if id := newID(); len(id) != 36 || id[14] != '4' {
		t.Fatalf("newID = %q", id)
	}
}

func TestRecordRejectsIncompleteEntries(t *testing.T) {
	s := &Store{}
	if err := s.Record(t.Context(), Entry{Action: "x"}); err == nil {
		t.Fatal("an entry without an actor was accepted")
	}
	if err := s.Record(t.Context(), Entry{Actor: "a"}); err == nil {
		t.Fatal("an entry without an action was accepted")
	}
	if err := (&Store{DB: &storeTestPool{}}).Record(t.Context(), Entry{Actor: "a", Action: "x", AuthMethod: "basic"}); err == nil {
		t.Fatal("an unsupported authentication method was accepted")
	}
}

type storeTestPool struct {
	execErr, rowErr, queryErr error
	count                     int
	rows                      pgx.Rows
	execSQL, querySQL         string
	execArgs, queryArgs       []any
}

func (p *storeTestPool) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	p.execSQL, p.execArgs = sql, args
	return pgconn.CommandTag{}, p.execErr
}

func (p *storeTestPool) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	p.querySQL, p.queryArgs = sql, args
	if p.queryErr != nil {
		return nil, p.queryErr
	}
	return p.rows, nil
}

func (p *storeTestPool) QueryRow(context.Context, string, ...any) pgx.Row {
	return storeTestRow{count: p.count, err: p.rowErr}
}

type storeTestRow struct {
	count int
	err   error
}

func (r storeTestRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	count, ok := dest[0].(*int)
	if !ok {
		return errors.New("unexpected count destination")
	}
	*count = r.count
	return nil
}

type storeTestRows struct {
	entry   Entry
	count   int
	index   int
	scanErr error
	rowsErr error
}

func (r *storeTestRows) Close()                                       {}
func (r *storeTestRows) Err() error                                   { return r.rowsErr }
func (r *storeTestRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *storeTestRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *storeTestRows) Values() ([]any, error)                       { return nil, nil }
func (r *storeTestRows) RawValues() [][]byte                          { return nil }
func (r *storeTestRows) Conn() *pgx.Conn                              { return nil }
func (r *storeTestRows) TypeMap() *pgtype.Map                         { return nil }

func (r *storeTestRows) Next() bool {
	if r.index >= r.count {
		return false
	}
	r.index++
	return true
}

func (r *storeTestRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	values := []any{r.entry.ID, r.entry.TS, r.entry.Actor, r.entry.AuthMethod, r.entry.CredentialID,
		r.entry.Action, r.entry.TargetType, r.entry.TargetID, r.entry.Summary, r.entry.Remote}
	for i, target := range dest {
		switch value := target.(type) {
		case *string:
			*value = values[i].(string)
		case *time.Time:
			*value = values[i].(time.Time)
		default:
			return errors.New("unexpected audit row destination")
		}
	}
	return nil
}

func TestRecordStoresAuthenticationMetadataAndBoundsSummary(t *testing.T) {
	zone := time.FixedZone("test", 7*60*60)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, zone)
	pool := &storeTestPool{}
	store := &Store{DB: pool, Now: func() time.Time { return now }}
	entry := Entry{Actor: "owner@example.test", Action: "node.update", Summary: strings.Repeat("x", 350)}
	if err := store.Record(context.Background(), entry); err != nil {
		t.Fatal("record session audit entry:", err)
	}
	if !strings.Contains(pool.execSQL, "auth_method,credential_id") || pool.execArgs[8] != AuthMethodSession || pool.execArgs[9] != "" {
		t.Fatalf("session audit insert = %q args %#v", pool.execSQL, pool.execArgs)
	}
	if got := pool.execArgs[1].(time.Time); !got.Equal(now) || got.Location() != time.UTC {
		t.Fatalf("stored timestamp = %v, want UTC instant %v", got, now)
	}
	if len(pool.execArgs[6].(string)) != 300 {
		t.Fatalf("stored summary length = %d, want 300", len(pool.execArgs[6].(string)))
	}
	entry.AuthMethod, entry.CredentialID = AuthMethodBearer, "10000000-0000-4000-8000-000000000201"
	if err := store.Record(context.Background(), entry); err != nil || pool.execArgs[8] != AuthMethodBearer || pool.execArgs[9] != entry.CredentialID {
		t.Fatalf("record bearer audit metadata: args=%#v error=%v", pool.execArgs, err)
	}
	pool.execErr = errors.New("write failed")
	if err := store.Record(context.Background(), entry); err == nil {
		t.Fatal("database write failure was hidden")
	}
	pool.execErr = nil
	store.Now = nil
	entry.Summary = "short"
	if err := store.Record(context.Background(), entry); err != nil || len(pool.execArgs[6].(string)) != len(entry.Summary) {
		t.Fatalf("record without custom clock or summary truncation: args=%#v error=%v", pool.execArgs, err)
	}
}

func TestListReadsAuthenticationMetadataAndHandlesErrors(t *testing.T) {
	entry := Entry{ID: "10000000-0000-4000-8000-000000000202", TS: time.Now(), Actor: "owner@example.test",
		AuthMethod: AuthMethodBearer, CredentialID: "10000000-0000-4000-8000-000000000201", Action: "node.update",
		TargetType: "node", TargetID: "node-1", Summary: "Changed node", Remote: "198.51.100.10"}
	rows := &storeTestRows{entry: entry, count: 1}
	pool := &storeTestPool{count: 1, rows: rows}
	store := &Store{DB: pool}
	items, total, err := store.List(context.Background(), Filter{Action: "node_%"}, 2, 5)
	if err != nil || total != 1 || len(items) != 1 || items[0].CredentialID != entry.CredentialID || items[0].AuthMethod != AuthMethodBearer {
		t.Fatalf("list audit metadata = %+v total %d error %v", items, total, err)
	}
	if !strings.Contains(pool.querySQL, "coalesce(credential_id::text,'')") || pool.queryArgs[0] != `node\_\%%` || pool.queryArgs[1] != 5 || pool.queryArgs[2] != 5 {
		t.Fatalf("filtered list query = %q args %#v", pool.querySQL, pool.queryArgs)
	}
	pool.rowErr = errors.New("count failed")
	if _, _, err := store.List(context.Background(), Filter{}, 1, 1); err == nil {
		t.Fatal("count failure was hidden")
	}
	pool.rowErr, pool.queryErr = nil, errors.New("query failed")
	if _, _, err := store.List(context.Background(), Filter{}, 1, 1); err == nil {
		t.Fatal("list query failure was hidden")
	}
	pool.queryErr, pool.rows = nil, &storeTestRows{count: 1, scanErr: errors.New("scan failed")}
	if _, _, err := store.List(context.Background(), Filter{}, 1, 1); err == nil {
		t.Fatal("row scan failure was hidden")
	}
	pool.rows = &storeTestRows{rowsErr: errors.New("read failed")}
	if _, _, err := store.List(context.Background(), Filter{}, 1, 1); err == nil {
		t.Fatal("row iteration failure was hidden")
	}
	pool.rows = &storeTestRows{}
	items, total, err = store.List(context.Background(), Filter{}, 3, 10)
	if err != nil || total != 1 || items == nil || len(items) != 0 {
		t.Fatalf("empty page = %+v total %d error %v", items, total, err)
	}
}
