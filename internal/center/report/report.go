// Package report builds the monthly ISP and plan report: per ISP and node, delivered against advertised speed,
// p95, worst hours, failures, downtime and IP changes, exported as JSON, CSV or PDF.
package report

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrInvalid reports a month, engine or ISP the caller may correct.
var ErrInvalid = errors.New("invalid report request")

// Pool is the part of a pgx pool the report needs.
type Pool interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// Row is one ISP on one node for the month.
type Row struct {
	ISPID          string   `json:"ispId"`
	ISP            string   `json:"isp"`
	ASN            int64    `json:"asn"`
	NodeID         string   `json:"nodeId"`
	Node           string   `json:"node"`
	Plan           *string  `json:"plan"`
	AdvertisedDown *float64 `json:"advertisedDownMbps"`
	AdvertisedUp   *float64 `json:"advertisedUpMbps"`
	Tests          int      `json:"tests"`
	Failures       int      `json:"failures"`
	AvgDown        *float64 `json:"avgDownMbps"`
	P95Down        *float64 `json:"p95DownMbps"`
	AvgUp          *float64 `json:"avgUpMbps"`
	P95Up          *float64 `json:"p95UpMbps"`
	DeliveredPct   *float64 `json:"deliveredPercent"`
	WorstHours     []int    `json:"worstHoursUtc"`
	DowntimeMin    int      `json:"downtimeMinutes"`
	IPChanges      int      `json:"ipChanges"`
}

// Report is the whole month.
type Report struct {
	Month     string    `json:"month"`
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
	Partial   bool      `json:"partial"`
	Engine    string    `json:"engine"`
	Generated time.Time `json:"generatedAt"`
	Rows      []Row     `json:"rows"`
}

// Store reads the report from PostgreSQL.
type Store struct {
	DB  Pool
	Now func() time.Time
}

var monthPattern = regexp.MustCompile(`^\d{4}-(0[1-9]|1[0-2])$`)

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Window parses YYYY-MM into the UTC month [from, to). A month in the future is invalid.
func Window(month string, now time.Time) (time.Time, time.Time, error) {
	if !monthPattern.MatchString(month) {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: month must be YYYY-MM", ErrInvalid)
	}
	from, err := time.Parse("2006-01", month)
	if err != nil || from.Year() < 2000 {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: month must be YYYY-MM", ErrInvalid)
	}
	if from.After(now.UTC()) {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: month is in the future", ErrInvalid)
	}
	return from, from.AddDate(0, 1, 0), nil
}

// PreviousMonth returns the month before now as YYYY-MM.
func PreviousMonth(now time.Time) string {
	n := now.UTC()
	return time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 0).Format("2006-01")
}

// engineClause scopes results to an explicit engine or each node's primary engine; engines are never mixed.
func engineClause(engine string) (string, []any) {
	if engine == "" {
		return "r.engine = n.primary_engine", nil
	}
	return "r.engine = $3", []any{engine}
}

const rowsSQL = `SELECT i.id::text, i.name, i.asn, n.id::text, n.name, p.name, p.advertised_down_mbps, p.advertised_up_mbps,
	COUNT(*)::int, COUNT(*) FILTER (WHERE r.status = 'failed')::int,
	AVG(r.down_mbps) FILTER (WHERE r.status <> 'failed'),
	percentile_cont(0.95) WITHIN GROUP (ORDER BY r.down_mbps) FILTER (WHERE r.status <> 'failed'),
	AVG(r.up_mbps) FILTER (WHERE r.status <> 'failed'),
	percentile_cont(0.95) WITHIN GROUP (ORDER BY r.up_mbps) FILTER (WHERE r.status <> 'failed')
FROM results r JOIN nodes n ON n.id = r.node_id AND %s
JOIN isps i ON i.id = r.isp_id
LEFT JOIN isp_plans p ON p.id = n.isp_plan_id
WHERE r.ts >= $1 AND r.ts < $2 %s
GROUP BY i.id, n.id, p.id
ORDER BY lower(i.name), i.asn, lower(n.name)`

const worstSQL = `SELECT node_id::text, hr FROM (
	SELECT r.node_id, extract(hour FROM r.ts AT TIME ZONE 'UTC')::int AS hr,
		row_number() OVER (PARTITION BY r.node_id ORDER BY AVG(r.down_mbps), extract(hour FROM r.ts AT TIME ZONE 'UTC')) AS rn
	FROM results r JOIN nodes n ON n.id = r.node_id AND %s
	WHERE r.ts >= $1 AND r.ts < $2 AND r.status <> 'failed' AND r.down_mbps IS NOT NULL
	GROUP BY r.node_id, extract(hour FROM r.ts AT TIME ZONE 'UTC')
) w WHERE rn <= 3 ORDER BY node_id, rn`

