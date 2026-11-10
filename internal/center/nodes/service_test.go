package nodes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestInputNormalizeAppliesDefaultsAndNormalizesAddress(t *testing.T) {
	address := "2001:0db8::1"
	got, err := (Input{Name: "  edge  ", ExpectedDown: 300, ExpectedUp: 100, Binding: Binding{SourceIP: &address}}).Normalize()
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if got.Name != "edge" || got.Threads != 4 || got.Enabled != false {
		t.Fatalf("Normalize() defaults = %+v", got)
	}
	if got.Binding.SourceIP == nil || *got.Binding.SourceIP != "2001:db8::1" {
		t.Fatalf("normalized source IP = %v", got.Binding.SourceIP)
	}
}

func TestInputNormalizeRejectsInvalidValues(t *testing.T) {
	valid := Input{Name: "edge", ExpectedDown: 300, ExpectedUp: 100, Threads: 4}
	zero := 0
	badIP := "not-an-ip"
	tests := []struct {
		name string
		edit func(*Input)
	}{
		{"blank name", func(i *Input) { i.Name = "  " }},
		{"long name", func(i *Input) { i.Name = strings.Repeat("n", 101) }},
		{"nonpositive download", func(i *Input) { i.ExpectedDown = 0 }},
		{"infinite upload", func(i *Input) { i.ExpectedUp = math.Inf(1) }},
		{"nan download", func(i *Input) { i.ExpectedDown = math.NaN() }},
		{"threads below range", func(i *Input) { i.Threads = -1 }},
		{"threads above range", func(i *Input) { i.Threads = 65 }},
		{"invalid server", func(i *Input) { i.ServerID = &zero }},
		{"invalid source ip", func(i *Input) { i.Binding.SourceIP = &badIP }},
		{"invalid interface", func(i *Input) { i.Binding.Iface = strings.Repeat("x", 65) }},
		{"invalid UTF-8", func(i *Input) { i.Note = string([]byte{0xff}) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := valid
			tt.edit(&input)
			if _, err := input.Normalize(); err == nil {
				t.Fatal("Normalize() error = nil, want invalid input")
			}
		})
	}
}

func TestServerSelectionJSON(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  any
		bad   bool
	}{
		{input: `"auto"`, want: nil},
		{input: `42`, want: 42},
		{input: `0`, bad: true},
		{input: `"manual"`, bad: true},
		{input: `null`, bad: true},
	} {
		var got ServerSelection
		err := json.Unmarshal([]byte(tc.input), &got)
		if tc.bad {
			if err == nil {
				t.Errorf("Unmarshal(%s) error = nil", tc.input)
			}
			continue
		}
		if err != nil {
			t.Fatalf("Unmarshal(%s) error = %v", tc.input, err)
		}
		if tc.want == nil && got.ID != nil {
			t.Errorf("server ID = %v, want auto", *got.ID)
		}
		if want, ok := tc.want.(int); ok && (got.ID == nil || *got.ID != want) {
			t.Errorf("server ID = %v, want %d", got.ID, want)
		}
		encoded, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != tc.input {
			t.Errorf("Marshal() = %s, want %s", encoded, tc.input)
		}
	}
}

func TestSecretAndUUIDHelpers(t *testing.T) {
	id, err := newUUID()
	if err != nil || !validUUID(id) {
		t.Fatalf("newUUID() = %q, %v", id, err)
	}
	if id[14] != '4' {
		t.Errorf("UUID version = %q, want 4", id[14])
	}
	secret, err := newSecret("sp_")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, "sp_") || len(secret) < 40 {
		t.Fatalf("generated secret has invalid format")
	}
	if hashSecret(secret) == secret || len(hashSecret(secret)) != 64 {
		t.Fatal("hashSecret must return a SHA-256 hex digest")
	}
	if !constantHashEqual(hashSecret(secret), hashSecret(secret)) || constantHashEqual(hashSecret(secret), hashSecret("different")) || constantHashEqual("bad", hashSecret(secret)) {
		t.Fatal("constantHashEqual returned an unexpected result")
	}
	for _, value := range []string{"", "not-a-uuid", "00000000-0000-0000-0000-000000000000x"} {
		if validUUID(value) {
			t.Errorf("validUUID(%q) = true", value)
		}
	}
	extraHyphen := "-0000000-0000-4000-8000-000000000001"
	if len(extraHyphen) != 36 || validUUID(extraHyphen) {
		t.Fatal("UUID with an extra separator was accepted")
	}
	if _, err := newUUIDFrom(&testRandomReader{failAt: 1}); err == nil {
		t.Fatal("newUUIDFrom accepted a failing random source")
	}
	if _, err := newSecretFrom(&testRandomReader{failAt: 1}, "test_"); err == nil {
		t.Fatal("newSecretFrom accepted a failing random source")
	}
}

