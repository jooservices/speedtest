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
	"github.com/jooservices/speedtest/internal/center/geo/budget"
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
	highValue bool
	log       *slog.Logger
	purpose   budget.Purpose
}

// ErrBudgetExhausted means a configured free-provider quota refuses the request before HTTP is attempted.
var ErrBudgetExhausted = errors.New("geolocation provider budget exhausted")

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

// WithPurpose applies the interactive or background reservation policy to fallback requests.
func (r *Resolver) WithPurpose(purpose budget.Purpose) *Resolver {
	r.purpose = purpose
	return r
}

// WithHighValue enables the full provider chain for durable node addresses.
func (r *Resolver) WithHighValue(enabled bool) *Resolver {
	r.highValue = enabled
	return r
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
	best, source, score, done, err := r.cacheCandidate(ctx, ip)
	if err != nil || done {
		return best, err
	}
	best, source, score, done, err = r.offlineCandidate(ctx, ip, best, source, score)
	if err != nil || done {
		return best, err
	}
	return r.lookupProviders(ctx, ip, best, source, score)
}

func (r *Resolver) cacheCandidate(ctx context.Context, ip string) (Location, string, int, bool, error) {
	if r.cache == nil {
		return Location{}, "", 0, false, nil
	}
	location, source, found, err := r.cache.Get(ctx, ip)
	if err != nil {
		return Location{}, "", 0, false, errors.New("geolocation cache read failed")
	}
	if !found {
		return Location{}, "", 0, false, nil
	}
	location.QualityScore = QualityScore(location, source, location.AgeDays)
	return location, source, location.QualityScore, location.QualityScore >= QualityThreshold, nil
}

func (r *Resolver) offlineCandidate(ctx context.Context, ip string, best Location, source string, score int) (Location, string, int, bool, error) {
	if r.offline == nil {
		return best, source, score, false, nil
	}
	location, err := r.offline.Lookup(ctx, ip)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			return Location{}, "", 0, false, err
		}
		return best, source, score, false, nil
	}
	best, source = MergeLocations(best, source, location, GeoLiteSource)
	score = QualityScore(best, source, 0)
	if score < QualityThreshold {
		return best, source, score, false, nil
	}
	location, err = r.store(ctx, ip, best, source)
	return location, source, score, true, err
}

func (r *Resolver) lookupProviders(ctx context.Context, ip string, best Location, bestSource string, bestScore int) (Location, error) {
	providers := r.fallbacks
	if r.consensus && r.highValue && len(providers) > 1 {
		if bestScore > 0 {
			if _, err := r.store(ctx, ip, best, bestSource); err != nil {
				return Location{}, err
			}
		}
		return r.lookupConsensus(ctx, ip)
	}
	var failures []error
	budgetExhausted := false
	for _, provider := range providers {
		if err := ctx.Err(); err != nil {
			return Location{}, err
		}
		location, err := lookupFallback(ctx, provider, ip, r.purpose)
		if err == nil {
			best, bestSource = MergeLocations(best, bestSource, location, provider.Source())
			bestScore = QualityScore(best, bestSource, 0)
			if bestScore >= QualityThreshold || !r.highValue {
				return r.store(ctx, ip, best, bestSource)
			}
			continue
		}
		if errors.Is(err, ErrBudgetExhausted) {
			budgetExhausted = true
			continue
		}
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Location{}, err
		}
		failures = append(failures, err)
	}
	if bestScore > 0 {
		return r.store(ctx, ip, best, bestSource)
	}
	if budgetExhausted {
		return Location{}, ErrBudgetExhausted
	}
	if len(failures) != 0 {
		return Location{}, errors.Join(append([]error{ErrNotFound}, failures...)...)
	}
	return Location{}, ErrNotFound
}

func lookupFallback(ctx context.Context, provider Fallback, ip string, purpose budget.Purpose) (Location, error) {
	if limited, ok := provider.(interface {
		LookupFor(context.Context, string, budget.Purpose) (Location, error)
	}); ok {
		return limited.LookupFor(ctx, ip, purpose)
	}
	return provider.Lookup(ctx, ip)
}

