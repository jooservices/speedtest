// Package dashboard builds the single-call dashboard snapshot: fleet tiles with the previous period, node cards,
// the latest results and the open alerts.
package dashboard

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jooservices/speedtest/internal/center/alerts"
	"github.com/jooservices/speedtest/internal/center/ownerapi"
	"github.com/jooservices/speedtest/internal/center/stats"
)

const (
	// RecentResults and OpenAlerts are the sizes of the lists in the snapshot.
	RecentResults = 9
	OpenAlerts    = 6
	// SparkHours and SparkPoints describe the node sparklines: the last 24 hours in half-hour buckets.
	SparkHours  = 24
	SparkPoints = 48
	nodePage    = 100
)

// Sources are what the snapshot is assembled from; the center wires the real stores.
type (
	// Nodes lists node views in a stable order.
	Nodes interface {
		List(ctx context.Context, page, size int) ([]map[string]any, int, error)
	}
	// Stats provides the per-node summaries and sparkline series.
	Stats interface {
		Summary(ctx context.Context, q stats.Query) ([]stats.SummaryComparison, error)
		Series(ctx context.Context, q stats.Query, percent bool, maxPoints int) ([]stats.Series, error)
	}
	// Results lists results.
	Results interface {
		ListResults(ctx context.Context, f ownerapi.ResultFilter, page, size int) ([]map[string]any, int, error)
	}
	// Alerts lists alert events.
	Alerts interface {
		ListEvents(ctx context.Context, filter alerts.EventFilter, page, size int) ([]alerts.Event, int, error)
	}
	// DB is the subset of the pool used for the fleet figures.
	DB interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	}
)

// Store assembles the dashboard.
type Store struct {
	DB      DB
	Nodes   Nodes
	Stats   Stats
	Results Results
	Alerts  Alerts
	Now     func() time.Time
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Fleet is the headline figures of one period.
type Fleet struct {
	Down    *float64 `json:"down"`
	Up      *float64 `json:"up"`
	Ping    *float64 `json:"ping"`
	Success *float64 `json:"success"`
	IPs     int      `json:"ips"`
	Count   int      `json:"count"`
}

// NodeCard is one node with its summary and 24 hour download sparkline.
type NodeCard struct {
	Node      map[string]any    `json:"node"`
	Summary   stats.NodeSummary `json:"summary"`
	Sparkline []stats.Point     `json:"sparkline"`
}

// Snapshot is the dashboard resource.
type Snapshot struct {
	Current        Fleet            `json:"current"`
	Previous       Fleet            `json:"previous"`
	Nodes          []NodeCard       `json:"nodes"`
	LatestResults  []map[string]any `json:"latestResults"`
	OpenAlerts     []alerts.Event   `json:"openAlerts"`
	OpenAlertCount int              `json:"openAlertCount"`
}

func mean(values []float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	m := sum / float64(len(values))
	return &m
}

// fleet follows the preview: down/up are the unweighted mean of each node's percentage of its expected speed
// (nodes without data are left out), success is non-failed over all tests, ips counts address changes and
// ping is the median of every successful test's latency.
func (s *Store) fleet(ctx context.Context, summaries []stats.SummaryComparison, current bool, from, to time.Time, engine string) (Fleet, error) {
	var downs, ups []float64
	var ok, count int
	for _, item := range summaries {
		sum := item.Previous
		if current {
			sum = item.Current
		}
		if sum.DownPct != nil {
			downs = append(downs, *sum.DownPct)
		}
		if sum.UpPct != nil {
			ups = append(ups, *sum.UpPct)
		}
		ok += sum.OK
		count += sum.Count
	}
	f := Fleet{Down: mean(downs), Up: mean(ups), Count: count}
	if count > 0 {
		success := float64(ok) / float64(count) * 100
		f.Success = &success
	}
	err := s.DB.QueryRow(ctx, `SELECT
		(SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY ping_ms) FROM results r JOIN nodes n ON n.id=r.node_id WHERE r.engine=COALESCE(NULLIF($3,''),n.primary_engine) AND ts >= $1 AND ts < $2 AND status = 'ok' AND ping_ms IS NOT NULL),
		(SELECT count(*) FROM node_ip_history WHERE ts >= $1 AND ts < $2 AND from_ip IS NOT NULL)::int`, from, to, engine).Scan(&f.Ping, &f.IPs)
	return f, err
}

func (s *Store) allNodes(ctx context.Context) ([]map[string]any, error) {
	var all []map[string]any
	for page := 1; ; page++ {
		items, total, err := s.Nodes.List(ctx, page, nodePage)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		if len(all) >= total || len(items) == 0 {
			return all, nil
		}
	}
}

// Snapshot builds the dashboard for the window [q.From, q.To) and the equally long period before it.
func (s *Store) Snapshot(ctx context.Context, q stats.Query) (Snapshot, error) {
	out := Snapshot{Nodes: []NodeCard{}, LatestResults: []map[string]any{}, OpenAlerts: []alerts.Event{}}
	nodes, err := s.allNodes(ctx)
	if err != nil {
		return out, err
	}
	summaries, err := s.Stats.Summary(ctx, stats.Query{Engine: q.Engine, From: q.From, To: q.To, PreviousFrom: q.PreviousFrom, PreviousTo: q.PreviousTo})
	if err != nil {
		return out, err
	}
	if out.Current, err = s.fleet(ctx, summaries, true, q.From, q.To, q.Engine); err != nil {
		return out, err
	}
	if out.Previous, err = s.fleet(ctx, summaries, false, q.PreviousFrom, q.PreviousTo, q.Engine); err != nil {
		return out, err
	}

	now := s.now()
	spark, err := s.Stats.Series(ctx, stats.Query{Engine: q.Engine, From: now.Add(-SparkHours * time.Hour), To: now.Add(time.Second), Metric: stats.Download}, false, SparkPoints)
	if err != nil {
		return out, err
	}
	sparkByNode := map[string][]stats.Point{}
	for _, series := range spark {
		if series.NodeID != nil {
			sparkByNode[*series.NodeID] = series.Points
		}
	}
	summaryByNode := map[string]stats.NodeSummary{}
	for _, item := range summaries {
		summaryByNode[item.NodeID] = item.Current
	}
	for _, node := range nodes {
		id, _ := node["id"].(string)
		points := sparkByNode[id]
		if points == nil {
			points = []stats.Point{}
		}
		out.Nodes = append(out.Nodes, NodeCard{Node: node, Summary: summaryByNode[id], Sparkline: points})
	}

	if out.LatestResults, _, err = s.Results.ListResults(ctx, ownerapi.ResultFilter{Engine: q.Engine, Sort: "ts", Dir: "desc"}, 1, RecentResults); err != nil {
		return out, err
	}
	events, total, err := s.Alerts.ListEvents(ctx, alerts.EventFilter{State: "open"}, 1, OpenAlerts)
	if err != nil {
		return out, err
	}
	if events != nil {
		out.OpenAlerts = events
	}
	out.OpenAlertCount = total
	return out, nil
}

// Series returns the bounded comparison series of the selected nodes.
func (s *Store) Series(ctx context.Context, q stats.Query, percent bool, maxPoints int) ([]stats.Series, error) {
	return s.Stats.Series(ctx, q, percent, maxPoints)
}