func TestNodeResponses(t *testing.T) {
	serverID := 7
	input := Input{Name: "edge", Label: "label", Provider: "provider", Plan: "plan", ExpectedDown: 300, ExpectedUp: 100, Threads: 4, ServerID: &serverID, Enabled: true, AutoUpdate: true}
	node := nodeResponse("00000000-0000-4000-8000-000000000001", input, 2, testTime(), 30, 0, "", "")
	isp, hasISP := node["isp"]
	if node["server"] != 7 || node["status"] != "offline" || node["lastSeen"] != nil || node["autoUpdate"] != true || !hasISP || isp != nil {
		t.Fatalf("node response = %#v", node)
	}
	record := nodeRecord{ID: "node-id", Name: "edge", ExpectedDown: 300, ExpectedUp: 100, Threads: 4, Revision: 3, Interval: 30, ScheduleOn: true, Enabled: true, AutoUpdate: true, ServerID: pgtype.Int4{Int32: 7, Valid: true}}
	config := record.agentConfig()
	if config["revision"] != "3" || config["name"] != "edge" || config["server"] != 7 || config["release"] != nil {
		t.Fatalf("agent config = %#v", config)
	}
	address := "192.0.2.1"
	record.BindingIP = pgtype.Text{String: address, Valid: true}
	if got := record.input().Binding.SourceIP; got == nil || *got != address {
		t.Fatalf("node input source IP = %v", got)
	}
}

func testTime() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }

type scriptedDatabase struct {
	beginErr error
	row      func(string, ...any) pgx.Row
	execErr  error
	tx       *scriptedTx
}

func (d *scriptedDatabase) Begin(context.Context) (pgx.Tx, error) {
	if d.beginErr != nil {
		return nil, d.beginErr
	}
	if d.tx == nil {
		d.tx = &scriptedTx{}
	}
	return d.tx, nil
}
func (d *scriptedDatabase) QueryRow(_ context.Context, query string, args ...any) pgx.Row {
	if d.row != nil {
		return d.row(query, args...)
	}
	return scriptedRow{err: errors.New("unexpected query")}
}
func (d *scriptedDatabase) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, d.execErr
}

type scriptedTx struct {
	pgx.Tx
	queryRow  func(string, ...any) pgx.Row
	exec      func(string, ...any) error
	commitErr error
}

func (tx *scriptedTx) QueryRow(_ context.Context, query string, args ...any) pgx.Row {
	if tx.queryRow != nil {
		return tx.queryRow(query, args...)
	}
	return scriptedRow{err: errors.New("unexpected transaction query")}
}
func (tx *scriptedTx) Exec(_ context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	if tx.exec != nil {
		return pgconn.CommandTag{}, tx.exec(query, args...)
	}
	return pgconn.CommandTag{}, nil
}
func (tx *scriptedTx) Commit(context.Context) error   { return tx.commitErr }
func (tx *scriptedTx) Rollback(context.Context) error { return nil }

type scriptedRow struct {
	values []any
	err    error
}

func (row scriptedRow) Scan(dest ...any) error {
	if row.err != nil {
		return row.err
	}
	if len(row.values) != len(dest) {
		return errors.New("unexpected scan shape")
	}
	for i, value := range row.values {
		target := reflect.ValueOf(dest[i])
		if target.Kind() != reflect.Pointer || target.IsNil() {
			return errors.New("scan destination is not a pointer")
		}
		source := reflect.ValueOf(value)
		if !source.IsValid() {
			continue
		}
		switch {
		case source.Type().AssignableTo(target.Elem().Type()):
			target.Elem().Set(source)
		case source.Type().ConvertibleTo(target.Elem().Type()):
			target.Elem().Set(source.Convert(target.Elem().Type()))
		default:
			return errors.New("incompatible scripted scan value")
		}
	}
	return nil
}

type badJSON struct{}

func (badJSON) MarshalJSON() ([]byte, error) { return nil, errors.New("marshal failed") }

type testRandomReader struct {
	calls  int
	failAt int
}

func (r *testRandomReader) Read(destination []byte) (int, error) {
	r.calls++
	if r.calls == r.failAt {
		return 0, errors.New("random source failed")
	}
	for i := range destination {
		destination[i] = byte(r.calls)
	}
	return len(destination), nil
}

