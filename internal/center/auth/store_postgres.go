package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore persists users and sessions using the JOOST-10 schema.
type PostgresStore struct {
	pool authPool
}

type authPool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Begin(context.Context) (pgx.Tx, error)
}

func NewPostgresStore(pool *pgxpool.Pool) (*PostgresStore, error) {
	if pool == nil {
		return nil, errors.New("PostgreSQL pool is required")
	}
	return &PostgresStore{pool: pool}, nil
}

func (s *PostgresStore) CreateSession(ctx context.Context, tokenHash string, userID *string, csrfHash string, expires time.Time) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at<=now()`); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO sessions(token_hash,user_id,csrf_token_hash,expires_at)
		VALUES ($1,$2,$3,$4)`, tokenHash, userID, csrfHash, expires)
	return err
}

func (s *PostgresStore) GetSession(ctx context.Context, tokenHash string, now time.Time) (SessionRecord, error) {
	var record SessionRecord
	var userID, email pgtype.Text
	err := s.pool.QueryRow(ctx, `SELECT s.csrf_token_hash,s.expires_at,u.id::text,u.email
		FROM sessions s LEFT JOIN users u ON u.id=s.user_id
		WHERE s.token_hash=$1 AND s.expires_at>$2`, tokenHash, now).
		Scan(&record.CSRFTokenHash, &record.ExpiresAt, &userID, &email)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionRecord{}, ErrInvalidSession
	}
	if err != nil {
		return SessionRecord{}, err
	}
	if userID.Valid {
		record.Identity = &Identity{ID: userID.String, Email: email.String}
	}
	return record, nil
}

func (s *PostgresStore) FindUserByEmail(ctx context.Context, email string) (Credential, error) {
	return s.findUser(ctx, `SELECT id::text,email,password_hash FROM users WHERE lower(email)=lower($1)`, email)
}

func (s *PostgresStore) FindUserByID(ctx context.Context, id string) (Credential, error) {
	return s.findUser(ctx, `SELECT id::text,email,password_hash FROM users WHERE id=$1`, id)
}

func (s *PostgresStore) findUser(ctx context.Context, query string, value string) (Credential, error) {
	var user Credential
	err := s.pool.QueryRow(ctx, query, value).Scan(&user.ID, &user.Email, &user.PasswordHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return Credential{}, ErrNotFound
	}
	return user, err
}

func (s *PostgresStore) RotateAnonymousSession(ctx context.Context, oldHash, userID, newHash, csrfHash string, expires time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	deleted, err := tx.Exec(ctx, `DELETE FROM sessions
		WHERE token_hash=$1 AND user_id IS NULL AND expires_at>now()`, oldHash)
	if err != nil {
		return err
	}
	if deleted.RowsAffected() != 1 {
		return ErrInvalidSession
	}
	if _, err = tx.Exec(ctx, `INSERT INTO sessions(token_hash,user_id,csrf_token_hash,expires_at)
		VALUES ($1,$2,$3,$4)`, newHash, userID, csrfHash, expires); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) DeleteSession(ctx context.Context, tokenHash string) error {
	deleted, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash=$1`, tokenHash)
	if err != nil {
		return err
	}
	if deleted.RowsAffected() != 1 {
		return ErrInvalidSession
	}
	return nil
}

func (s *PostgresStore) ChangePassword(ctx context.Context, userID, oldPasswordHash, newPasswordHash, oldSessionHash, newSessionHash, newCSRFHash string, expires time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	updated, err := tx.Exec(ctx, `UPDATE users SET password_hash=$1,updated_at=now()
		WHERE id=$2 AND password_hash=$3`, newPasswordHash, userID, oldPasswordHash)
	if err != nil {
		return err
	}
	if updated.RowsAffected() != 1 {
		return ErrInvalidCurrentPass
	}
	if _, err = tx.Exec(ctx, `DELETE FROM sessions WHERE user_id=$1 AND token_hash<>$2`, userID, oldSessionHash); err != nil {
		return err
	}
	rotated, err := tx.Exec(ctx, `UPDATE sessions
		SET token_hash=$1,csrf_token_hash=$2,created_at=now(),expires_at=$3
		WHERE token_hash=$4 AND user_id=$5 AND expires_at>now()`, newSessionHash, newCSRFHash, expires, oldSessionHash, userID)
	if err != nil {
		return err
	}
	if rotated.RowsAffected() != 1 {
		return ErrInvalidSession
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) SeedAdmin(ctx context.Context, email, passwordHash string) error {
	id, err := randomUUID()
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO users(id,email,password_hash)
		VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, id, strings.ToLower(email), passwordHash)
	return err
}

func randomUUID() (string, error) {
	var value [16]byte
	if _, err := io.ReadFull(rand.Reader, value[:]); err != nil {
		return "", errors.New("generate user id")
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", encoded[:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:]), nil
}

// GetTwoFactor reads the second-factor state of an account.
func (s *PostgresStore) GetTwoFactor(ctx context.Context, userID string) (TwoFactor, error) {
	var record TwoFactor
	err := s.pool.QueryRow(ctx, `SELECT secret_enc, enabled, last_step, recovery_hashes FROM user_two_factor WHERE user_id=$1`, userID).
		Scan(&record.SecretEnc, &record.Enabled, &record.LastStep, &record.RecoveryHashes)
	if errors.Is(err, pgx.ErrNoRows) {
		return TwoFactor{}, ErrNotFound
	}
	return record, err
}

// SaveTwoFactorSetup stores a pending secret; it never touches an enabled row (the service refuses that case).
func (s *PostgresStore) SaveTwoFactorSetup(ctx context.Context, userID string, secretEnc []byte) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO user_two_factor(user_id, secret_enc) VALUES ($1,$2)
		ON CONFLICT (user_id) DO UPDATE SET secret_enc=EXCLUDED.secret_enc, enabled=false, last_step=0, recovery_hashes='{}', enabled_at=NULL
		WHERE user_two_factor.enabled=false`, userID, secretEnc)
	return err
}

// EnableTwoFactor switches a pending setup on; it changes nothing when none is pending.
func (s *PostgresStore) EnableTwoFactor(ctx context.Context, userID string, step int64, recoveryHashes []string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE user_two_factor SET enabled=true, last_step=$2, recovery_hashes=$3, enabled_at=now()
		WHERE user_id=$1 AND enabled=false`, userID, step, recoveryHashes)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrTwoFactorNotPending
	}
	return nil
}

func (s *PostgresStore) DeleteTwoFactor(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM user_two_factor WHERE user_id=$1`, userID)
	return err
}

// ConsumeTwoFactorStep advances last_step only forward, so two requests with the same code cannot both succeed.
func (s *PostgresStore) ConsumeTwoFactorStep(ctx context.Context, userID string, step int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE user_two_factor SET last_step=$2 WHERE user_id=$1 AND enabled AND last_step<$2`, userID, step)
	return err == nil && tag.RowsAffected() == 1, err
}

// ConsumeRecoveryCode removes one digest atomically.
func (s *PostgresStore) ConsumeRecoveryCode(ctx context.Context, userID, hash string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE user_two_factor SET recovery_hashes=array_remove(recovery_hashes,$2)
		WHERE user_id=$1 AND enabled AND $2 = ANY(recovery_hashes)`, userID, hash)
	return err == nil && tag.RowsAffected() == 1, err
}
