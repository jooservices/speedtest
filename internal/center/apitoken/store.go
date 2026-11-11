package apitoken

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrInvalidToken = errors.New("invalid API token")
	ErrNotFound     = errors.New("API token not found")
	ErrUnavailable  = errors.New("could not verify API token")
)

// DB is the database surface used by the token store.
type DB interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Store persists API tokens and tracks successful uses.
type Store struct {
	DB  DB
	Now func() time.Time
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Create stores a digest and returns the raw bearer token once.
func (s *Store) Create(ctx context.Context, userID string, input CreateInput) (Token, string, error) {
	input, valid := normalizedInput(input, s.now())
	if !valid {
		return Token{}, "", errors.New("invalid API token input")
	}
	id, raw, hash, err := issue()
	if err != nil {
		return Token{}, "", errors.New("could not issue API token")
	}
	var created time.Time
	err = s.DB.QueryRow(ctx, `INSERT INTO api_access_tokens(id,user_id,name,scope,token_hash,expires_at)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING created_at`, id, userID, input.Name, input.Scope, hash[:], input.ExpiresAt).Scan(&created)
	if err != nil {
		return Token{}, "", errors.New("could not create API token")
	}
	return Token{ID: id, Name: input.Name, Scope: input.Scope, CreatedAt: created.UTC(), ExpiresAt: input.ExpiresAt}, raw, nil
}

// List returns one page of the owner's token metadata, never a token value or hash.
func (s *Store) List(ctx context.Context, userID string, page, size int) ([]Token, int, error) {
	var total int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM api_access_tokens WHERE user_id=$1`, userID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.DB.Query(ctx, `SELECT id::text,name,scope,created_at,expires_at,last_used_at,revoked_at
		FROM api_access_tokens WHERE user_id=$1 ORDER BY created_at DESC,id LIMIT $2 OFFSET $3`, userID, size, (page-1)*size)
	if err != nil {
		return nil, 0, err
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Token, error) {
		var item Token
		err := row.Scan(&item.ID, &item.Name, &item.Scope, &item.CreatedAt, &item.ExpiresAt, &item.LastUsedAt, &item.RevokedAt)
		item.CreatedAt = item.CreatedAt.UTC()
		return item, err
	})
	if items == nil {
		items = []Token{}
	}
	return items, total, err
}

// Revoke revokes an active token owned by userID.
func (s *Store) Revoke(ctx context.Context, userID, id string) error {
	tag, err := s.DB.Exec(ctx, `UPDATE api_access_tokens SET revoked_at=now() WHERE id=$1 AND user_id=$2 AND revoked_at IS NULL`, id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Authenticate verifies a token in constant time and advances its last-used time.
func (s *Store) Authenticate(ctx context.Context, raw string) (Principal, error) {
	id, ok := parse(raw)
	if !ok {
		_ = matches(make([]byte, 32), raw)
		return Principal{}, ErrInvalidToken
	}
	var stored []byte
	var principal Principal
	err := s.DB.QueryRow(ctx, `SELECT t.token_hash,t.id::text,t.user_id::text,t.name,t.scope,u.email
		FROM api_access_tokens t JOIN users u ON u.id=t.user_id
		WHERE t.id=$1 AND t.revoked_at IS NULL AND (t.expires_at IS NULL OR t.expires_at>now())`, id).
		Scan(&stored, &principal.TokenID, &principal.UserID, &principal.Name, &principal.Scope, &principal.Email)
	if errors.Is(err, pgx.ErrNoRows) {
		stored = make([]byte, 32)
	}
	valid := matches(stored, raw)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrUnavailable
	}
	if !valid {
		return Principal{}, ErrInvalidToken
	}
	tag, err := s.DB.Exec(ctx, `UPDATE api_access_tokens SET last_used_at=now()
		WHERE id=$1 AND token_hash=$2 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>now())`, id, stored)
	if err != nil {
		return Principal{}, ErrUnavailable
	}
	if tag.RowsAffected() != 1 {
		return Principal{}, ErrInvalidToken
	}
	return principal, nil
}