func TestDatabaseFailurePathsFailClosed(t *testing.T) {
	ctx := t.Context()
	input := Input{Name: "edge", ExpectedDown: 300, ExpectedUp: 100, Enabled: true, Threads: 4}
	key := bytes.Repeat([]byte{0x44}, 32)
	connectionError := errors.New("database unavailable")
	t.Run("enroll begin failure", func(t *testing.T) {
		service := NewService(&scriptedDatabase{beginErr: connectionError}, key)
		if _, err := service.Enroll(ctx, "token", "edge", "", "agent"); !errors.Is(err, connectionError) {
			t.Fatalf("Enroll() error = %v", err)
		}
	})
	t.Run("create begins with app key", func(t *testing.T) {
		service := NewService(&scriptedDatabase{beginErr: connectionError}, key)
		if _, _, err := service.CreateNode(ctx, input, "owner", "key"); !errors.Is(err, connectionError) {
			t.Fatalf("CreateNode() error = %v", err)
		}
	})
	t.Run("enroll rejects invalid token lookup error", func(t *testing.T) {
		tx := &scriptedTx{queryRow: func(string, ...any) pgx.Row { return scriptedRow{err: connectionError} }}
		service := NewService(&scriptedDatabase{tx: tx}, key)
		if _, err := service.Enroll(ctx, "token", "edge", "", "agent"); !errors.Is(err, connectionError) {
			t.Fatalf("Enroll() error = %v", err)
		}
	})
	t.Run("enroll rejects absent token", func(t *testing.T) {
		tx := &scriptedTx{queryRow: func(string, ...any) pgx.Row { return scriptedRow{err: pgx.ErrNoRows} }}
		service := NewService(&scriptedDatabase{tx: tx}, key)
		if _, err := service.Enroll(ctx, "token", "edge", "", "agent"); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("Enroll() error = %v", err)
		}
	})
	t.Run("revoke fails when transaction cannot begin", func(t *testing.T) {
		service := NewService(&scriptedDatabase{beginErr: connectionError}, key)
		if err := service.RevokeCredential(ctx, "00000000-0000-4000-8000-000000000001"); !errors.Is(err, connectionError) {
			t.Fatalf("RevokeCredential() error = %v", err)
		}
	})
	t.Run("revoke returns not found", func(t *testing.T) {
		tx := &scriptedTx{queryRow: func(string, ...any) pgx.Row { return scriptedRow{err: pgx.ErrNoRows} }}
		service := NewService(&scriptedDatabase{tx: tx}, key)
		if err := service.RevokeCredential(ctx, "00000000-0000-4000-8000-000000000001"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("RevokeCredential() error = %v", err)
		}
	})
	t.Run("auth query errors", func(t *testing.T) {
		service := NewService(&scriptedDatabase{row: func(string, ...any) pgx.Row { return scriptedRow{err: connectionError} }}, key)
		if _, err := service.AuthenticateAgent(ctx, "credential"); !errors.Is(err, connectionError) {
			t.Fatalf("AuthenticateAgent() error = %v", err)
		}
	})
	t.Run("Settings key query errors", func(t *testing.T) {
		service := NewService(&scriptedDatabase{row: func(string, ...any) pgx.Row { return scriptedRow{err: connectionError} }}, key)
		if _, err := service.ValidateEnrollmentKey(ctx, "key"); !errors.Is(err, connectionError) {
			t.Fatalf("ValidateEnrollmentKey() error = %v", err)
		}
	})
}

