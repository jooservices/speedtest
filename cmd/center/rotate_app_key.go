package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/shared/config"
)

// runRotateAppKey is an offline operation: callers must stop all center writers first.
// Keys are supplied together through the environment or as two lines on stdin.
func runRotateAppKey(ctx context.Context, args []string, input io.Reader, output io.Writer) error {
	if len(args) != 0 {
		return errors.New("usage: center rotate-app-key (keys via OLD_APP_KEY and NEW_APP_KEY, or two stdin lines; backup first)")
	}
	if err := loadDotEnv(); err != nil {
		return err
	}
	oldKey, newKey, err := readRotationKeys(input, os.Getenv("OLD_APP_KEY"), os.Getenv("NEW_APP_KEY"))
	if err != nil {
		return err
	}
	cfg, err := config.LoadCenter()
	if err != nil {
		return errors.New("invalid rotation database configuration")
	}
	if err := validateLocalDatabaseName(cfg); err != nil {
		return err
	}
	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		return errors.New("could not connect to the rotation database")
	}
	defer pool.Close()
	if err := verifyLocalDatabaseIdentity(ctx, pool, cfg); err != nil {
		return err
	}
	count, err := rotateAppKey(ctx, pool, oldKey, newKey)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "APP_KEY rotation committed: %d values verified; %d APP_KEY-HMAC idempotency records cleared (pre-rotation retries are new requests); update APP_KEY before restarting the center\n", count.values, count.cleared)
	return err
}

func readRotationKeys(input io.Reader, oldValue, newValue string) ([]byte, []byte, error) {
	if (oldValue == "") != (newValue == "") {
		return nil, nil, errors.New("set both OLD_APP_KEY and NEW_APP_KEY, or neither for stdin")
	}
	if oldValue == "" {
		// A key line is at most 51 bytes including the optional base64: prefix.
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 128), 128)
		if !scanner.Scan() {
			return nil, nil, errors.New("read OLD_APP_KEY from the first stdin line")
		}
		oldValue = scanner.Text()
		if !scanner.Scan() {
			return nil, nil, errors.New("read NEW_APP_KEY from the second stdin line")
		}
		newValue = scanner.Text()
	}
	oldKey, err := base64.StdEncoding.Strict().DecodeString(strings.TrimPrefix(oldValue, "base64:"))
	if err != nil || len(oldKey) != 32 {
		return nil, nil, errors.New("OLD_APP_KEY must be base64 encoding of exactly 32 bytes")
	}
	newKey, err := base64.StdEncoding.Strict().DecodeString(strings.TrimPrefix(newValue, "base64:"))
	if err != nil || len(newKey) != 32 {
		return nil, nil, errors.New("NEW_APP_KEY must be base64 encoding of exactly 32 bytes")
	}
	if bytes.Equal(oldKey, newKey) {
		return nil, nil, errors.New("OLD_APP_KEY and NEW_APP_KEY must differ")
	}
	return oldKey, newKey, nil
}

func rotationCipher(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, errors.New("rotation requires 32-byte keys")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("cannot initialize rotation encryption")
	}
	return cipher.NewGCM(block)
}

type rotationDatabase interface {
	Begin(context.Context) (pgx.Tx, error)
}

type rotationCounts struct {
	values  int
	cleared int64
}

