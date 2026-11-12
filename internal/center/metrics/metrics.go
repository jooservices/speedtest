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
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
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
	Bandwidth                        []BandwidthUsage
	BandwidthBudgetRatios            []BandwidthBudgetRatio
}

// BandwidthUsage is the current UTC month's measured traffic for one node and engine.
type BandwidthUsage struct {
	NodeID, Engine string
	Bytes          float64
}

// BandwidthBudgetRatio is one configured monthly budget's current utilization.
type BandwidthBudgetRatio struct {
	Scope, NodeID string
	Ratio         float64
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
	if err != nil {
		return Snapshot{}, err
	}
	if s.Bandwidth, err = bandwidthUsage(ctx, c.DB, now); err != nil {
		return Snapshot{}, err
	}
	s.BandwidthBudgetRatios, err = bandwidthBudgetRatios(ctx, c.DB, now)
	return s, err
}

func bandwidthUsage(ctx context.Context, db Querier, now time.Time) ([]BandwidthUsage, error) {
	rows, err := db.Query(ctx, `SELECT node_id::text,engine,
 COALESCE(sum(COALESCE(bytes_down,0)::numeric+COALESCE(bytes_up,0)::numeric),0)::float8
 FROM results WHERE ts >= $1 AND ts < $2 GROUP BY node_id,engine ORDER BY node_id,engine`, monthStart(now), now.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []BandwidthUsage{}
	for rows.Next() {
		var item BandwidthUsage
		if err := rows.Scan(&item.NodeID, &item.Engine, &item.Bytes); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func bandwidthBudgetRatios(ctx context.Context, db Querier, now time.Time) ([]BandwidthBudgetRatio, error) {
	rows, err := db.Query(ctx, `SELECT CASE WHEN b.scope_key='global' THEN 'global' ELSE 'node' END,COALESCE(b.node_id::text,''),
 COALESCE(used.bytes,0)::float8/b.monthly_bytes
 FROM usage_budgets b LEFT JOIN LATERAL (
   SELECT sum(t.bytes_down::numeric+t.bytes_up::numeric) AS bytes FROM usage_monthly_totals t
   WHERE t.period_start=$1::date AND (b.scope_key='global' OR t.node_id=b.node_id)
 ) used ON true ORDER BY b.scope_key`, monthStart(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []BandwidthBudgetRatio{}
	for rows.Next() {
		var item BandwidthBudgetRatio
		if err := rows.Scan(&item.Scope, &item.NodeID, &item.Ratio); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func monthStart(now time.Time) time.Time {
	year, month, _ := now.UTC().Date()
	return time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
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
	_, _ = fmt.Fprintln(w, "# HELP speedtest_bandwidth_month_to_date_bytes Measured UTC month-to-date test traffic by node and engine.")
	_, _ = fmt.Fprintln(w, "# TYPE speedtest_bandwidth_month_to_date_bytes gauge")
	for _, usage := range s.Bandwidth {
		_, _ = fmt.Fprintf(w, "speedtest_bandwidth_month_to_date_bytes{node_id=%q,engine=%q} %g\n", usage.NodeID, usage.Engine, usage.Bytes)
	}
	_, _ = fmt.Fprintln(w, "# HELP speedtest_bandwidth_budget_ratio Fraction of a configured monthly traffic budget used.")
	_, _ = fmt.Fprintln(w, "# TYPE speedtest_bandwidth_budget_ratio gauge")
	for _, budget := range s.BandwidthBudgetRatios {
		_, _ = fmt.Fprintf(w, "speedtest_bandwidth_budget_ratio{scope=%q,node_id=%q} %g\n", budget.Scope, budget.NodeID, budget.Ratio)
	}
}