func TestIdempotencyTransactionFailurePaths(t *testing.T) {
	ctx := t.Context()
	key := bytes.Repeat([]byte{0x51}, 32)
	queryErr := errors.New("query failed")
	createErr := errors.New("create failed")
	insertErr := errors.New("insert failed")
	cases := []struct {
		name   string
		db     *scriptedDatabase
		row    pgx.Row
		create func(pgx.Tx) (any, error)
		want   error
	}{
		{name: "begin failure", db: &scriptedDatabase{beginErr: queryErr}, want: queryErr},
		{name: "advisory lock failure", db: &scriptedDatabase{tx: &scriptedTx{exec: func(string, ...any) error { return queryErr }}}, want: queryErr},
		{name: "lookup failure", db: &scriptedDatabase{tx: &scriptedTx{queryRow: func(string, ...any) pgx.Row { return scriptedRow{err: queryErr} }}}, want: queryErr},
		{name: "same key different request", db: &scriptedDatabase{tx: &scriptedTx{queryRow: func(string, ...any) pgx.Row { return scriptedRow{values: []any{strings.Repeat("0", 64), []byte{}}} }}}, want: ErrConflict},
		{name: "bad stored replay", db: &scriptedDatabase{tx: &scriptedTx{queryRow: func(string, ...any) pgx.Row {
			sum := sha256.Sum256([]byte(`"request"`))
			return scriptedRow{values: []any{hex.EncodeToString(sum[:]), []byte{1}}}
		}}}, want: errors.New("stored idempotency response is invalid")},
		{name: "delete expired failure", db: &scriptedDatabase{tx: &scriptedTx{queryRow: func(string, ...any) pgx.Row { return scriptedRow{err: pgx.ErrNoRows} }, exec: func(query string, _ ...any) error {
			if strings.Contains(query, "principal=$1") {
				return queryErr
			}
			return nil
		}}}, want: queryErr},
		{name: "cleanup failure", db: &scriptedDatabase{tx: &scriptedTx{queryRow: func(string, ...any) pgx.Row { return scriptedRow{err: pgx.ErrNoRows} }, exec: func(query string, _ ...any) error {
			if strings.Contains(query, "ctid") {
				return queryErr
			}
			return nil
		}}}, want: queryErr},
		{name: "create failure", db: &scriptedDatabase{tx: &scriptedTx{queryRow: func(string, ...any) pgx.Row { return scriptedRow{err: pgx.ErrNoRows} }}}, create: func(pgx.Tx) (any, error) { return nil, createErr }, want: createErr},
		{name: "response marshal failure", db: &scriptedDatabase{tx: &scriptedTx{queryRow: func(string, ...any) pgx.Row { return scriptedRow{err: pgx.ErrNoRows} }}}, create: func(pgx.Tx) (any, error) { return badJSON{}, nil }, want: errors.New("marshal failed")},
		{name: "insert failure", db: &scriptedDatabase{tx: &scriptedTx{queryRow: func(string, ...any) pgx.Row { return scriptedRow{err: pgx.ErrNoRows} }, exec: func(query string, _ ...any) error {
			if strings.Contains(query, "INSERT INTO api_idempotency") {
				return insertErr
			}
			return nil
		}}}, create: func(pgx.Tx) (any, error) { return map[string]string{"ok": "yes"}, nil }, want: insertErr},
		{name: "commit failure", db: &scriptedDatabase{tx: &scriptedTx{queryRow: func(string, ...any) pgx.Row { return scriptedRow{err: pgx.ErrNoRows} }, exec: func(string, ...any) error { return nil }, commitErr: queryErr}}, create: func(pgx.Tx) (any, error) { return map[string]string{"ok": "yes"}, nil }, want: queryErr},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service := NewService(tc.db, key)
			create := tc.create
			if create == nil {
				create = func(pgx.Tx) (any, error) { return map[string]string{"ok": "yes"}, nil }
			}
			_, _, err := service.idempotent(ctx, "owner", "operation", "key", "request", create)
			switch tc.name {
			case "bad stored replay":
				if err == nil || !strings.Contains(err.Error(), "stored idempotency response is invalid") {
					t.Fatalf("error = %v", err)
				}
			case "response marshal failure":
				if err == nil || !strings.Contains(err.Error(), "marshal failed") {
					t.Fatalf("error = %v", err)
				}
			default:
				if !errors.Is(err, tc.want) {
					t.Fatalf("error = %v, want %v", err, tc.want)
				}
			}
		})
	}
	service := NewService(&scriptedDatabase{}, key)
	if _, _, err := service.idempotent(ctx, "owner", "op", "key", badJSON{}, func(pgx.Tx) (any, error) { return nil, nil }); err == nil {
		t.Fatal("invalid request JSON was accepted")
	}
}

func TestDatabaseErrorTranslationAndInvalidIDs(t *testing.T) {
	if !errors.Is(translateDBError(&pgconn.PgError{Code: "23505"}), ErrConflict) {
		t.Fatal("unique violation was not translated to conflict")
	}
	other := errors.New("other database error")
	if !errors.Is(translateDBError(other), other) {
		t.Fatal("non-unique error was changed")
	}
	service := NewService(&scriptedDatabase{}, bytes.Repeat([]byte{1}, 32))
	if _, _, err := service.RotateCredential(t.Context(), "bad", "owner", "key"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("malformed rotation ID error = %v", err)
	}
	if _, err := service.AuthenticateAgent(t.Context(), ""); !errors.Is(err, ErrInvalidAuth) {
		t.Fatalf("empty agent credential error = %v", err)
	}
	if _, err := service.ValidateEnrollmentKey(t.Context(), ""); !errors.Is(err, ErrInvalidAuth) {
		t.Fatalf("empty enrollment key error = %v", err)
	}
}

