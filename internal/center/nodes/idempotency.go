package nodes

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"
)

func (s *Service) idempotent(ctx context.Context, principal, operation, key string, request any, create func(pgx.Tx) (any, error)) (json.RawMessage, bool, error) {
	if principal == "" || key == "" || len(key) > 128 {
		return nil, false, ErrInvalidInput
	}
	if len(s.appKey) != 32 {
		return nil, false, ErrCryptoKeyUnset
	}
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return nil, false, err
	}
	requestSum := sha256.Sum256(requestJSON)
	keyHash := hashSecret(key)
	lockSum := sha256.Sum256([]byte(principal + "\x00" + operation + "\x00" + keyHash))
	lockID := int64(binary.BigEndian.Uint64(lockSum[:8]))

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockID); err != nil {
		return nil, false, err
	}
	var storedRequestHash string
	var storedResponse []byte
	err = tx.QueryRow(ctx, `SELECT request_hash,response_ciphertext FROM api_idempotency WHERE principal=$1 AND operation=$2 AND key_hash=$3 AND expires_at>now() FOR UPDATE`, principal, operation, keyHash).Scan(&storedRequestHash, &storedResponse)
	if err == nil {
		if !constantHashEqual(storedRequestHash, hex.EncodeToString(requestSum[:])) {
			return nil, false, ErrConflict
		}
		response, decryptErr := s.decryptReplay(storedResponse, principal, operation, keyHash)
		if decryptErr != nil {
			return nil, false, decryptErr
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, false, err
		}
		return json.RawMessage(response), true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM api_idempotency WHERE principal=$1 AND operation=$2 AND key_hash=$3 AND expires_at<=now()`, principal, operation, keyHash); err != nil {
		return nil, false, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM api_idempotency WHERE ctid IN (SELECT ctid FROM api_idempotency WHERE expires_at<=now() ORDER BY expires_at LIMIT 100 FOR UPDATE SKIP LOCKED)`); err != nil {
		return nil, false, err
	}
	data, err := create(tx)
	if err != nil {
		return nil, false, err
	}
	responseJSON, err := json.Marshal(data)
	if err != nil {
		return nil, false, err
	}
	ciphertext, err := s.encryptReplay(responseJSON, principal, operation, keyHash)
	if err != nil {
		return nil, false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO api_idempotency (principal,operation,key_hash,request_hash,response_ciphertext,expires_at) VALUES ($1,$2,$3,$4,$5,now()+interval '24 hours')`, principal, operation, keyHash, hex.EncodeToString(requestSum[:]), ciphertext)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return json.RawMessage(responseJSON), false, nil
}

func (s *Service) encryptReplay(plain []byte, principal, operation, keyHash string) ([]byte, error) {
	block, err := aes.NewCipher(s.appKey)
	if err != nil {
		return nil, fmt.Errorf("initialize idempotency encryption: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initialize idempotency encryption: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(s.random, nonce); err != nil {
		return nil, fmt.Errorf("generate idempotency nonce: %w", err)
	}
	return append(nonce, aead.Seal(nil, nonce, plain, idempotencyAAD(principal, operation, keyHash))...), nil
}

func (s *Service) decryptReplay(ciphertext []byte, principal, operation, keyHash string) ([]byte, error) {
	block, err := aes.NewCipher(s.appKey)
	if err != nil {
		return nil, fmt.Errorf("initialize idempotency decryption: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initialize idempotency decryption: %w", err)
	}
	if len(ciphertext) < aead.NonceSize()+aead.Overhead() {
		return nil, errors.New("stored idempotency response is invalid")
	}
	nonce, encrypted := ciphertext[:aead.NonceSize()], ciphertext[aead.NonceSize():]
	plain, err := aead.Open(nil, nonce, encrypted, idempotencyAAD(principal, operation, keyHash))
	if err != nil {
		return nil, errors.New("stored idempotency response cannot be decrypted")
	}
	return plain, nil
}

func idempotencyAAD(principal, operation, keyHash string) []byte {
	return []byte(principal + "\x00" + operation + "\x00" + keyHash)
}
