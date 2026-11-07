package stats

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// DefaultMaxPoints and MaxPointsLimit bound the number of buckets of a comparison series.
const (
	DefaultMaxPoints = 300
	MaxPointsLimit   = 300
)

// seriesExpression is the SQL value a series averages. In percent mode each sample is converted with the
// expected speed of its own node before it is aggregated, as the contract requires.
func seriesExpression(m Metric, percent bool) (string, error) {
	column, err := m.column()
	if err != nil {
		return "", err
	}
	if !percent {
		return "r." + column, nil
	}
	switch m {
	case Download:
		return "(r.down_mbps / NULLIF(n.expected_down_mbps, 0) * 100)", nil
	case Upload:
		return "(r.up_mbps / NULLIF(n.expected_up_mbps, 0) * 100)", nil
	}
	return "", errors.New("percent mode is available only for down and up")
}

// BucketWidth returns the width in seconds of equally spaced buckets that cover the window with at most maxPoints
// buckets, and how many buckets that is.
func BucketWidth(from, to time.Time, maxPoints int) (seconds float64, buckets int) {
	if maxPoints < 1 {
		maxPoints = DefaultMaxPoints
	}
	span := to.Sub(from).Seconds()
	if span <= 0 {
		return 1, 0
	}
	seconds = math.Max(1, math.Ceil(span/float64(maxPoints)))
	return seconds, int(math.Ceil(span / seconds))
}

// Series returns one series per selected node with equally spaced buckets: the mean of the non-failed samples
// of each bucket and how many there were. Buckets without samples are null gaps. Each summary is computed
// from the original samples of the window, not from the bucket means.
func (s *Store) Series(ctx context.Context, q Query, percent bool, maxPoints int) ([]Series, error) {
	if maxPoints < 1 || maxPoints > MaxPointsLimit {
		return nil, fmt.Errorf("maxPoints must be between 1 and %d", MaxPointsLimit)
	}
	expr, err := seriesExpression(q.Metric, percent)
	if err != nil {
		return nil, err
	}
	if q.NodesSet && len(q.NodeIDs) == 0 {
		return []Series{}, nil
	}
	width, buckets := BucketWidth(q.From, q.To, maxPoints)
	if buckets == 0 {
		return []Series{}, nil
	}
	query := seriesQuery(expr)
	rows, err := s.query(ctx, q, query, q.nodeIDs(), q.From, q.To, buckets, width)
	if err != nil {
		return nil, fmt.Errorf("query series: %w", err)
	}
	defer rows.Close()
	unit := q.Metric.unit()
	if percent {
		unit = "%"
	}
	items := make([]Series, 0)
	byID := map[string]int{}
	for rows.Next() {
		var id, name string
		var ts time.Time
		var count int
		var value *float64
		if err := rows.Scan(&id, &name, &ts, &count, &value); err != nil {
			return nil, fmt.Errorf("scan series: %w", err)
		}
		idx, found := byID[id]
		if !found {
			copyID := id
			idx = len(items)
			byID[id] = idx
			items = append(items, Series{NodeID: &copyID, Name: name, Metric: q.Metric, Unit: unit, Points: make([]Point, 0, buckets)})
		}
		items[idx].Points = append(items[idx].Points, Point{TS: ts.UTC(), Value: value, Count: count})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read series: %w", err)
	}
	rows.Close()
	summaries, err := s.metricSummaries(ctx, q, expr, "r.status <> 'failed'")
	if err != nil {
		return nil, err
	}
	for i := range items {
		items[i].Summary = summaries[*items[i].NodeID]
	}
	return items, nil
}

func seriesQuery(expr string) string {
	return `WITH selected_nodes AS (` + selectedNodes + `), grid AS (SELECT g AS k FROM generate_series(0, $4::int - 1) g),
grouped AS (
 SELECT r.node_id, floor(extract(epoch FROM (r.ts - $2::timestamptz)) / $5::float8)::int AS k, count(*)::int AS c, avg(` + expr + `)::float8 AS v
 FROM results r JOIN selected_nodes n ON r.node_id = n.id
 WHERE r.ts >= $2 AND r.ts < $3 AND r.status <> 'failed' AND ` + expr + ` IS NOT NULL
 GROUP BY r.node_id, k
)
SELECT n.id::text, n.name, $2::timestamptz + (grid.k * $5::float8) * interval '1 second', coalesce(g.c, 0), g.v
FROM selected_nodes n CROSS JOIN grid
LEFT JOIN grouped g ON g.node_id = n.id AND g.k = grid.k
ORDER BY n.name, n.id, grid.k`
}