func TestCreateNodeTransactionSuccessAndDatabaseBoundaries(t *testing.T) {
	input := Input{Name: "edge", ExpectedDown: 300, ExpectedUp: 100, Enabled: true, Threads: 4}
	makeDatabase := func(fail string) *scriptedDatabase {
		tx := &scriptedTx{}
		tx.queryRow = func(query string, _ ...any) pgx.Row {
			switch {
			case strings.Contains(query, "api_idempotency"):
				return scriptedRow{err: pgx.ErrNoRows}
			case strings.Contains(query, "MAX(color_slot)"):
				if fail == "color" {
					return scriptedRow{err: errors.New("color query failed")}
				}
				return scriptedRow{values: []any{1}}
			case strings.Contains(query, "INSERT INTO nodes"):
				if fail == "node" {
					return scriptedRow{err: &pgconn.PgError{Code: "23505"}}
				}
				return scriptedRow{values: []any{testTime()}}
			case strings.Contains(query, "INSERT INTO node_credentials"):
				if fail == "enrollment" {
					return scriptedRow{err: &pgconn.PgError{Code: "23505"}}
				}
				return scriptedRow{values: []any{testTime().Add(enrollmentLifetime)}}
			default:
				return scriptedRow{err: fmt.Errorf("unexpected create query: %s", query)}
			}
		}
		tx.exec = func(query string, _ ...any) error {
			if fail == "schedule" && strings.Contains(query, "INSERT INTO schedules") {
				return errors.New("schedule insert failed")
			}
			return nil
		}
		return &scriptedDatabase{tx: tx}
	}

	t.Run("complete transactional issuance", func(t *testing.T) {
		service := NewService(makeDatabase(""), bytes.Repeat([]byte{0x42}, 32))
		response, replay, err := service.CreateNode(t.Context(), input, "owner", "key")
		if err != nil || replay {
			t.Fatalf("CreateNode() = %s, %t, %v", response, replay, err)
		}
		var issued map[string]any
		if err := json.Unmarshal(response, &issued); err != nil || issued["token"] == nil {
			t.Fatalf("issued response = %s, %v", response, err)
		}
	})
	for _, test := range []struct {
		name string
		want error
	}{
		{"color query", errors.New("color query failed")},
		{"node insert", ErrConflict},
		{"schedule insert", errors.New("schedule insert failed")},
		{"enrollment insert", ErrConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := NewService(makeDatabase(strings.Split(test.name, " ")[0]), bytes.Repeat([]byte{0x42}, 32))
			_, _, err := service.CreateNode(t.Context(), input, "owner", "key")
			if test.name == "color query" || test.name == "schedule insert" {
				if err == nil || err.Error() != test.want.Error() {
					t.Fatalf("CreateNode() error = %v, want %v", err, test.want)
				}
			} else if !errors.Is(err, test.want) {
				t.Fatalf("CreateNode() error = %v, want %v", err, test.want)
			}
		})
	}
	for _, failAt := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("random generation %d", failAt), func(t *testing.T) {
			service := NewService(makeDatabase(""), bytes.Repeat([]byte{0x42}, 32))
			service.random = &testRandomReader{failAt: failAt}
			if _, _, err := service.CreateNode(t.Context(), input, "owner", "key"); err == nil || !strings.Contains(err.Error(), "random source failed") {
				t.Fatalf("CreateNode() random failure = %v", err)
			}
		})
	}
	if _, _, err := NewService(makeDatabase(""), bytes.Repeat([]byte{0x42}, 32)).CreateNode(t.Context(), Input{}, "owner", "key"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid CreateNode() error = %v", err)
	}
}

