package geo

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const GeoLiteSource = "geolite2"

// Cache stores a successful geolocation result for an IP address.
type Cache interface {
	Get(context.Context, string) (Location, string, bool, error)
	Put(context.Context, string, Location, string) error
}

// Fallback is a network-backed geolocation provider with a stable source name.
type Fallback interface {
	Geo
	Source() string
}

// Resolver checks the durable cache, then GeoLite2, then each configured
// fallback in order. Concurrent lookups for the same IP share one result.
type Resolver struct {
	cache     Cache
	offline   Geo
	fallbacks []Fallback
	mu        sync.Mutex
	active    map[string]*lookupCall
	consensus bool
	log       *slog.Logger
}

type lookupCall struct {
	done     chan struct{}
	location Location
	err      error
}

// NewResolver builds the cache -> offline -> fallback chain. A nil cache or
// offline reader is allowed for unit tests and callers without that component.
func NewResolver(cache Cache, offline Geo, fallbacks ...Fallback) *Resolver {
	return &Resolver{cache: cache, offline: offline, fallbacks: append([]Fallback(nil), fallbacks...), active: make(map[string]*lookupCall)}
}

// Lookup returns a cached result when present and persists the first successful
// offline or provider result. Invalid IPs never reach the cache or network.
func (r *Resolver) Lookup(ctx context.Context, address string) (Location, error) {
	if err := ctx.Err(); err != nil {
		return Location{}, err
	}
	ip := net.ParseIP(address)
	if ip == nil {
		return Location{}, ErrInvalidIP
	}
	canonical := ip.String()

	r.mu.Lock()
	if r.active == nil {
		r.active = make(map[string]*lookupCall)
	}
	if active, ok := r.active[canonical]; ok {
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return Location{}, ctx.Err()
		case <-active.done:
			return active.location, active.err
		}
	}
	active := &lookupCall{done: make(chan struct{})}
	r.active[canonical] = active
	r.mu.Unlock()

	active.location, active.err = r.lookup(ctx, canonical)
	r.mu.Lock()
	delete(r.active, canonical)
	close(active.done)
	r.mu.Unlock()
	return active.location, active.err
}

func (r *Resolver) lookup(ctx context.Context, ip string) (Location, error) {
	if r.cache != nil {
		location, _, found, err := r.cache.Get(ctx, ip)
		if err != nil {
			return Location{}, errors.New("geolocation cache read failed")
		}
		if found {
			return location, nil
		}
	}

	if r.offline != nil {
		location, err := r.offline.Lookup(ctx, ip)
		if err == nil {
			return r.store(ctx, ip, location, GeoLiteSource)
		}
		if !errors.Is(err, ErrNotFound) {
			return Location{}, err
		}
	}

	if r.consensus && len(r.fallbacks) > 1 {
		return r.lookupConsensus(ctx, ip)
	}
	var failures []error
	for _, provider := range r.fallbacks {
		if err := ctx.Err(); err != nil {
			return Location{}, err
		}
		location, err := provider.Lookup(ctx, ip)
		if err == nil {
			return r.store(ctx, ip, location, provider.Source())
		}
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Location{}, err
		}
		failures = append(failures, err)
	}
	if len(failures) != 0 {
		return Location{}, errors.Join(append([]error{ErrNotFound}, failures...)...)
	}
	return Location{}, ErrNotFound
}

func (r *Resolver) store(ctx context.Context, ip string, location Location, source string) (Location, error) {
	if r.cache != nil {
		if err := r.cache.Put(ctx, ip, location, source); err != nil {
			return Location{}, errors.New("geolocation cache write failed")
		}
	}
	return location, nil
}

// PostgresCache persists lookup results in the existing ip_addresses table.
// The caller supplies its shared PostgreSQL pool; no service address is fixed.
type PostgresCache struct{ db PGXDB }

// PGXDB is the subset of pgx pool or transaction behavior used by the cache.
type PGXDB interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// NewPostgresCache wraps a pgx pool or transaction.
func NewPostgresCache(db PGXDB) *PostgresCache { return &PostgresCache{db: db} }

func (c *PostgresCache) Get(ctx context.Context, ip string) (Location, string, bool, error) {
	var raw []byte
	var source string
	var lookedUp bool
	err := c.db.QueryRow(ctx, `SELECT geo, source, looked_up_at IS NOT NULL FROM ip_addresses WHERE ip = $1::inet`, ip).Scan(&raw, &source, &lookedUp)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Location{}, "", false, nil
		}
		return Location{}, "", false, err
	}
	if !lookedUp {
		return Location{}, source, false, nil
	}
	var location Location
	if err := json.Unmarshal(raw, &location); err != nil {
		return Location{}, "", false, errors.New("invalid cached geolocation")
	}
	return location, source, true, nil
}

func (c *PostgresCache) Put(ctx context.Context, ip string, location Location, source string) error {
	if source == "" {
		return errors.New("geolocation source is required")
	}
	data, err := json.Marshal(location)
	if err != nil {
		return errors.New("cannot encode geolocation")
	}
	_, err = c.db.Exec(ctx, `
		INSERT INTO ip_addresses (ip, geo, source, looked_up_at)
		VALUES ($1::inet, $2::jsonb, $3, $4)
		ON CONFLICT (ip) DO UPDATE
		SET geo = EXCLUDED.geo, source = EXCLUDED.source, looked_up_at = EXCLUDED.looked_up_at
		WHERE ip_addresses.looked_up_at IS NULL`, ip, data, source, time.Now().UTC())
	if err != nil {
		return errors.New("cannot persist geolocation")
	}
	return nil
}
