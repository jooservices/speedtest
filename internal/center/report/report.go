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
	ISPID          *string  `json:"ispId"`
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

const attributedSQL = `WITH attributed AS (
	SELECT r.*, COALESCE(r.isp_id, history.isp_id, prior.isp_id) AS report_isp_id
	FROM results r JOIN nodes n ON n.id = r.node_id
	LEFT JOIN LATERAL (
		SELECT h.to_isp_id AS isp_id FROM node_ip_history h
		WHERE h.node_id = r.node_id AND h.ts <= r.ts AND h.to_isp_id IS NOT NULL
		ORDER BY h.ts DESC, h.id DESC LIMIT 1
	) history ON true
	LEFT JOIN LATERAL (
		SELECT previous.isp_id FROM results previous
		WHERE previous.node_id = r.node_id AND previous.ts < r.ts AND previous.isp_id IS NOT NULL
		ORDER BY previous.ts DESC, previous.id DESC LIMIT 1
	) prior ON true
	WHERE r.ts >= $1 AND r.ts < $2 AND %s
)`

const rowsSQL = `SELECT i.id::text, COALESCE(i.name, 'Unknown ISP'), COALESCE(i.asn, 0), n.id::text, n.name, p.name, p.advertised_down_mbps, p.advertised_up_mbps,
	COUNT(*)::int, COUNT(*) FILTER (WHERE r.status = 'failed')::int,
	AVG(r.down_mbps) FILTER (WHERE r.status <> 'failed'),
	percentile_cont(0.95) WITHIN GROUP (ORDER BY r.down_mbps) FILTER (WHERE r.status <> 'failed'),
	AVG(r.up_mbps) FILTER (WHERE r.status <> 'failed'),
	percentile_cont(0.95) WITHIN GROUP (ORDER BY r.up_mbps) FILTER (WHERE r.status <> 'failed')
FROM attributed r JOIN nodes n ON n.id = r.node_id
LEFT JOIN isps i ON i.id = r.report_isp_id
LEFT JOIN isp_plans p ON p.id = n.isp_plan_id AND p.isp_id = r.report_isp_id
WHERE true %s
GROUP BY i.id, n.id, p.id
ORDER BY lower(COALESCE(i.name, 'Unknown ISP')), COALESCE(i.asn, 0), lower(n.name)`

const worstSQL = `SELECT COALESCE(report_isp_id::text, ''), node_id::text, hr FROM (
	SELECT report_isp_id, node_id, extract(hour FROM ts AT TIME ZONE 'UTC')::int AS hr,
		row_number() OVER (PARTITION BY report_isp_id, node_id ORDER BY AVG(down_mbps), extract(hour FROM ts AT TIME ZONE 'UTC')) AS rn
	FROM attributed WHERE status <> 'failed' AND down_mbps IS NOT NULL
	GROUP BY report_isp_id, node_id, extract(hour FROM ts AT TIME ZONE 'UTC')
) w WHERE rn <= 3 ORDER BY report_isp_id, node_id, rn`

// Downtime counts from a failed test to the next test of the node or the report boundary, at most a day per failure.
const downtimeSQL = `SELECT COALESCE(report_isp_id::text, ''), node_id::text, COALESCE(SUM(LEAST(extract(epoch FROM (COALESCE(nxt, LEAST($2::timestamptz, $%d::timestamptz)) - ts)) / 60, 1440)), 0)::int
FROM (
	SELECT report_isp_id, node_id, ts, status, LEAD(ts) OVER (PARTITION BY node_id ORDER BY ts, id) AS nxt
	FROM attributed
	) t WHERE status = 'failed' GROUP BY report_isp_id, node_id`

const changesSQL = `SELECT COALESCE(COALESCE(to_isp_id, from_isp_id)::text, ''), node_id::text, COUNT(*)::int FROM node_ip_history
WHERE ts >= $1 AND ts < $2 AND from_ip IS NOT NULL %s GROUP BY COALESCE(to_isp_id, from_isp_id), node_id`

type metricKey struct{ isp, node string }

// Build assembles the report for month (YYYY-MM). ispID limits it to one ISP when not empty.
func (s *Store) Build(ctx context.Context, month, engine, ispID string) (Report, error) {
	now := s.now()
	from, to, err := Window(month, now)
	if err != nil {
		return Report{}, err
	}
	clause, extra := engineClause(engine)
	args := append([]any{from, to}, extra...)
	rowArgs, filter := args, ""
	if ispID != "" {
		rowArgs = append(append([]any{}, args...), ispID)
		filter = fmt.Sprintf("AND r.report_isp_id = $%d::uuid", len(rowArgs))
	}
	out := Report{Month: month, From: from, To: to, Partial: to.After(now), Engine: engine, Generated: now, Rows: []Row{}}
	if err := s.scanRows(ctx, &out, fmt.Sprintf(attributedSQL, clause)+fmt.Sprintf(rowsSQL, filter), rowArgs); err != nil {
		return Report{}, err
	}
	if len(out.Rows) == 0 {
		return out, nil
	}
	worst := map[metricKey][]int{}
	if err := s.collect(ctx, fmt.Sprintf(attributedSQL, clause)+worstSQL, args, func(rows pgx.Rows) error {
		var key metricKey
		var hour int
		if err := rows.Scan(&key.isp, &key.node, &hour); err != nil {
			return err
		}
		worst[key] = append(worst[key], hour)
		return nil
	}); err != nil {
		return Report{}, err
	}
	down, changes := map[metricKey]int{}, map[metricKey]int{}
	downArgs := append(append([]any{}, args...), now)
	if err := s.collect(ctx, fmt.Sprintf(attributedSQL, clause)+fmt.Sprintf(downtimeSQL, len(downArgs)), downArgs, scanCount(down)); err != nil {
		return Report{}, err
	}
	changeFilter, changeArgs := "", []any{from, to}
	if ispID != "" {
		changeArgs = append(changeArgs, ispID)
		changeFilter = fmt.Sprintf("AND COALESCE(to_isp_id, from_isp_id) = $%d::uuid", len(changeArgs))
	}
	if err := s.collect(ctx, fmt.Sprintf(changesSQL, changeFilter), changeArgs, scanCount(changes)); err != nil {
		return Report{}, err
	}
	for i := range out.Rows {
		row := &out.Rows[i]
		key := metricKey{isp: stringValue(row.ISPID), node: row.NodeID}
		row.WorstHours, row.DowntimeMin, row.IPChanges = worst[key], down[key], changes[key]
		if row.WorstHours == nil {
			row.WorstHours = []int{}
		}
	}
	return out, nil
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func scanCount(into map[metricKey]int) func(pgx.Rows) error {
	return func(rows pgx.Rows) error {
		var key metricKey
		var n int
		if err := rows.Scan(&key.isp, &key.node, &n); err != nil {
			return err
		}
		into[key] = n
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
