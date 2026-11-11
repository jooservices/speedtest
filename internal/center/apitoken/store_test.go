package apitoken

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

type fakeDB struct {
	row     func(string, ...any) pgx.Row
	rows    func(string, ...any) (pgx.Rows, error)
	exec    func(string, ...any) (pgconn.CommandTag, error)
	queries int
}

func (db *fakeDB) QueryRow(_ context.Context, query string, args ...any) pgx.Row {
	db.queries++
	return db.row(query, args...)
}
func (db *fakeDB) Query(_ context.Context, query string, args ...any) (pgx.Rows, error) {
	db.queries++
	return db.rows(query, args...)
}
func (db *fakeDB) Exec(_ context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	db.queries++
	return db.exec(query, args...)
}

type scanRow func(...any) error

func (row scanRow) Scan(dest ...any) error { return row(dest...) }

type fakeRows struct {
	rows   [][]any
	index  int
	closed bool
	err    error
}

func (rows *fakeRows) Close()                                       { rows.closed = true }
func (rows *fakeRows) Err() error                                   { return rows.err }
func (rows *fakeRows) CommandTag() pgconn.CommandTag                { return pgconn.NewCommandTag("SELECT 0") }
func (rows *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (rows *fakeRows) Next() bool {
	if rows.index >= len(rows.rows) {
		rows.closed = true
		return false
	}
	rows.index++
	return true
}
func (rows *fakeRows) Scan(dest ...any) error {
	values := rows.rows[rows.index-1]
	if len(dest) != len(values) {
		return errors.New("scan destination count mismatch")
	}
	for i, value := range values {
		if err := assignScan(dest[i], value); err != nil {
			return err
		}
	}
	return nil
}
func (*fakeRows) Values() ([]any, error) { return nil, nil }
func (*fakeRows) RawValues() [][]byte    { return nil }
func (*fakeRows) Conn() *pgx.Conn        { return nil }
func (*fakeRows) TypeMap() *pgtype.Map   { return nil }

func assignScan(destination, source any) error {
	value := reflect.ValueOf(destination)
	if value.Kind() != reflect.Pointer || value.IsNil() {
		return errors.New("scan destination must be a non-nil pointer")
	}
	value = value.Elem()
	if source == nil {
		value.Set(reflect.Zero(value.Type()))
		return nil
	}
	sourceValue := reflect.ValueOf(source)
	if sourceValue.Type().AssignableTo(value.Type()) {
		value.Set(sourceValue)
		return nil
	}
	if value.Kind() == reflect.Pointer && sourceValue.Type().AssignableTo(value.Type().Elem()) {
		pointer := reflect.New(value.Type().Elem())
		pointer.Elem().Set(sourceValue)
		value.Set(pointer)
		return nil
	}
	return errors.New("unsupported scan assignment")
}

func TestStoreCreateAndValidationFailures(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	db := &fakeDB{row: func(query string, args ...any) pgx.Row {
		if !strings.Contains(query, "INSERT INTO api_access_tokens") || len(args) != 6 {
			t.Fatalf("unexpected create SQL or args: %q, %d", query, len(args))
		}
		rawHash, ok := args[4].([]byte)
		if !ok || len(rawHash) != 32 {
			t.Fatalf("stored hash arg = %#v", args[4])
		}
		return scanRow(func(dest ...any) error { *dest[0].(*time.Time) = now; return nil })
	}}
	store := Store{DB: db, Now: func() time.Time { return now }}
	token, raw, err := store.Create(context.Background(), "owner", CreateInput{Name: "  automation ", Scope: ReadWrite})
	if err != nil || token.Name != "automation" || token.Scope != ReadWrite || token.CreatedAt != now || !strings.HasPrefix(raw, tokenPrefix) {
		t.Fatalf("Create() = (%+v, %q, %v)", token, raw[:min(len(raw), 12)], err)
	}
	if db.queries != 1 {
		t.Fatalf("query count = %d, want 1", db.queries)
	}
	db.queries = 0
	if _, _, err := store.Create(context.Background(), "owner", CreateInput{Name: "bad", Scope: "unknown"}); err == nil || db.queries != 0 {
		t.Fatalf("invalid Create() error = %v, query count = %d", err, db.queries)
	}
}

func TestStoreListAndRevoke(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	expires := now.Add(time.Hour)
	db := &fakeDB{
		row: func(query string, args ...any) pgx.Row {
			if !strings.Contains(query, "count(*)") || args[0] != "owner" {
				t.Fatalf("unexpected count query: %q", query)
			}
			return scanRow(func(dest ...any) error { *dest[0].(*int) = 1; return nil })
		},
		rows: func(query string, args ...any) (pgx.Rows, error) {
			if !strings.Contains(query, "api_access_tokens") || args[0] != "owner" || args[1] != 20 || args[2] != 0 {
				t.Fatalf("unexpected token list query: %q, %#v", query, args)
			}
			return &fakeRows{rows: [][]any{{"token-id", "automation", ReadOnly, now, &expires, nil, nil}}}, nil
		},
		exec: func(query string, args ...any) (pgconn.CommandTag, error) {
			if !strings.Contains(query, "UPDATE api_access_tokens SET revoked_at") || args[0] != "token-id" || args[1] != "owner" {
				t.Fatalf("unexpected revoke query: %q, %#v", query, args)
			}
			return pgconn.NewCommandTag("UPDATE 1"), nil
		},
	}
	store := Store{DB: db}
	items, total, err := store.List(context.Background(), "owner", 1, 20)
	if err != nil || total != 1 || len(items) != 1 || items[0].Name != "automation" || items[0].ExpiresAt == nil || !items[0].ExpiresAt.Equal(expires) || items[0].LastUsedAt != nil {
		t.Fatalf("List() = (%+v, %d, %v)", items, total, err)
	}
	if err := store.Revoke(context.Background(), "owner", "token-id"); err != nil {
		t.Fatal(err)
	}
	db.exec = func(string, ...any) (pgconn.CommandTag, error) { return pgconn.NewCommandTag("UPDATE 0"), nil }
	if err := store.Revoke(context.Background(), "owner", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Revoke() error = %v", err)
	}
	db.exec = func(string, ...any) (pgconn.CommandTag, error) {
		return pgconn.CommandTag{}, errors.New("database error")
	}
	if err := store.Revoke(context.Background(), "owner", "token-id"); err == nil {
		t.Fatal("database failure was ignored")
	}
}

func TestStoreAuthenticateChecksHashExpiryAndLastUse(t *testing.T) {
	id, raw, hash, err := issue()
	if err != nil {
		t.Fatal(err)
	}
	principal := Principal{TokenID: id, UserID: "owner-id", Name: "automation", Email: "owner@example.test", Scope: ReadOnly}
	updated := false
	db := &fakeDB{
		row: func(query string, args ...any) pgx.Row {
			if !strings.Contains(query, "JOIN users") {
				t.Fatalf("unexpected authentication query: %q", query)
			}
			return scanRow(func(dest ...any) error {
				*dest[0].(*[]byte) = hash[:]
				*dest[1].(*string) = principal.TokenID
				*dest[2].(*string) = principal.UserID
				*dest[3].(*string) = principal.Name
				*dest[4].(*Scope) = principal.Scope
				*dest[5].(*string) = principal.Email
				return nil
			})
		},
		exec: func(query string, args ...any) (pgconn.CommandTag, error) {
			if !strings.Contains(query, "SET last_used_at") || args[0] != principal.TokenID || !reflect.DeepEqual(args[1], hash[:]) {
				t.Fatalf("unexpected last-use query: %q, %#v", query, args)
			}
			updated = true
			return pgconn.NewCommandTag("UPDATE 1"), nil
		},
	}
	got, err := (&Store{DB: db}).Authenticate(context.Background(), raw)
	if err != nil || got != principal || !updated {
		t.Fatalf("Authenticate() = (%+v, %v), last-used update=%t", got, err, updated)
	}
	db.row = func(string, ...any) pgx.Row { return scanRow(func(...any) error { return pgx.ErrNoRows }) }
	if _, err := (&Store{DB: db}).Authenticate(context.Background(), raw); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("unknown token error = %v", err)
	}
	db.row = func(string, ...any) pgx.Row {
		return scanRow(func(...any) error { return errors.New("database error") })
	}
	if _, err := (&Store{DB: db}).Authenticate(context.Background(), raw); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("database error = %v", err)
	}
	if _, err := (&Store{DB: db}).Authenticate(context.Background(), "malformed"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("malformed token error = %v", err)
	}
}