func TestEnrollTransactionSuccessAndNodeBoundaries(t *testing.T) {
	const credentialID = "00000000-0000-4000-8000-000000000011"
	const nodeID = "00000000-0000-4000-8000-000000000012"
	makeDatabase := func(fail string) *scriptedDatabase {
		tx := &scriptedTx{}
		tx.queryRow = func(query string, _ ...any) pgx.Row {
			switch {
			case strings.Contains(query, "token_hash=$1 AND kind='enrollment'"):
				if fail == "token-query" {
					return scriptedRow{err: errors.New("token query failed")}
				}
				if fail == "bad-hash" {
					return scriptedRow{values: []any{credentialID, nodeID, "not-a-hash"}}
				}
				return scriptedRow{values: []any{credentialID, nodeID, hashSecret("enr_token")}}
			case strings.Contains(query, "FROM nodes n JOIN schedules"):
				if fail == "missing-node" {
					return scriptedRow{err: pgx.ErrNoRows}
				}
				if fail == "lock-error" {
					return scriptedRow{err: errors.New("node lock failed")}
				}
				return scriptedRow{values: []any{nodeID, "edge", "", "", "", float64(300), float64(100), 4, pgtype.Int4{}, 1, true, false, "", pgtype.Text{}, "", int64(1), 30, 0, true, "", "", testTime()}}
			case strings.Contains(query, "SELECT expires_at,consumed_at,revoked_at"):
				if fail == "expiry-query" {
					return scriptedRow{err: errors.New("expiry query failed")}
				}
				return scriptedRow{values: []any{time.Now().Add(time.Hour), pgtype.Timestamptz{}, pgtype.Timestamptz{}}}
			default:
				return scriptedRow{err: fmt.Errorf("unexpected enrollment query: %s", query)}
			}
		}
		tx.exec = func(query string, _ ...any) error {
			for _, part := range []string{"consumed_at=now()", "kind='bearer'", "VALUES ($1,$2,'bearer',$3)", "UPDATE nodes SET host="} {
				if fail == part && strings.Contains(query, part) {
					return errors.New(part + " failed")
				}
			}
			return nil
		}
		if fail == "commit" {
			tx.commitErr = errors.New("commit failed")
		}
		return &scriptedDatabase{tx: tx}
	}
	t.Run("consumes token and issues bearer", func(t *testing.T) {
		service := NewService(makeDatabase(""), nil)
		result, err := service.Enroll(t.Context(), "enr_token", "edge", "host", "agent")
		if err != nil {
			t.Fatalf("Enroll() error = %v", err)
		}
		if result["nodeId"] != nodeID || !strings.HasPrefix(result["credential"].(string), "sp_") {
			t.Fatalf("Enroll() result = %#v", result)
		}
	})
	for _, test := range []struct {
		name string
		want error
	}{
		{"token query", errors.New("token query failed")},
		{"bad hash", ErrInvalidToken},
		{"missing node", ErrInvalidToken},
		{"lock error", errors.New("node lock failed")},
		{"expiry query", errors.New("expiry query failed")},
		{"consume failure", errors.New("consumed_at=now() failed")},
		{"revoke failure", errors.New("kind='bearer' failed")},
		{"bearer insert failure", errors.New("VALUES ($1,$2,'bearer',$3) failed")},
		{"node update failure", errors.New("UPDATE nodes SET host= failed")},
		{"commit failure", errors.New("commit failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := NewService(makeDatabase(map[string]string{
				"token query": "token-query", "bad hash": "bad-hash", "missing node": "missing-node",
				"lock error": "lock-error", "expiry query": "expiry-query",
				"consume failure": "consumed_at=now()", "revoke failure": "kind='bearer'",
				"bearer insert failure": "VALUES ($1,$2,'bearer',$3)",
				"node update failure":   "UPDATE nodes SET host=", "commit failure": "commit",
			}[test.name]), nil)
			_, err := service.Enroll(t.Context(), "enr_token", "edge", "host", "agent")
			if test.want == ErrInvalidToken {
				if !errors.Is(err, test.want) {
					t.Fatalf("Enroll() error = %v, want %v", err, test.want)
				}
			} else if err == nil || err.Error() != test.want.Error() {
				t.Fatalf("Enroll() error = %v, want %v", err, test.want)
			}
		})
	}
	for _, failAt := range []int{1, 2} {
		t.Run(fmt.Sprintf("random generation %d", failAt), func(t *testing.T) {
			service := NewService(makeDatabase(""), nil)
			service.random = &testRandomReader{failAt: failAt}
			if _, err := service.Enroll(t.Context(), "enr_token", "edge", "", "agent"); err == nil || !strings.Contains(err.Error(), "random source failed") {
				t.Fatalf("Enroll() random failure = %v", err)
			}
		})
	}
	for _, test := range []struct {
		name, nameOnNode  string
		expiresAt         time.Time
		consumed, revoked bool
	}{
		{name: "wrong name", nameOnNode: "another", expiresAt: time.Now().Add(time.Hour)},
		{name: "expired", nameOnNode: "edge", expiresAt: time.Now().Add(-time.Second)},
		{name: "consumed", nameOnNode: "edge", expiresAt: time.Now().Add(time.Hour), consumed: true},
		{name: "revoked", nameOnNode: "edge", expiresAt: time.Now().Add(time.Hour), revoked: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			database := makeDatabase("")
			tx := database.tx
			original := tx.queryRow
			tx.queryRow = func(query string, args ...any) pgx.Row {
				if strings.Contains(query, "FROM nodes n JOIN schedules") {
					values := []any{nodeID, test.nameOnNode, "", "", "", float64(300), float64(100), 4, pgtype.Int4{}, 1, true, false, "", pgtype.Text{}, "", int64(1), 30, 0, true, "", "", testTime()}
					return scriptedRow{values: values}
				}
				if strings.Contains(query, "SELECT expires_at,consumed_at,revoked_at") {
					consumed := pgtype.Timestamptz{}
					revoked := pgtype.Timestamptz{}
					if test.consumed {
						consumed = pgtype.Timestamptz{Time: testTime(), Valid: true}
					}
					if test.revoked {
						revoked = pgtype.Timestamptz{Time: testTime(), Valid: true}
					}
					return scriptedRow{values: []any{test.expiresAt, consumed, revoked}}
				}
				return original(query, args...)
			}
			service := NewService(database, nil)
			if _, err := service.Enroll(t.Context(), "enr_token", "edge", "", "agent"); !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("Enroll() error = %v, want invalid token", err)
			}
		})
	}
	if _, err := NewService(makeDatabase(""), nil).Enroll(t.Context(), "", "edge", "", "agent"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty token error = %v", err)
	}
}

