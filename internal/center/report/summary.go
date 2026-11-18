package report

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// PeriodSummary contains the owner-local bounds and per-node primary-engine metrics for a period.
type PeriodSummary struct {
	Kind      PeriodKind    `json:"kind"`
	LocalDate string        `json:"localDate"`
	Timezone  string        `json:"timezone"`
	From      time.Time     `json:"from"`
	To        time.Time     `json:"to"`
	Nodes     []NodeSummary `json:"nodes"`
}

// NodeSummary contains measurements from one node's primary engine only.
type NodeSummary struct {
	NodeID    string   `json:"nodeId"`
	Node      string   `json:"node"`
	Engine    string   `json:"engine"`
	Successes int      `json:"successes"`
	Failures  int      `json:"failures"`
	AvgDown   *float64 `json:"avgDownMbps"`
	AvgUp     *float64 `json:"avgUpMbps"`
}

const primaryEngineSummarySQL = `SELECT n.id::text, n.name, n.primary_engine,
	COUNT(*) FILTER (WHERE r.status <> 'failed')::int,
	COUNT(*) FILTER (WHERE r.status = 'failed')::int,
	AVG(r.down_mbps) FILTER (WHERE r.status <> 'failed'),
	AVG(r.up_mbps) FILTER (WHERE r.status <> 'failed')
FROM results r
JOIN nodes n ON n.id = r.node_id
WHERE (%s) AND r.engine = n.primary_engine
GROUP BY n.id, n.name, n.primary_engine
ORDER BY lower(n.name), n.id`

func summaryQuery(period Period) (string, []any) {
	predicates := make([]string, 0, len(period.intervals))
	args := make([]any, 0, len(period.intervals)*2)
	for index, interval := range period.intervals {
		start := index*2 + 1
		end := start + 1
		predicates = append(predicates, fmt.Sprintf("(r.ts >= $%d AND r.ts < $%d)", start, end))
		args = append(args, interval.from, interval.to)
	}
	return fmt.Sprintf(primaryEngineSummarySQL, strings.Join(predicates, " OR ")), args
}

// Summarize returns one row per node with results from that node's primary engine in period.
func (s *Store) Summarize(ctx context.Context, period Period) (PeriodSummary, error) {
	if !period.to.After(period.from) {
		return PeriodSummary{}, fmt.Errorf("%w: period must be created with NewPeriod", ErrInvalidPeriod)
	}
	if period.from.Year() < 0 || period.from.Year() > 9999 || period.to.Year() < 0 || period.to.Year() > 9999 {
		return PeriodSummary{}, fmt.Errorf("%w: UTC bounds cannot be encoded as JSON", ErrInvalidPeriod)
	}
	out := PeriodSummary{
		Kind:      period.kind,
		LocalDate: period.localDate,
		Timezone:  period.timezone,
		From:      period.from,
		To:        period.to,
		Nodes:     []NodeSummary{},
	}
	query, args := summaryQuery(period)
	err := s.collect(ctx, query, args, func(rows pgx.Rows) error {
		var node NodeSummary
		if err := rows.Scan(&node.NodeID, &node.Node, &node.Engine, &node.Successes, &node.Failures, &node.AvgDown, &node.AvgUp); err != nil {
			return err
		}
		out.Nodes = append(out.Nodes, node)
		return nil
	})
	if err != nil {
		return PeriodSummary{}, err
	}
	return out, nil
}