func TestStoreCreateAndListDatabaseFailures(t *testing.T) {
	store := &Store{DB: &fakeDB{row: func(string, ...any) pgx.Row {
		return scanRow(func(...any) error { return errors.New("insert failed") })
	}}}
	expires := time.Now().Add(time.Hour)
	if _, _, err := store.Create(context.Background(), "owner", CreateInput{Name: "automation", Scope: ReadOnly, ExpiresAt: &expires}); err == nil {
		t.Fatal("Create ignored insert failure")
	}
	countFailure := &fakeDB{row: func(string, ...any) pgx.Row {
		return scanRow(func(...any) error { return errors.New("count failed") })
	}}
	if _, _, err := (&Store{DB: countFailure}).List(context.Background(), "owner", 1, 20); err == nil {
		t.Fatal("List ignored count failure")
	}
	queryFailure := &fakeDB{
		row: func(string, ...any) pgx.Row {
			return scanRow(func(dest ...any) error { *dest[0].(*int) = 1; return nil })
		},
		rows: func(string, ...any) (pgx.Rows, error) { return nil, errors.New("query failed") },
	}
	if _, _, err := (&Store{DB: queryFailure}).List(context.Background(), "owner", 1, 20); err == nil {
		t.Fatal("List ignored page query failure")
	}
	scanFailure := &fakeDB{
		row: func(string, ...any) pgx.Row {
			return scanRow(func(dest ...any) error { *dest[0].(*int) = 1; return nil })
		},
		rows: func(string, ...any) (pgx.Rows, error) { return &fakeRows{rows: [][]any{{"too-few-values"}}}, nil },
	}
	if _, _, err := (&Store{DB: scanFailure}).List(context.Background(), "owner", 1, 20); err == nil {
		t.Fatal("List ignored row scan failure")
	}
}

