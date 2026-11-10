package budget

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB is the subset of the pgx pool or transaction the store needs, so tests can substitute a fake.
type DB interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// PostgresStore keeps the daily counts in geo_provider_usage, so a restart does not reset a quota.
type PostgresStore struct {
	db DB
}

// NewPostgresStore wraps db; the caller owns the pool and its lifetime.
func NewPostgresStore(db DB) *PostgresStore {
	return &PostgresStore{db: db}
}

// Add increments the day's count atomically with an upsert and returns the new total.
func (s *PostgresStore) Add(ctx context.Context, provider string, day time.Time, n int) (int, error) {
	var total int
	err := s.db.QueryRow(ctx, `INSERT INTO geo_provider_usage(provider, day, requests) VALUES ($1, $2::date, $3)
		ON CONFLICT (provider, day) DO UPDATE SET requests = geo_provider_usage.requests + EXCLUDED.requests
		RETURNING requests`, provider, day.UTC(), n).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("add geolocation usage: %w", err)
	}
	return total, nil
}

// Reserve atomically counts a request only when daily, minute, and provider cooldown limits allow it.
func (s *PostgresStore) Reserve(ctx context.Context, provider string, day, minute, now time.Time, dailyLimit, minuteLimit int) (bool, error) {
	var counted int
	err := s.db.QueryRow(ctx, `INSERT INTO geo_provider_usage(provider,day,requests,minute_start,minute_requests)
SELECT $1,$2::date,1,$3,1
WHERE (SELECT coalesce(max(blocked_until),'-infinity'::timestamptz) FROM geo_provider_usage WHERE provider=$1)<=$4
ON CONFLICT(provider,day) DO UPDATE SET
	requests=geo_provider_usage.requests+1,
	minute_requests=CASE WHEN geo_provider_usage.minute_start=$3 THEN geo_provider_usage.minute_requests+1 ELSE 1 END,
	minute_start=$3
WHERE ( $5=0 OR geo_provider_usage.requests < $5 )
	AND ( $6=0 OR geo_provider_usage.minute_start<>$3 OR geo_provider_usage.minute_requests<$6 )
	AND COALESCE(geo_provider_usage.blocked_until,'-infinity'::timestamptz)<=$4
RETURNING requests`, provider, day.UTC(), minute.UTC(), now.UTC(), dailyLimit, minuteLimit).Scan(&counted)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reserve geolocation usage: %w", err)
	}
	return true, nil
}

// BlockUntil persists provider supplied cooldown windows across restarts and UTC day changes.
func (s *PostgresStore) BlockUntil(ctx context.Context, provider string, now, until time.Time) error {
	_, err := s.db.Exec(ctx, `INSERT INTO geo_provider_usage(provider,day,blocked_until)
VALUES($1,$2::date,$3)
ON CONFLICT(provider,day) DO UPDATE SET blocked_until=GREATEST(COALESCE(geo_provider_usage.blocked_until,EXCLUDED.blocked_until),EXCLUDED.blocked_until)`,
		provider, utcDay(now), until.UTC())
	if err != nil {
		return fmt.Errorf("persist geolocation provider cooldown: %w", err)
	}
	return nil
}

// Used returns the day's count, or zero when no request was counted that day.
func (s *PostgresStore) Used(ctx context.Context, provider string, day time.Time) (int, error) {
	var total int
	err := s.db.QueryRow(ctx, `SELECT coalesce((SELECT requests FROM geo_provider_usage WHERE provider=$1 AND day=$2::date), 0)`,
		provider, day.UTC()).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("read geolocation usage: %w", err)
	}
	return total, nil
}