func (r *Resolver) store(ctx context.Context, ip string, location Location, source string) (Location, error) {
	location, source = MergeLocations(Location{}, "", location, source)
	if r.cache != nil {
		if merger, ok := r.cache.(interface {
			Merge(context.Context, string, Location, string) (Location, string, error)
		}); ok {
			merged, _, err := merger.Merge(ctx, ip, location, source)
			if err != nil {
				return Location{}, errors.New("geolocation cache write failed")
			}
			return merged, nil
		}
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
	Begin(context.Context) (pgx.Tx, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// NewPostgresCache wraps a pgx pool or transaction.
func NewPostgresCache(db PGXDB) *PostgresCache { return &PostgresCache{db: db} }

func (c *PostgresCache) Get(ctx context.Context, ip string) (Location, string, bool, error) {
	var raw []byte
	var source string
	var lookedUp bool
	var lookedUpAt time.Time
	err := c.db.QueryRow(ctx, `SELECT geo, source, looked_up_at IS NOT NULL, COALESCE(looked_up_at,now()) FROM ip_addresses WHERE ip = $1::inet`, ip).Scan(&raw, &source, &lookedUp, &lookedUpAt)
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
	location.AgeDays = max(0, int(time.Since(lookedUpAt).Hours()/24))
	location.QualityScore = QualityScore(location, source, location.AgeDays)
	return location, source, true, nil
}

func (c *PostgresCache) Put(ctx context.Context, ip string, location Location, source string) error {
	_, _, err := c.Merge(ctx, ip, location, source)
	return err
}

func (c *PostgresCache) Merge(ctx context.Context, ip string, location Location, source string) (Location, string, error) {
	if source == "" {
		return Location{}, "", errors.New("geolocation source is required")
	}
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return Location{}, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `INSERT INTO ip_addresses(ip) VALUES($1::inet) ON CONFLICT DO NOTHING`, ip); err != nil {
		return Location{}, "", errors.New("cannot prepare geolocation cache")
	}
	var raw []byte
	var oldSource string
	var lookedUp bool
	var lookedUpAt time.Time
	if err = tx.QueryRow(ctx, `SELECT geo,source,looked_up_at IS NOT NULL,COALESCE(looked_up_at,now()) FROM ip_addresses WHERE ip=$1::inet FOR UPDATE`, ip).
		Scan(&raw, &oldSource, &lookedUp, &lookedUpAt); err != nil {
		return Location{}, "", errors.New("cannot read geolocation cache")
	}
	var existing Location
	if lookedUp {
		if err = json.Unmarshal(raw, &existing); err != nil {
			return Location{}, "", errors.New("invalid cached geolocation")
		}
		existing.AgeDays = max(0, int(time.Since(lookedUpAt).Hours()/24))
	}
	baseline, baselineSource := MergeLocations(Location{}, "", existing, oldSource)
	merged, mergedSource := MergeLocations(existing, oldSource, location, source)
	refreshed := (!lookedUp && !emptyLocation(merged)) || !sameLocationData(baseline, merged) || baselineSource != mergedSource
	var lastObserved any
	if lookedUp {
		lastObserved = lookedUpAt
	}
	if refreshed {
		lastObserved = time.Now().UTC()
		merged.AgeDays = 0
	} else if lookedUp {
		merged.AgeDays = max(0, int(time.Since(lookedUpAt).Hours()/24))
	}
	merged.QualityScore = QualityScore(merged, mergedSource, merged.AgeDays)
	data, err := json.Marshal(merged)
	if err != nil {
		return Location{}, "", errors.New("cannot encode geolocation")
	}
	_, err = tx.Exec(ctx, `UPDATE ip_addresses SET geo=$2::jsonb,source=$3,looked_up_at=$4 WHERE ip=$1::inet`, ip, data, mergedSource, lastObserved)
	if err != nil {
		return Location{}, "", errors.New("cannot persist geolocation")
	}
	if err = tx.Commit(ctx); err != nil {
		return Location{}, "", errors.New("cannot commit geolocation cache")
	}
	return merged, mergedSource, nil
}

func sameLocationData(a, b Location) bool {
	a.AgeDays, b.AgeDays = 0, 0
	a.QualityScore, b.QualityScore = 0, 0
	return a == b
}