func TestCredentialRotationAndRevocationTransactionPaths(t *testing.T) {
	const nodeID = "00000000-0000-4000-8000-000000000012"
	makeNodeRow := func() pgx.Row {
		return scriptedRow{values: []any{nodeID, "edge", "", "", "", float64(300), float64(100), 4, pgtype.Int4{}, 1, true, false, "", pgtype.Text{}, "", int64(1), 30, 0, true, "", "", testTime()}}
	}
	t.Run("rotation issues replacement", func(t *testing.T) {
		tx := &scriptedTx{queryRow: func(query string, _ ...any) pgx.Row {
			if strings.Contains(query, "api_idempotency") {
				return scriptedRow{err: pgx.ErrNoRows}
			}
			return makeNodeRow()
		}}
		service := NewService(&scriptedDatabase{tx: tx}, bytes.Repeat([]byte{0x7a}, 32))
		response, replay, err := service.RotateCredential(t.Context(), nodeID, "owner", "rotate-key")
		if err != nil || replay {
			t.Fatalf("RotateCredential() = %s, %t, %v", response, replay, err)
		}
		if !strings.Contains(string(response), `"credential":"sp_`) {
			t.Fatalf("rotation response = %s", response)
		}
	})
	t.Run("rotation missing node", func(t *testing.T) {
		tx := &scriptedTx{queryRow: func(query string, _ ...any) pgx.Row {
			if strings.Contains(query, "api_idempotency") {
				return scriptedRow{err: pgx.ErrNoRows}
			}
			return scriptedRow{err: pgx.ErrNoRows}
		}}
		service := NewService(&scriptedDatabase{tx: tx}, bytes.Repeat([]byte{0x7a}, 32))
		if _, _, err := service.RotateCredential(t.Context(), nodeID, "owner", "rotate-key"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("RotateCredential() error = %v", err)
		}
	})
	for _, test := range []struct {
		name       string
		queryError error
		execMatch  string
		execError  error
		failAt     int
	}{
		{name: "lock error", queryError: errors.New("lock failed")},
		{name: "update error", execMatch: "UPDATE node_credentials", execError: errors.New("update failed")},
		{name: "insert error", execMatch: "INSERT INTO node_credentials", execError: errors.New("insert failed")},
		{name: "secret generation", failAt: 1},
		{name: "identifier generation", failAt: 2},
	} {
		t.Run("rotation/"+test.name, func(t *testing.T) {
			tx := &scriptedTx{
				queryRow: func(query string, _ ...any) pgx.Row {
					if strings.Contains(query, "api_idempotency") {
						return scriptedRow{err: pgx.ErrNoRows}
					}
					if test.queryError != nil {
						return scriptedRow{err: test.queryError}
					}
					return makeNodeRow()
				},
				exec: func(query string, _ ...any) error {
					if test.execMatch != "" && strings.Contains(query, test.execMatch) {
						return test.execError
					}
					return nil
				},
			}
			service := NewService(&scriptedDatabase{tx: tx}, bytes.Repeat([]byte{0x7a}, 32))
			if test.failAt != 0 {
				service.random = &testRandomReader{failAt: test.failAt}
			}
			_, _, err := service.RotateCredential(t.Context(), nodeID, "owner", "rotate-key")
			want := test.execError
			if test.queryError != nil {
				want = test.queryError
			}
			if err == nil || (want != nil && err.Error() != want.Error()) || (want == nil && !strings.Contains(err.Error(), "random source failed")) {
				t.Fatalf("RotateCredential() error = %v, want %v", err, want)
			}
		})
	}
	t.Run("enrollment key database and entropy errors", func(t *testing.T) {
		newDB := func() *scriptedDatabase {
			return &scriptedDatabase{tx: &scriptedTx{
				queryRow: func(string, ...any) pgx.Row { return scriptedRow{err: pgx.ErrNoRows} },
				exec: func(query string, _ ...any) error {
					if strings.Contains(query, "INSERT INTO settings") {
						return errors.New("settings update failed")
					}
					return nil
				},
			}}
		}
		service := NewService(newDB(), bytes.Repeat([]byte{0x7a}, 32))
		if _, _, err := service.RotateEnrollmentKey(t.Context(), "owner", "key"); err == nil || err.Error() != "settings update failed" {
			t.Fatalf("RotateEnrollmentKey() database error = %v", err)
		}
		service = NewService(newDB(), bytes.Repeat([]byte{0x7a}, 32))
		service.random = &testRandomReader{failAt: 1}
		if _, _, err := service.RotateEnrollmentKey(t.Context(), "owner", "key"); err == nil || !strings.Contains(err.Error(), "random source failed") {
			t.Fatalf("RotateEnrollmentKey() random error = %v", err)
		}
	})
	for _, test := range []struct {
		name      string
		queryErr  error
		execErr   error
		commitErr error
		want      error
	}{
		{"missing node", pgx.ErrNoRows, nil, nil, ErrNotFound},
		{"lock query failure", errors.New("lock failed"), nil, nil, errors.New("lock failed")},
		{"revoke update failure", nil, errors.New("update failed"), nil, errors.New("update failed")},
		{"commit failure", nil, nil, errors.New("commit failed"), errors.New("commit failed")},
		{"successful revoke", nil, nil, nil, nil},
	} {
		t.Run("revoke/"+test.name, func(t *testing.T) {
			tx := &scriptedTx{
				queryRow: func(string, ...any) pgx.Row {
					if test.queryErr != nil {
						return scriptedRow{err: test.queryErr}
					}
					return makeNodeRow()
				},
				exec:      func(string, ...any) error { return test.execErr },
				commitErr: test.commitErr,
			}
			service := NewService(&scriptedDatabase{tx: tx}, nil)
			err := service.RevokeCredential(t.Context(), nodeID)
			switch test.want {
			case nil:
				if err != nil {
					t.Fatalf("RevokeCredential() error = %v", err)
				}
			case ErrNotFound:
				if !errors.Is(err, test.want) {
					t.Fatalf("RevokeCredential() error = %v, want %v", err, test.want)
				}
			default:
				if err == nil || err.Error() != test.want.Error() {
					t.Fatalf("RevokeCredential() error = %v, want %v", err, test.want)
				}
			}
		})
	}
}