func rotateAppKey(ctx context.Context, pool rotationDatabase, oldKey, newKey []byte) (rotationCounts, error) {
	oldCipher, err := rotationCipher(oldKey)
	if err != nil {
		return rotationCounts{}, err
	}
	newCipher, err := rotationCipher(newKey)
	if err != nil {
		return rotationCounts{}, err
	}
	if bytes.Equal(oldKey, newKey) {
		return rotationCounts{}, errors.New("rotation keys must differ")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return rotationCounts{}, errors.New("could not begin APP_KEY rotation")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	// Block readers/writers until commit; the runbook also requires stopping old-key processes.
	if _, err = tx.Exec(ctx, `LOCK TABLE installer_jobs, installer_idempotency, settings, api_idempotency IN ACCESS EXCLUSIVE MODE`); err != nil {
		return rotationCounts{}, errors.New("could not lock APP_KEY storage")
	}
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM installer_jobs WHERE status IN ('pending','running'))`).Scan(&active); err != nil {
		return rotationCounts{}, errors.New("could not check active installation jobs")
	}
	if active {
		return rotationCounts{}, errors.New("APP_KEY rotation refused: installation jobs are queued/running; finish them before retrying")
	}
	cleared, err := tx.Exec(ctx, `DELETE FROM installer_idempotency`)
	if err != nil {
		return rotationCounts{}, errors.New("could not clear APP_KEY-HMAC idempotency records")
	}
	total := 0
	for _, table := range []struct{ query, update string }{
		{`SELECT ctid::text, secret_ciphertext, '', '', '' FROM settings WHERE secret_ciphertext IS NOT NULL AND key <> 'enrollment_key'`, `UPDATE settings SET secret_ciphertext=$2 WHERE ctid=$1::tid RETURNING secret_ciphertext`},
		{`SELECT ctid::text, response_ciphertext, principal, operation, key_hash FROM api_idempotency`, `UPDATE api_idempotency SET response_ciphertext=$2 WHERE ctid=$1::tid RETURNING response_ciphertext`},
	} {
		count, err := rotateTable(ctx, tx, table.query, table.update, oldCipher, newCipher)
		if err != nil {
			return rotationCounts{}, err
		}
		total += count
	}
	if err := tx.Commit(ctx); err != nil {
		return rotationCounts{}, errors.New("could not commit APP_KEY rotation; verify database state before retry")
	}
	return rotationCounts{values: total, cleared: cleared.RowsAffected()}, nil
}

type rotationRow struct {
	id                            string
	encrypted                     []byte
	principal, operation, keyHash string
}

// rotateTable bounds working memory to 100 records and verifies the stored bytes, not just the generated value.
func rotateTable(ctx context.Context, tx pgx.Tx, query, update string, oldCipher, newCipher cipher.AEAD) (int, error) {
	if _, err := tx.Exec(ctx, "DECLARE rotation_cursor NO SCROLL CURSOR FOR "+query); err != nil {
		return 0, errors.New("could not read APP_KEY storage")
	}
	total := 0
	for {
		rows, err := tx.Query(ctx, `FETCH FORWARD 100 FROM rotation_cursor`)
		if err != nil {
			return 0, errors.New("could not fetch APP_KEY values")
		}
		batch, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (rotationRow, error) {
			var value rotationRow
			err := row.Scan(&value.id, &value.encrypted, &value.principal, &value.operation, &value.keyHash)
			return value, err
		})
		if err != nil {
			return 0, errors.New("could not decode APP_KEY storage")
		}
		if len(batch) == 0 {
			break
		}
		for _, row := range batch {
			var aad []byte
			if row.principal != "" {
				aad = []byte(row.principal + "\x00" + row.operation + "\x00" + row.keyHash)
			}
			plain, err := openRotationValue(oldCipher, row.encrypted, aad)
			if err != nil {
				return 0, err
			}
			nonce := make([]byte, newCipher.NonceSize())
			if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
				return 0, errors.New("could not generate rotation nonce")
			}
			encrypted := newCipher.Seal(nonce, nonce, plain, aad)
			var stored []byte
			if err := tx.QueryRow(ctx, update, row.id, encrypted).Scan(&stored); err != nil {
				return 0, errors.New("could not write rotated APP_KEY value")
			}
			verified, err := openRotationValue(newCipher, stored, aad)
			if err != nil || !bytes.Equal(plain, verified) {
				return 0, errors.New("rotated APP_KEY value failed verification")
			}
			total++
		}
	}
	if _, err := tx.Exec(ctx, `CLOSE rotation_cursor`); err != nil {
		return 0, errors.New("could not close APP_KEY storage cursor")
	}
	return total, nil
}

func openRotationValue(aead cipher.AEAD, encrypted, aad []byte) ([]byte, error) {
	if len(encrypted) < aead.NonceSize()+aead.Overhead() {
		return nil, errors.New("stored APP_KEY value is invalid; rotation rolled back")
	}
	plain, err := aead.Open(nil, encrypted[:aead.NonceSize()], encrypted[aead.NonceSize():], aad)
	if err != nil {
		return nil, errors.New("stored APP_KEY value cannot be decrypted; rotation rolled back")
	}
	return plain, nil
}
