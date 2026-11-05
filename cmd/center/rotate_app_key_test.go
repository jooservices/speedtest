package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func rotationTestKeys() ([]byte, []byte, string, string) {
	oldKey, newKey := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	return oldKey, newKey, base64.StdEncoding.EncodeToString(oldKey), base64.StdEncoding.EncodeToString(newKey)
}

type rotationReadError struct{}

func (rotationReadError) Read([]byte) (int, error) { return 0, errors.New("private input error") }

func TestReadRotationKeys(t *testing.T) {
	oldKey, newKey, oldValue, newValue := rotationTestKeys()
	for _, tc := range []struct {
		name, old, new, stdin string
		valid                 bool
	}{
		{"environment", oldValue, newValue, "", true},
		{"stdin", "", "", oldValue + "\n" + newValue + "\n", true},
		{"prefix and CRLF", "", "", "base64:" + oldValue + "\r\nbase64:" + newValue + "\r\n", true},
		{"missing new", oldValue, "", "", false},
		{"missing old", "", newValue, "", false},
		{"empty input", "", "", "", false},
		{"one line", "", "", oldValue + "\n", false},
		{"oversized", "", "", strings.Repeat("x", 4096), false},
		{"bad old", "secret-invalid", newValue, "", false},
		{"short old", base64.StdEncoding.EncodeToString([]byte("short")), newValue, "", false},
		{"bad new", oldValue, "secret-invalid", "", false},
		{"short new", oldValue, base64.StdEncoding.EncodeToString([]byte("short")), "", false},
		{"identical", oldValue, oldValue, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old, newKeyGot, err := readRotationKeys(strings.NewReader(tc.stdin), tc.old, tc.new)
			if tc.valid {
				if err != nil || !bytes.Equal(old, oldKey) || !bytes.Equal(newKeyGot, newKey) {
					t.Fatal("key decoding failed")
				}
			} else if err == nil || strings.Contains(err.Error(), "secret-invalid") {
				t.Fatal("invalid keys accepted or echoed")
			}
		})
	}
	if _, _, err := readRotationKeys(rotationReadError{}, "", ""); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatal("input error not safely handled")
	}
}

func TestRotationCrypto(t *testing.T) {
	oldKey, newKey, _, _ := rotationTestKeys()
	old, err := rotationCipher(oldKey)
	if err != nil {
		t.Fatal(err)
	}
	other, err := rotationCipher(newKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rotationCipher(oldKey[:16]); err == nil {
		t.Fatal("non-256-bit key accepted")
	}
	plain, aad := []byte("private-value"), []byte("owner\x00create-node\x00hash")
	nonce := bytes.Repeat([]byte{3}, old.NonceSize())
	encrypted := old.Seal(nonce, nonce, plain, aad)
	got, err := openRotationValue(old, encrypted, aad)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatal("round trip failed")
	}
	for _, tc := range []struct{ value, aad []byte }{{encrypted, nil}, {encrypted[:2], aad}, {append([]byte(nil), encrypted...), []byte("other")}} {
		if _, err := openRotationValue(old, tc.value, tc.aad); err == nil {
			t.Fatal("invalid ciphertext accepted")
		}
	}
	if _, err := openRotationValue(other, encrypted, aad); err == nil {
		t.Fatal("wrong key accepted")
	}
}

func TestRotateCommandRejectsArguments(t *testing.T) {
	var output bytes.Buffer
	if code := mainRun([]string{"rotate-app-key", "private-argv"}, &output, io.Discard); code != 1 || strings.Contains(output.String(), "private-argv") {
		t.Fatal("command accepted or exposed argv")
	}
}

// These doubles exercise transaction failures without requiring any service.
type rotationFakeDB struct {
	tx   *rotationFakeTx
	fail bool
}

func (d rotationFakeDB) Begin(context.Context) (pgx.Tx, error) {
	if d.fail {
		return nil, errors.New("private begin failure")
	}
	return d.tx, nil
}

type rotationFakeTx struct {
	pgx.Tx
	fail                  string
	fetched               bool
	encrypted             []byte
	committed, rolledBack bool
}

func (tx *rotationFakeTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	for _, prefix := range []string{"LOCK", "DECLARE", "CLOSE", "DELETE"} {
		if strings.HasPrefix(sql, prefix) && tx.fail == prefix {
			return pgconn.CommandTag{}, errors.New("private SQL error")
		}
	}
	if strings.HasPrefix(sql, "DECLARE") {
		tx.fetched = false
	}
	return pgconn.NewCommandTag("OK"), nil
}
func (tx *rotationFakeTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	if tx.fail == "FETCH" {
		return nil, errors.New("private fetch error")
	}
	rows := &rotationFakeRows{encrypted: tx.encrypted, empty: tx.fetched, fail: tx.fail == "SCAN"}
	tx.fetched = true
	return rows, nil
}
func (tx *rotationFakeTx) QueryRow(_ context.Context, query string, args ...any) pgx.Row {
	if strings.HasPrefix(query, "SELECT EXISTS") {
		return rotationActiveRow{fail: tx.fail}
	}
	encrypted := args[1].([]byte)
	if tx.fail == "MISMATCH" {
		_, newKey, _, _ := rotationTestKeys()
		aead, _ := rotationCipher(newKey)
		nonce := make([]byte, aead.NonceSize())
		encrypted = aead.Seal(nonce, nonce, []byte("different plaintext"), nil)
	}
	return rotationFakeRow{encrypted: encrypted, fail: tx.fail}
}
func (tx *rotationFakeTx) Commit(context.Context) error {
	if tx.fail == "COMMIT" {
		return errors.New("private commit error")
	}
	tx.committed = true
	return nil
}
func (tx *rotationFakeTx) Rollback(context.Context) error { tx.rolledBack = true; return nil }