func TestStoreAuthenticateRejectsHashMismatchAndLastUseFailures(t *testing.T) {
	id, raw, _, err := issue()
	if err != nil {
		t.Fatal(err)
	}
	row := func(hash []byte) pgx.Row {
		return scanRow(func(dest ...any) error {
			*dest[0].(*[]byte) = hash
			*dest[1].(*string) = id
			*dest[2].(*string) = "owner-id"
			*dest[3].(*string) = "automation"
			*dest[4].(*Scope) = ReadOnly
			*dest[5].(*string) = "owner@example.test"
			return nil
		})
	}
	wrongHash := &fakeDB{row: func(string, ...any) pgx.Row { return row(make([]byte, 32)) }}
	if _, err := (&Store{DB: wrongHash}).Authenticate(context.Background(), raw); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("hash mismatch error = %v", err)
	}
	correct := digest(raw)
	for _, result := range []struct {
		tag pgconn.CommandTag
		err error
	}{{err: errors.New("update failed")}, {tag: pgconn.NewCommandTag("UPDATE 0")}} {
		db := &fakeDB{
			row:  func(string, ...any) pgx.Row { return row(correct[:]) },
			exec: func(string, ...any) (pgconn.CommandTag, error) { return result.tag, result.err },
		}
		_, err := (&Store{DB: db}).Authenticate(context.Background(), raw)
		if result.err != nil && !errors.Is(err, ErrUnavailable) || result.err == nil && !errors.Is(err, ErrInvalidToken) {
			t.Errorf("Authenticate update result (%v, %v) error = %v", result.tag, result.err, err)
		}
	}
}
