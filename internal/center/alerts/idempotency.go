package alerts

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

const createRuleOperation = "create-alert-rule"

// CreateRule implements the OpenAPI 24-hour idempotency contract. The replay
// response is encrypted in the existing api_idempotency store.
func (s *Store) CreateRule(ctx context.Context, principal, key string, in RuleInput) (json.RawMessage, bool, error) {
	clean, err := NormalizeRule(in)
	if err != nil {
		return nil, false, err
	}
	if principal == "" || key == "" || len(key) > 128 {
		return nil, false, ErrInvalid
	}
	if len(s.AppKey) != 32 {
		return nil, false, errors.New("alert idempotency key is unavailable")
	}
	requestJSON, err := json.Marshal(clean)
	if err != nil {
		return nil, false, err
	}
	requestSum := sha256.Sum256(requestJSON)
	keySum := sha256.Sum256([]byte(key))
	keyHash := hex.EncodeToString(keySum[:])
	lockSum := sha256.Sum256([]byte(principal + "\x00" + createRuleOperation + "\x00" + keyHash))
	lockID := int64(binary.BigEndian.Uint64(lockSum[:8]))

	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockID); err != nil {
		return nil, false, err
	}
	var storedHash string
	var ciphertext []byte
	err = tx.QueryRow(ctx, `SELECT request_hash,response_ciphertext FROM api_idempotency
		WHERE principal=$1 AND operation=$2 AND key_hash=$3 AND expires_at>now() FOR UPDATE`, principal, createRuleOperation, keyHash).Scan(&storedHash, &ciphertext)
	if err == nil {
		if storedHash != hex.EncodeToString(requestSum[:]) {
			return nil, false, ErrConflict
		}
		response, decryptErr := s.decryptReplay(ciphertext, principal, keyHash)
		if decryptErr != nil {
			return nil, false, decryptErr
		}
		if err = tx.Commit(ctx); err != nil {
			return nil, false, err
		}
		return json.RawMessage(response), true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM api_idempotency WHERE principal=$1 AND operation=$2 AND key_hash=$3 AND expires_at<=now()`, principal, createRuleOperation, keyHash); err != nil {
		return nil, false, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM api_idempotency WHERE ctid IN
		(SELECT ctid FROM api_idempotency WHERE expires_at<=now() ORDER BY expires_at LIMIT 100 FOR UPDATE SKIP LOCKED)`); err != nil {
		return nil, false, err
	}
	return s.storeNewRule(ctx, tx, clean, principal, keyHash, requestSum)
}

func (s *Store) createRuleTx(ctx context.Context, tx pgx.Tx, in RuleInput) (Rule, error) {
	if err := checkNodeIDs(ctx, tx, in); err != nil {
		return Rule{}, err
	}
	id, err := s.newUUID()
	if err != nil {
		return Rule{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO alert_rules(id,name,metric,op,value,for_tests,node_ids,severity,channels,enabled,engine)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NULLIF($11,''))`, id, in.Name, in.Metric, in.Op, in.Value, in.ForTests,
		ruleNodeIDs(in), in.Severity, in.Channels, in.Enabled, in.Engine); err != nil {
		return Rule{}, err
	}
	r, err := scanRule(tx.QueryRow(ctx, `SELECT `+ruleColumns+` FROM alert_rules WHERE id=$1`, id))
	return r, err
}

func (s *Store) newUUID() (string, error) {
	var value [16]byte
	if _, err := io.ReadFull(s.random(), value[:]); err != nil {
		return "", fmt.Errorf("generate alert identifier: %w", err)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}

func (s *Store) encryptReplay(plain []byte, principal, keyHash string) ([]byte, error) {
	block, err := aes.NewCipher(s.AppKey)
	if err != nil {
		return nil, errors.New("initialize alert idempotency encryption")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("initialize alert idempotency encryption")
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(s.random(), nonce); err != nil {
		return nil, errors.New("generate alert idempotency nonce")
	}
	return append(nonce, aead.Seal(nil, nonce, plain, idempotencyAAD(principal, keyHash))...), nil
}

func (s *Store) decryptReplay(ciphertext []byte, principal, keyHash string) ([]byte, error) {
	block, err := aes.NewCipher(s.AppKey)
	if err != nil {
		return nil, errors.New("initialize alert idempotency decryption")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil || len(ciphertext) < aead.NonceSize()+aead.Overhead() {
		return nil, errors.New("stored alert idempotency response is invalid")
	}
	nonce, encrypted := ciphertext[:aead.NonceSize()], ciphertext[aead.NonceSize():]
	plain, err := aead.Open(nil, nonce, encrypted, idempotencyAAD(principal, keyHash))
	if err != nil {
		return nil, errors.New("stored alert idempotency response cannot be decrypted")
	}
	return plain, nil
}

func idempotencyAAD(principal, keyHash string) []byte {
	return []byte(principal + "\x00" + createRuleOperation + "\x00" + keyHash)
}

func (s *Store) storeNewRule(ctx context.Context, tx pgx.Tx, clean RuleInput, principal, keyHash string, requestSum [32]byte) (json.RawMessage, bool, error) {
	rule, err := s.createRuleTx(ctx, tx, clean)
	if err != nil {
		return nil, false, err
	}
	responseJSON, err := json.Marshal(rule)
	if err != nil {
		return nil, false, err
	}
	sealed, err := s.encryptReplay(responseJSON, principal, keyHash)
	if err != nil {
		return nil, false, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO api_idempotency
		(principal,operation,key_hash,request_hash,response_ciphertext,expires_at)
		VALUES ($1,$2,$3,$4,$5,now()+interval '24 hours')`, principal, createRuleOperation, keyHash, hex.EncodeToString(requestSum[:]), sealed); err != nil {
		return nil, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return json.RawMessage(responseJSON), false, nil
}