func TestInstallTokenFailuresReturnNoCredential(t *testing.T) {
	const id = "00000000-0000-4000-8000-000000000043"
	injected := errors.New("injected token failure")
	for _, stage := range []string{"begin", "lookup", "secret", "uuid", "revoke-old", "insert", "commit"} {
		t.Run(stage, func(t *testing.T) {
			tx := &scriptedTx{queryRow: func(string, ...any) pgx.Row { return scriptedRow{values: []any{"node"}} }}
			database := &scriptedDatabase{tx: tx}
			random := &testRandomReader{}
			switch stage {
			case "begin":
				database.beginErr = injected
			case "lookup":
				tx.queryRow = func(string, ...any) pgx.Row { return scriptedRow{err: injected} }
			case "secret":
				random.failAt = 1
			case "uuid":
				random.failAt = 2
			case "revoke-old", "insert":
				tx.exec = func(query string, _ ...any) error {
					if (stage == "insert" && strings.HasPrefix(query, "INSERT")) || (stage == "revoke-old" && strings.HasPrefix(query, "UPDATE")) {
						return injected
					}
					return nil
				}
			case "commit":
				tx.commitErr = injected
			}
			s := NewService(database, nil)
			s.random = random
			token, name, err := s.IssueInstallToken(t.Context(), id)
			if err == nil || token != "" || name != "" {
				t.Fatalf("%s returned a credential or swallowed failure: token present=%t name=%q err=%v", stage, token != "", name, err)
			}
		})
	}
	s := NewService(&scriptedDatabase{execErr: injected}, nil)
	if err := s.RevokeInstallToken(t.Context(), "fixture"); !errors.Is(err, injected) {
		t.Fatalf("revocation error: %v", err)
	}
}
