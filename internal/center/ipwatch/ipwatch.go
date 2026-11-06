// Package ipwatch maintains measurement-ordered public-IP history atomically
// with result ingestion. It implements agentapi.ResultHook.
package ipwatch

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jooservices/speedtest/internal/center/agentapi"
	"github.com/jooservices/speedtest/internal/center/geo"
)

// Watcher uses transaction-local geo caching and shared provider rate limits.
// Callers must serialize ingestion by locking the node before result insertion.
type Watcher struct {
	Offline geo.Geo
	// Reader is the offline reader behind Offline; the center runs its database updaters on it.
	Reader    *geo.Reader
	Fallbacks []geo.Fallback
	// Consensus asks every fallback provider and keeps the majority answer (SPEEDTEST_GEO_CONSENSUS).
	Consensus bool
	Logger    *slog.Logger
}

// Open configures offline and fallback geolocation from the documented env keys.
func Open(logger *slog.Logger) (*Watcher, func() error, error) {
	cfg, err := geo.ProviderConfigFromEnv()
	if err != nil {
		return nil, nil, err
	}
	providers, err := geo.NewFallbackProviders(cfg)
	if err != nil {
		return nil, nil, err
	}
	reader, err := geo.Open(geo.ConfigFromEnv(), logger)
	if err != nil {
		return nil, nil, err
	}
	return &Watcher{Offline: reader, Reader: reader, Fallbacks: providers, Consensus: cfg.Consensus, Logger: logger}, reader.Close, nil
}

// boundCache retains the inet key already locked by ingestion. Resolver may
// present an IPv4-mapped IPv6 address to providers as IPv4; cache persistence
// must still target the original result address, not a second unlocked row.
type boundCache struct {
	geo.Cache
	ip string
}

func (c boundCache) Get(ctx context.Context, _ string) (geo.Location, string, bool, error) {
	return c.Cache.Get(ctx, c.ip)
}
func (c boundCache) Put(ctx context.Context, _ string, l geo.Location, source string) error {
	return c.Cache.Put(ctx, c.ip, l, source)
}

type position struct{ id, ip string }

// neighbor reads only one adjacent sample using the (node_id,ts,id) index.
func neighbor(ctx context.Context, tx pgx.Tx, node string, ts time.Time, id string, newer bool) (*position, error) {
	op, order := "<", "DESC"
	if newer {
		op, order = ">", "ASC"
	}
	var p position
	err := tx.QueryRow(ctx, `SELECT id::text,host(ip) FROM results WHERE node_id=$1 AND ip IS NOT NULL AND (ts,id) `+op+` ($2,$3::uuid) ORDER BY ts `+order+`,id `+order+` LIMIT 1`, node, ts, id).Scan(&p.id, &p.ip)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &p, err
}

func setBoundary(ctx context.Context, tx pgx.Tx, node, id, ip string, previous *position) error {
	if previous != nil && previous.ip == ip {
		_, err := tx.Exec(ctx, `DELETE FROM node_ip_history WHERE id=$1`, id)
		return err
	}
	var from *string
	if previous != nil {
		from = &previous.ip
	}
	_, err := tx.Exec(ctx, `INSERT INTO node_ip_history(id,node_id,ts,from_ip,to_ip)
 SELECT id,node_id,ts,$2::inet,ip FROM results WHERE id=$1 AND node_id=$3
 ON CONFLICT(id) DO UPDATE SET from_ip=EXCLUDED.from_ip`, id, from, node)
	return err
}

// AfterResultStored repairs at most two boundaries. The result is already stored
// and replays never reach this method. Every write uses the caller's transaction.
func (w *Watcher) AfterResultStored(ctx context.Context, tx pgx.Tx, node string, r agentapi.Result) error {
	if r.IP == nil {
		return nil
	}
	// Read the stored timestamp, which has PostgreSQL microsecond precision.
	var ts time.Time
	var ip string
	if err := tx.QueryRow(ctx, `SELECT ts,host(ip) FROM results WHERE id=$1 AND node_id=$2 AND ip IS NOT NULL`, r.ID, node).Scan(&ts, &ip); err != nil {
		return err
	}
	before, err := neighbor(ctx, tx, node, ts, r.ID, false)
	if err != nil {
		return err
	}
	after, err := neighbor(ctx, tx, node, ts, r.ID, true)
	if err != nil {
		return err
	}
	if err = setBoundary(ctx, tx, node, r.ID, ip, before); err != nil {
		return err
	}
	if after != nil {
		if err = setBoundary(ctx, tx, node, after.id, after.ip, &position{id: r.ID, ip: ip}); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO node_ip_addresses(node_id,ip,tests,first_seen,last_seen,speed_tests,down_sum,up_sum)
 VALUES ($1,$2,1,$3,$3,CASE WHEN $4 THEN 1 ELSE 0 END,CASE WHEN $4 THEN $5::numeric ELSE 0 END,CASE WHEN $4 THEN $6::numeric ELSE 0 END)
 ON CONFLICT(node_id,ip) DO UPDATE SET tests=node_ip_addresses.tests+1,
	first_seen=least(node_ip_addresses.first_seen,EXCLUDED.first_seen), last_seen=greatest(node_ip_addresses.last_seen,EXCLUDED.last_seen),
	speed_tests=node_ip_addresses.speed_tests+EXCLUDED.speed_tests, down_sum=node_ip_addresses.down_sum+EXCLUDED.down_sum, up_sum=node_ip_addresses.up_sum+EXCLUDED.up_sum`, node, ip, ts, r.Status != "failed", r.Down, r.Up); err != nil {
		return err
	}
	// Lock the shared address before using a transaction-local cache. A pool-backed
	// cache would deadlock on the address placeholder inserted by this transaction.
	if _, err = tx.Exec(ctx, `SELECT ip FROM ip_addresses WHERE ip=$1 FOR NO KEY UPDATE`, ip); err != nil {
		return err
	}
	resolver := geo.NewResolver(boundCache{Cache: geo.NewPostgresCache(tx), ip: ip}, w.Offline, w.Fallbacks...).WithConsensus(w.Consensus, w.Logger)
	if _, err = resolver.Lookup(ctx, ip); err != nil {
		if !errors.Is(err, geo.ErrNotFound) {
			return err
		}
		w.logger().InfoContext(ctx, "public IP location unavailable", "node_id", node, "result_id", r.ID)
	}
	if after != nil {
		w.logger().InfoContext(ctx, "late IP observation reconciled without change signal", "node_id", node, "result_id", r.ID)
		return nil
	}
	if before == nil || before.ip == ip {
		return nil
	}
	// The outbox survives restarts; a notification only wakes the consumer. Both
	// become visible at commit, and neither survives a rolled-back result.
	if _, err = tx.Exec(ctx, `INSERT INTO ip_change_signals(result_id,node_id,ts,from_ip,to_ip) VALUES ($1,$2,$3,$4,$5)`, r.ID, node, ts, before.ip, ip); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `SELECT pg_notify('ip_changed',$1)`, r.ID)
	return err
}

func (w *Watcher) logger() *slog.Logger {
	if w.Logger != nil {
		return w.Logger
	}
	return slog.Default()
}
