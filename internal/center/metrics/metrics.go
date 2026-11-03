// Package metrics exposes operator-facing gauges in the Prometheus text format.
// The figures are read from PostgreSQL at scrape time so staleness (no recent
// results, offline nodes, stuck notifications) is visible without logs.
package metrics

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
)

// Querier is the subset of the pool used for the gauges.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Collector reads the gauges.
type Collector struct {
	DB Querier
	// OfflineAfter is how long without a heartbeat marks an enabled node offline.
	OfflineAfter time.Duration
	Now          func() time.Time
}

// Snapshot is one scrape.
type Snapshot struct {
	NodesEnabled, NodesOffline       int64
	LastResultAgeSeconds             float64 // -1 when there are no results
	ResultsTotal                     int64
	OutboxPending, DeliveriesPending int64
	DeliveriesFailed                 int64
	OldestPendingDeliveryAgeSeconds  float64
	OpenAlerts                       int64
}

const query = `
SELECT
 (SELECT count(*) FROM nodes WHERE enabled),
 (SELECT count(*) FROM nodes WHERE enabled AND (last_seen_at IS NULL OR last_seen_at < $1)),
 COALESCE((SELECT extract(epoch FROM ($2::timestamptz - max(ts))) FROM results), -1),
 (SELECT count(*) FROM results),
 (SELECT count(*) FROM alert_notification_outbox WHERE consumed_at IS NULL),
 (SELECT count(*) FROM notification_deliveries WHERE status='pending'),
 (SELECT count(*) FROM notification_deliveries WHERE status='failed'),
 COALESCE((SELECT extract(epoch FROM ($2::timestamptz - min(created_at))) FROM notification_deliveries WHERE status='pending'), 0),
 (SELECT count(*) FROM alert_state WHERE active)`

// Collect takes one snapshot.
func (c Collector) Collect(ctx context.Context) (Snapshot, error) {
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	after := c.OfflineAfter
	if after <= 0 {
		after = 5 * time.Minute
	}
	var s Snapshot
	err := c.DB.QueryRow(ctx, query, now.Add(-after), now).Scan(&s.NodesEnabled, &s.NodesOffline,
		&s.LastResultAgeSeconds, &s.ResultsTotal, &s.OutboxPending, &s.DeliveriesPending,
		&s.DeliveriesFailed, &s.OldestPendingDeliveryAgeSeconds, &s.OpenAlerts)
	return s, err
}

// Write renders the snapshot in the Prometheus text exposition format.
func (s Snapshot) Write(w io.Writer) {
	g := func(name, help string, v any) {
		_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %v\n", name, help, name, name, v)
	}
	g("speedtest_nodes_enabled", "Enabled nodes.", s.NodesEnabled)
	g("speedtest_nodes_offline", "Enabled nodes without a recent heartbeat.", s.NodesOffline)
	g("speedtest_last_result_age_seconds", "Seconds since the newest result, -1 when none.", s.LastResultAgeSeconds)
	g("speedtest_results_total", "Stored results.", s.ResultsTotal)
	g("speedtest_notification_outbox_pending", "Alert events not yet turned into deliveries.", s.OutboxPending)
	g("speedtest_notification_deliveries_pending", "Notifications waiting to be sent.", s.DeliveriesPending)
	g("speedtest_notification_deliveries_failed", "Notifications that exhausted their retries.", s.DeliveriesFailed)
	g("speedtest_notification_oldest_pending_age_seconds", "Age of the oldest pending notification.", s.OldestPendingDeliveryAgeSeconds)
	g("speedtest_alerts_open", "Alert rules currently not ok.", s.OpenAlerts)
}
