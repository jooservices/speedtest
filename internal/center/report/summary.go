package report

import (
	"context"
	"fmt"
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
WHERE r.ts >= $1 AND r.ts < $2 AND r.engine = n.primary_engine
GROUP BY n.id, n.name, n.primary_engine
ORDER BY lower(n.name), n.id`

// Summarize returns one row per node with results from that node's primary engine in period.
func (s *Store) Summarize(ctx context.Context, period Period) (PeriodSummary, error) {
	if !period.to.After(period.from) {
		return PeriodSummary{}, fmt.Errorf("%w: period must be created with NewPeriod", ErrInvalidPeriod)
	}
	out := PeriodSummary{
		Kind:      period.kind,
		LocalDate: period.localDate,
		Timezone:  period.timezone,
		From:      period.from,
		To:        period.to,
		Nodes:     []NodeSummary{},
	}
	err := s.collect(ctx, primaryEngineSummarySQL, []any{period.from, period.to}, func(rows pgx.Rows) error {
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