// Downtime counts the minutes from each failed test to the next test of the node (or the end of the window), at most a
// day per failure, so a node that stopped reporting is not counted for the whole month.
const downtimeSQL = `SELECT node_id::text, COALESCE(SUM(LEAST(extract(epoch FROM (COALESCE(nxt, $2::timestamptz) - ts)) / 60, 1440)), 0)::int
FROM (
	SELECT r.node_id, r.ts, r.status, LEAD(r.ts) OVER (PARTITION BY r.node_id ORDER BY r.ts) AS nxt
	FROM results r JOIN nodes n ON n.id = r.node_id AND %s
	WHERE r.ts >= $1 AND r.ts < $2
) t WHERE status = 'failed' GROUP BY node_id`

const changesSQL = `SELECT node_id::text, COUNT(*)::int FROM node_ip_history WHERE ts >= $1 AND ts < $2 GROUP BY node_id`

// Build assembles the report for month (YYYY-MM). ispID limits it to one ISP when not empty.
func (s *Store) Build(ctx context.Context, month, engine, ispID string) (Report, error) {
	from, to, err := Window(month, s.now())
	if err != nil {
		return Report{}, err
	}
	clause, extra := engineClause(engine)
	args := append([]any{from, to}, extra...)
	rowArgs, filter := args, ""
	if ispID != "" {
		rowArgs = append(append([]any{}, args...), ispID)
		filter = fmt.Sprintf("AND i.id = $%d::uuid", len(rowArgs))
	}
	out := Report{Month: month, From: from, To: to, Partial: to.After(s.now()), Engine: engine, Generated: s.now(), Rows: []Row{}}
	if err := s.scanRows(ctx, &out, fmt.Sprintf(rowsSQL, clause, filter), rowArgs); err != nil {
		return Report{}, err
	}
	if len(out.Rows) == 0 {
		return out, nil
	}
	worst := map[string][]int{}
	if err := s.collect(ctx, fmt.Sprintf(worstSQL, clause), args, func(rows pgx.Rows) error {
		var node string
		var hour int
		if err := rows.Scan(&node, &hour); err != nil {
			return err
		}
		worst[node] = append(worst[node], hour)
		return nil
	}); err != nil {
		return Report{}, err
	}
	down, changes := map[string]int{}, map[string]int{}
	if err := s.collect(ctx, fmt.Sprintf(downtimeSQL, clause), args, scanCount(down)); err != nil {
		return Report{}, err
	}
	if err := s.collect(ctx, changesSQL, []any{from, to}, scanCount(changes)); err != nil {
		return Report{}, err
	}
	for i := range out.Rows {
		row := &out.Rows[i]
		row.WorstHours, row.DowntimeMin, row.IPChanges = worst[row.NodeID], down[row.NodeID], changes[row.NodeID]
		if row.WorstHours == nil {
			row.WorstHours = []int{}
		}
	}
	return out, nil
}

func scanCount(into map[string]int) func(pgx.Rows) error {
	return func(rows pgx.Rows) error {
		var node string
		var n int
		if err := rows.Scan(&node, &n); err != nil {
			return err
		}
		into[node] = n
		return nil
	}
}

func (s *Store) collect(ctx context.Context, sql string, args []any, each func(pgx.Rows) error) error {
	rows, err := s.DB.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := each(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *Store) scanRows(ctx context.Context, out *Report, sql string, args []any) error {
	return s.collect(ctx, sql, args, func(rows pgx.Rows) error {
		var r Row
		if err := rows.Scan(&r.ISPID, &r.ISP, &r.ASN, &r.NodeID, &r.Node, &r.Plan, &r.AdvertisedDown, &r.AdvertisedUp,
			&r.Tests, &r.Failures, &r.AvgDown, &r.P95Down, &r.AvgUp, &r.P95Up); err != nil {
			return err
		}
		if r.AdvertisedDown != nil && r.AvgDown != nil {
			pct := *r.AvgDown / *r.AdvertisedDown * 100
			r.DeliveredPct = &pct
		}
		out.Rows = append(out.Rows, r)
		return nil
	})
}
