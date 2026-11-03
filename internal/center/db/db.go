// Package db provides the center's PostgreSQL connection pool and migrations.
package db

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/jooservices/speedtest/internal/shared/config"
	"github.com/jooservices/speedtest/migrations"
)

// Open connects and pings a bounded pool. Every connection uses UTC, regardless
// of the server's default timezone. Callers own Close. Errors never expose a DSN.
func Open(ctx context.Context, cfg config.DatabaseConfig) (*pgxpool.Pool, error) {
	u := url.URL{Scheme: "postgres", User: url.UserPassword(cfg.User, cfg.Password), Host: net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)), Path: "/" + cfg.Name}
	q := url.Values{"sslmode": {cfg.SSLMode}, "timezone": {"UTC"}, "pool_max_conns": {"4"}}
	u.RawQuery = q.Encode()
	p, err := pgxpool.New(ctx, u.String())
	if err != nil {
		return nil, errors.New("invalid PostgreSQL connection configuration")
	}
	if err := p.Ping(ctx); err != nil {
		p.Close()
		return nil, errors.New("cannot connect to PostgreSQL")
	}
	return p, nil
}

// Migrate applies all pending versions on up, or rolls back one version on down.
// Goose keeps the version journal and serializes independent migration processes
// with a PostgreSQL session advisory lock. SQL and version writes are atomic.
func Migrate(ctx context.Context, pool *pgxpool.Pool, direction string) error {
	if direction != "up" && direction != "down" {
		return errors.New("migration direction must be up or down")
	}
	if direction == "down" {
		var applied bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM goose_db_version WHERE is_applied AND version_id>0)`).Scan(&applied); err != nil || !applied {
			return errors.New("PostgreSQL migration failed; no migration is applied")
		}
	}
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()
	locker, lockErr := lock.NewPostgresSessionLocker()
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.Files, goose.WithSessionLocker(locker))
	if lockErr != nil || err != nil {
		return errors.New("cannot initialize embedded migrations")
	}
	if direction == "up" {
		_, err = provider.Up(ctx)
	} else {
		_, err = provider.Down(ctx)
	}
	if err != nil {
		return errors.New("PostgreSQL migration failed; verify database state and dependencies")
	}
	return nil
}