type rotationFakeRows struct {
	pgx.Rows
	encrypted         []byte
	empty, used, fail bool
}

func (r *rotationFakeRows) Next() bool {
	if r.empty || r.used {
		return false
	}
	r.used = true
	return true
}
func (r *rotationFakeRows) Scan(dest ...any) error {
	if r.fail {
		return errors.New("private scan error")
	}
	*dest[0].(*string) = "(1,1)"
	*dest[1].(*[]byte) = r.encrypted
	return nil
}
func (*rotationFakeRows) Err() error { return nil }
func (*rotationFakeRows) Close()     {}

type rotationFakeRow struct {
	encrypted []byte
	fail      string
}

func (r rotationFakeRow) Scan(dest ...any) error {
	if r.fail == "UPDATE" {
		return errors.New("private update error")
	}
	if r.fail == "VERIFY" {
		*dest[0].(*[]byte) = []byte("corrupt")
	} else {
		*dest[0].(*[]byte) = r.encrypted
	}
	return nil
}

func TestRotateTransactionUnit(t *testing.T) {
	oldKey, newKey, _, _ := rotationTestKeys()
	old, _ := rotationCipher(oldKey)
	nonce := make([]byte, old.NonceSize())
	encrypted := old.Seal(nonce, nonce, []byte("secret"), nil)
	for _, failure := range []string{"BEGIN", "LOCK", "ACTIVE", "CHECK", "DELETE", "DECLARE", "FETCH", "SCAN", "DECRYPT", "UPDATE", "VERIFY", "MISMATCH", "CLOSE", "COMMIT", ""} {
		t.Run(failure, func(t *testing.T) {
			tx := &rotationFakeTx{fail: failure, encrypted: encrypted}
			if failure == "DECRYPT" {
				tx.encrypted = []byte("bad")
			}
			count, err := rotateAppKey(t.Context(), rotationFakeDB{tx: tx, fail: failure == "BEGIN"}, oldKey, newKey)
			if failure == "" {
				if err != nil || count.values != 2 || !tx.committed {
					t.Fatal("valid rotation failed", count, err)
				}
			} else if err == nil || count != (rotationCounts{}) || tx.committed || strings.Contains(err.Error(), "private") {
				t.Fatal("unsafe failed transaction", err)
			}
			if failure != "BEGIN" && !tx.rolledBack {
				t.Fatal("transaction cleanup missing")
			}
		})
	}
}

func TestRotateCommandConfigurationRefusal(t *testing.T) {
	_, _, oldValue, newValue := rotationTestKeys()
	t.Setenv("OLD_APP_KEY", oldValue)
	t.Setenv("NEW_APP_KEY", newValue)
	t.Setenv("APP_ENV", "test")
	t.Setenv("APP_KEY", "")
	t.Setenv("SPEEDTEST_DB_PORT", "invalid")
	if err := runRotateAppKey(t.Context(), nil, strings.NewReader(""), io.Discard); err == nil {
		t.Fatal("invalid configuration accepted")
	}
	t.Setenv("SPEEDTEST_DB_PORT", "5432")
	t.Setenv("SPEEDTEST_DB_NAME", "speedtest")
	if err := runRotateAppKey(t.Context(), nil, strings.NewReader(""), io.Discard); err == nil {
		t.Fatal("live database accepted in test environment")
	}
	// Keep the malformed dotenv fixture inside this worktree and away from its real .env.
	if err := os.MkdirAll("../../.tmp", 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("../../.tmp", "rotation-config-")
	if err != nil {
		t.Fatal(err)
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(absolute) }()
	t.Chdir(absolute)
	if err := os.WriteFile(".env", []byte("INVALID='unterminated"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runRotateAppKey(t.Context(), nil, strings.NewReader(""), io.Discard); err == nil {
		t.Fatal("malformed dotenv accepted")
	}
}

type rotationActiveRow struct{ fail string }

func (r rotationActiveRow) Scan(dest ...any) error {
	if r.fail == "CHECK" {
		return errors.New("private check failure")
	}
	*dest[0].(*bool) = r.fail == "ACTIVE"
	return nil
}
