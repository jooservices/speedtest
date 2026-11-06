package telemetry

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// Database is the scrape-time pool and node source.
type Database interface {
	Stat() *pgxpool.Stat
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

type databaseCollector struct {
	db   Database
	desc []*prometheus.Desc
}

// RegisterDatabase adds bounded node ages and current pgx pool statistics.
func (t *Telemetry) RegisterDatabase(db Database) {
	c := &databaseCollector{db: db}
	for _, name := range []string{"acquired_connections", "idle_connections", "total_connections", "max_connections", "wait_count_total", "wait_duration_seconds_total"} {
		c.desc = append(c.desc, prometheus.NewDesc("speedtest_db_pool_"+name, "pgx pool "+name+".", nil, nil))
	}
	for _, name := range []string{"heartbeat", "last_result"} {
		c.desc = append(c.desc, prometheus.NewDesc("speedtest_node_"+name+"_age_seconds", "Seconds since node "+name+"; -1 when unknown. At most 200 nodes.", []string{"node"}, nil))
	}
	t.Registry.MustRegister(c)
}
func (c *databaseCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range c.desc {
		ch <- d
	}
}
func (c *databaseCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.db.Stat()
	for i, v := range []float64{float64(s.AcquiredConns()), float64(s.IdleConns()), float64(s.TotalConns()), float64(s.MaxConns()), float64(s.EmptyAcquireCount()), s.EmptyAcquireWaitTime().Seconds()} {
		kind := prometheus.GaugeValue
		if i >= 4 {
			kind = prometheus.CounterValue
		}
		ch <- prometheus.MustNewConstMetric(c.desc[i], kind, v)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rows, err := c.db.Query(ctx, `SELECT n.id::text,
 COALESCE(extract(epoch FROM (statement_timestamp()-n.last_seen_at)), -1)::float8,
 COALESCE(extract(epoch FROM (statement_timestamp()-(SELECT max(r.ts) FROM results r WHERE r.node_id=n.id))), -1)::float8
 FROM nodes n ORDER BY n.id LIMIT 200`)
	if err != nil {
		collectorError(ch, c.desc[6])
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var heartbeat, result float64
		if rows.Scan(&id, &heartbeat, &result) != nil {
			collectorError(ch, c.desc[6])
			return
		}
		ch <- prometheus.MustNewConstMetric(c.desc[6], prometheus.GaugeValue, heartbeat, id)
		ch <- prometheus.MustNewConstMetric(c.desc[7], prometheus.GaugeValue, result, id)
	}
	if rows.Err() != nil {
		collectorError(ch, c.desc[6])
	}
}
