package nodes

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestIdempotencyReplayEncryption(t *testing.T) {
	service := NewService(nil, bytes.Repeat([]byte{0x4a}, 32))
	plain := []byte(`{"token":"one-time-secret"}`)
	ciphertext, err := service.encryptReplay(plain, "owner-1", "create-node", "key-hash")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, []byte("one-time-secret")) {
		t.Fatal("encrypted replay contains plaintext secret")
	}
	got, err := service.decryptReplay(ciphertext, "owner-1", "create-node", "key-hash")
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("decryptReplay() = %q, %v", got, err)
	}
	if _, err := service.decryptReplay(ciphertext, "owner-2", "create-node", "key-hash"); err == nil {
		t.Fatal("decrypt with different principal succeeded")
	}
	if _, err := service.decryptReplay(ciphertext[:4], "owner-1", "create-node", "key-hash"); err == nil {
		t.Fatal("decrypt of truncated ciphertext succeeded")
	}
	other := NewService(nil, bytes.Repeat([]byte{0x5b}, 32))
	if _, err := other.decryptReplay(ciphertext, "owner-1", "create-node", "key-hash"); err == nil {
		t.Fatal("decrypt with wrong APP_KEY succeeded")
	}
	invalidKey := NewService(nil, []byte("short"))
	if _, err := invalidKey.decryptReplay(ciphertext, "owner-1", "create-node", "key-hash"); err == nil {
		t.Fatal("decrypt with invalid APP_KEY length succeeded")
	}
}

func TestIdempotentRejectsInvalidKeysBeforeDatabaseUse(t *testing.T) {
	service := NewService(nil, bytes.Repeat([]byte{0x4a}, 32))
	create := func(_ pgx.Tx) (any, error) { return map[string]string{"token": "x"}, nil }
	if _, _, err := service.idempotent(t.Context(), "", "create", "key", nil, create); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty principal error = %v", err)
	}
	if _, _, err := service.idempotent(t.Context(), "owner", "create", "", nil, create); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty idempotency key error = %v", err)
	}
	if _, _, err := service.idempotent(t.Context(), "owner", "create", string(bytes.Repeat([]byte{'x'}, 129)), nil, create); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("long idempotency key error = %v", err)
	}
	service.appKey = nil
	if _, _, err := service.idempotent(t.Context(), "owner", "create", "key", nil, create); !errors.Is(err, ErrCryptoKeyUnset) {
		t.Fatalf("missing APP_KEY error = %v", err)
	}
}

func TestIdempotencyReplayCommitAndEncryptionFailures(t *testing.T) {
	key := bytes.Repeat([]byte{0x5c}, 32)
	service := NewService(nil, key)
	requestHash := sha256.Sum256([]byte(`"request"`))
	keyHash := hashSecret("key")
	ciphertext, err := service.encryptReplay([]byte(`{"ok":"yes"}`), "owner", "operation", keyHash)
	if err != nil {
		t.Fatal(err)
	}
	tx := &scriptedTx{
		queryRow: func(string, ...any) pgx.Row {
			return scriptedRow{values: []any{hex.EncodeToString(requestHash[:]), ciphertext}}
		},
		commitErr: errors.New("replay commit failed"),
	}
	service.db = &scriptedDatabase{tx: tx}
	if _, _, err := service.idempotent(t.Context(), "owner", "operation", "key", "request", func(pgx.Tx) (any, error) {
		return map[string]string{"unexpected": "creation"}, nil
	}); err == nil || err.Error() != "replay commit failed" {
		t.Fatalf("replay commit error = %v", err)
	}

	service = NewService(&scriptedDatabase{tx: &scriptedTx{queryRow: func(string, ...any) pgx.Row { return scriptedRow{err: pgx.ErrNoRows} }}}, key)
	service.random = &testRandomReader{failAt: 1}
	if _, _, err := service.idempotent(t.Context(), "owner", "operation", "key", "request", func(pgx.Tx) (any, error) {
		return map[string]string{"ok": "yes"}, nil
	}); err == nil || !strings.Contains(err.Error(), "random source failed") {
		t.Fatalf("idempotency nonce error = %v", err)
	}
	if _, err := NewService(nil, []byte("short")).encryptReplay([]byte("x"), "owner", "op", "key"); err == nil {
		t.Fatal("encryptReplay accepted an invalid APP_KEY length")
	}
}
