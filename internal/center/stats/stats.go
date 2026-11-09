// Package stats implements the PostgreSQL queries behind the owner statistics API.
package stats

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrNotFound = errors.New("statistics node not found")

type Metric string

const (
	Download Metric = "down"
	Upload   Metric = "up"
	Ping     Metric = "ping"
	Jitter   Metric = "jitter"
	Loss     Metric = "loss"
)

func (m Metric) column() (string, error) {
	switch m {
	case Download:
		return "down_mbps", nil
	case Upload:
		return "up_mbps", nil
	case Ping:
		return "ping_ms", nil
	case Jitter:
		return "jitter_ms", nil
	case Loss:
		return "loss_percent", nil
	default:
		return "", errors.New("unsupported statistics metric")
	}
}

func (m Metric) unit() string {
	switch m {
	case Download, Upload:
		return "Mbps"
	case Ping, Jitter:
		return "ms"
	default:
		return "%"
	}
}

type Query struct {
	Engine       string
	ISP          string
	NodeIDs      []string
	NodesSet     bool
	From         time.Time
	To           time.Time
	PreviousFrom time.Time
	PreviousTo   time.Time
	Metric       Metric
	Timezone     string
	Bucket       string
	Bins         int
	Combined     bool
}

type MetricSummary struct {
	N           int      `json:"n"`
	Mean        *float64 `json:"mean"`
	Median      *float64 `json:"median"`
	Min         *float64 `json:"min"`
	Max         *float64 `json:"max"`
	P5          *float64 `json:"p5"`
	P50         *float64 `json:"p50"`
	P95         *float64 `json:"p95"`
	P99         *float64 `json:"p99"`
	Std         *float64 `json:"std"`
	percentiles []float64
}

type NodeSummary struct {
	Count       int           `json:"count"`
	OK          int           `json:"ok"`
	Failed      int           `json:"failed"`
	SuccessRate *float64      `json:"successRate"`
	Down        MetricSummary `json:"down"`
	Up          MetricSummary `json:"up"`
	Ping        MetricSummary `json:"ping"`
	Jitter      MetricSummary `json:"jitter"`
	Loss        MetricSummary `json:"loss"`
	IPCount     int           `json:"ipCount"`
	DownPct     *float64      `json:"downPct"`
	UpPct       *float64      `json:"upPct"`
	Stability   *float64      `json:"stability"`
}

type SummaryComparison struct {
	NodeID   string      `json:"nodeId"`
	Current  NodeSummary `json:"current"`
	Previous NodeSummary `json:"previous"`
}

type Point struct {
	TS    time.Time `json:"ts"`
	Value *float64  `json:"value"`
	Count int       `json:"count"`
}

type Series struct {
	NodeID  *string       `json:"nodeId"`
	Name    string        `json:"name"`
	Metric  Metric        `json:"metric"`
	Unit    string        `json:"unit"`
	Points  []Point       `json:"points"`
	Summary MetricSummary `json:"summary"`
}

type HourValue struct {
	Hour  int      `json:"hour"`
	Value *float64 `json:"value"`
	Count int      `json:"count"`
}

type HourSeries struct {
	NodeID string      `json:"nodeId"`
	Hours  []HourValue `json:"hours"`
}

type HeatCell struct {
	Weekday int      `json:"weekday"`
	Hour    int      `json:"hour"`
	Value   *float64 `json:"value"`
	Count   int      `json:"count"`
}

type Heatmap struct {
	NodeID   string     `json:"nodeId"`
	Metric   Metric     `json:"metric"`
	Timezone string     `json:"timezone"`
	Cells    []HeatCell `json:"cells"`
}

type Bin struct {
	From    float64 `json:"from"`
	To      float64 `json:"to"`
	Count   int     `json:"count"`
	Percent float64 `json:"percent"`
}

type Distribution struct {
	NodeID string `json:"nodeId"`
	Metric Metric `json:"metric"`
	Bins   []Bin  `json:"bins"`
	Count  int    `json:"count"`
}

type DB interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type Store struct{ DB DB }

const selectedNodes = `
SELECT id, name, expected_down_mbps, expected_up_mbps, primary_engine
FROM nodes
WHERE ($1::uuid[] IS NULL OR id = ANY($1::uuid[]))`

func (q Query) nodeIDs() []string {
	if !q.NodesSet {
		return nil
	}
	return q.NodeIDs
}

func (s *Store) Summary(ctx context.Context, q Query) ([]SummaryComparison, error) {
	if q.NodesSet && len(q.NodeIDs) == 0 {
		return []SummaryComparison{}, nil
	}
	query := summarySQL()
	rows, err := s.query(ctx, q, query, q.nodeIDs(), q.From, q.To, q.PreviousFrom, q.PreviousTo)
	if err != nil {
		return nil, fmt.Errorf("query statistics summary: %w", err)
	}
	defer rows.Close()

	items := make([]SummaryComparison, 0)
	byID := make(map[string]int)
	for rows.Next() {
		var id, name, period string
		var summary NodeSummary
		dest := []any{&id, &name, &period, &summary.Count, &summary.OK, &summary.Failed, &summary.IPCount}
		for _, metric := range []*MetricSummary{&summary.Down, &summary.Up, &summary.Ping, &summary.Jitter, &summary.Loss} {
			dest = append(dest, metricDest(metric)...)
		}
		var expectedDown, expectedUp float64
		dest = append(dest, &expectedDown, &expectedUp)
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("scan statistics summary: %w", err)
		}
		for _, metric := range []*MetricSummary{&summary.Down, &summary.Up, &summary.Ping, &summary.Jitter, &summary.Loss} {
			metric.setPercentiles()
		}
		if summary.Count > 0 {
			summary.SuccessRate = number(float64(summary.OK) / float64(summary.Count) * 100)
		}
		if summary.Down.Mean != nil {
			summary.DownPct = number(*summary.Down.Mean / expectedDown * 100)
		}
		if summary.Up.Mean != nil {
			summary.UpPct = number(*summary.Up.Mean / expectedUp * 100)
		}
		if summary.Down.Mean != nil && *summary.Down.Mean != 0 && summary.Down.Std != nil {
			stability := math.Max(0, math.Min(100, 100-(*summary.Down.Std / *summary.Down.Mean)*220))
			summary.Stability = number(stability)
		}

		idx, found := byID[id]
		if !found {
			idx = len(items)
			byID[id] = idx
			items = append(items, SummaryComparison{NodeID: id})
		}
		if period == "current" {
			items[idx].Current = summary
		} else {
			items[idx].Previous = summary
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read statistics summary: %w", err)
	}
	return items, nil
}

func summarySQL() string {
	selects := []string{"n.id::text", "n.name", "p.period", "count(r.id)::int", "count(r.id) FILTER (WHERE r.status <> 'failed')::int",
		"count(r.id) FILTER (WHERE r.status = 'failed')::int", "count(DISTINCT r.ip)::int"}
	for _, col := range []string{"down_mbps", "up_mbps", "ping_ms", "jitter_ms", "loss_percent"} {
		selects = append(selects, metricAggregate("r."+col)...)
	}
	selects = append(selects, "n.expected_down_mbps", "n.expected_up_mbps")
	return `WITH selected_nodes AS (` + selectedNodes + `), periods(period, start_at, end_at) AS (
 SELECT 'current', $2::timestamptz, $3::timestamptz
 UNION ALL SELECT 'previous', $4::timestamptz, $5::timestamptz
)
SELECT ` + joinSQL(selects) + `
FROM selected_nodes n CROSS JOIN periods p
LEFT JOIN results r ON r.node_id = n.id AND r.ts >= p.start_at AND r.ts < p.end_at
GROUP BY n.id, n.name, n.expected_down_mbps, n.expected_up_mbps, p.period
ORDER BY n.name, n.id, p.period`
}

func metricAggregate(column string) []string {
	valid := "r.status <> 'failed' AND " + column + " IS NOT NULL"
	return []string{
		"count(" + column + ") FILTER (WHERE r.status <> 'failed')",
		"avg(" + column + ") FILTER (WHERE r.status <> 'failed')",
		"min(" + column + ") FILTER (WHERE r.status <> 'failed')",
		"max(" + column + ") FILTER (WHERE r.status <> 'failed')",
		"stddev_pop(" + column + ") FILTER (WHERE r.status <> 'failed')",
		"percentile_cont(ARRAY[0.05, 0.5, 0.95, 0.99]) WITHIN GROUP (ORDER BY " + column + ") FILTER (WHERE " + valid + ")",
	}
}

func metricDest(m *MetricSummary) []any {
	return []any{&m.N, &m.Mean, &m.Min, &m.Max, &m.Std, &m.percentiles}
}

func (m *MetricSummary) setPercentiles() {
	if len(m.percentiles) == 4 {
		m.P5 = number(m.percentiles[0])
		m.P50 = number(m.percentiles[1])
		m.Median = m.P50
		m.P95 = number(m.percentiles[2])
		m.P99 = number(m.percentiles[3])
	}
}

func joinSQL(parts []string) string {
	out := ""
	for i, part := range parts {
		if i > 0 {
			out += ", "
		}
		out += part
	}
	return out
}

func number(v float64) *float64 { return &v }

func (s *Store) Trend(ctx context.Context, q Query) ([]Series, error) {
	column, err := q.Metric.column()
	if err != nil {
		return nil, err
	}
	if q.Bucket != "hour" && q.Bucket != "day" {
		return nil, errors.New("unsupported statistics bucket")
	}
	if q.Combined && q.Metric != Download && q.Metric != Upload {
		return nil, errors.New("combined totals require down or up metrics")
	}
	if q.NodesSet && len(q.NodeIDs) == 0 {
		return []Series{}, nil
	}

	query := trendSQL(column)
	rows, err := s.query(ctx, q, query, q.nodeIDs(), q.From, q.To, q.Bucket, q.Timezone)
	if err != nil {
		return nil, fmt.Errorf("query statistics trend: %w", err)
	}
	defer rows.Close()
	items := make([]Series, 0)
	byID := make(map[string]int)
	for rows.Next() {
		var id, name string
		var bucket time.Time
		var value *float64
		var count int
		if err := rows.Scan(&id, &name, &bucket, &count, &value); err != nil {
			return nil, fmt.Errorf("scan statistics trend: %w", err)
		}
		idx, found := byID[id]
		if !found {
			copyID := id
			idx = len(items)
			byID[id] = idx
			items = append(items, Series{NodeID: &copyID, Name: name, Metric: q.Metric, Unit: q.Metric.unit(), Points: make([]Point, 0)})
		}
		items[idx].Points = append(items[idx].Points, Point{TS: bucket.UTC(), Value: value, Count: count})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read statistics trend: %w", err)
	}

	summaries, err := s.metricSummaries(ctx, q, column, "r.status = 'ok'")
	if err != nil {
		return nil, err
	}
	for idx := range items {
		if summary, ok := summaries[*items[idx].NodeID]; ok {
			items[idx].Summary = summary
		}
	}
	if q.Combined && len(items) > 1 {
		items = append(items, combinedSeries(items, q.Metric))
	}
	return items, nil
}

func trendSQL(column string) string {
	return `WITH selected_nodes AS (` + selectedNodes + `), calendar AS (
 SELECT local_bucket, local_bucket AT TIME ZONE $5 AS bucket_ts
 FROM generate_series(
   date_trunc($4, $2::timestamptz AT TIME ZONE $5),
   date_trunc($4, ($3::timestamptz - interval '1 microsecond') AT TIME ZONE $5),
   CASE WHEN $4 = 'hour' THEN interval '1 hour' ELSE interval '1 day' END
 ) AS generated(local_bucket)
 WHERE (local_bucket AT TIME ZONE $5) AT TIME ZONE $5 = local_bucket
), grouped AS (
 SELECT r.node_id, date_trunc($4, r.ts AT TIME ZONE $5) AS local_bucket,
        count(r.` + column + `)::int AS sample_count, avg(r.` + column + `)::float8 AS value
 FROM results r JOIN selected_nodes n ON r.node_id = n.id
 WHERE r.ts >= $2 AND r.ts < $3 AND r.status = 'ok' AND r.` + column + ` IS NOT NULL
 GROUP BY r.node_id, local_bucket
)
SELECT n.id::text, n.name, c.bucket_ts, coalesce(g.sample_count, 0), g.value
FROM selected_nodes n CROSS JOIN calendar c
LEFT JOIN grouped g ON g.node_id = n.id AND g.local_bucket = c.local_bucket
ORDER BY n.name, n.id, c.local_bucket`
}

func (s *Store) metricSummaries(ctx context.Context, q Query, column, statusCondition string) (map[string]MetricSummary, error) {
	if q.NodesSet && len(q.NodeIDs) == 0 {
		return map[string]MetricSummary{}, nil
	}
	query := `WITH selected_nodes AS (` + selectedNodes + `)
SELECT n.id::text, ` + joinSQL(metricAggregate(column)) + `
FROM selected_nodes n LEFT JOIN results r ON r.node_id = n.id AND r.ts >= $2 AND r.ts < $3 AND ` + statusCondition + `
GROUP BY n.id ORDER BY n.id`
	rows, err := s.query(ctx, q, query, q.nodeIDs(), q.From, q.To)
	if err != nil {
		return nil, fmt.Errorf("query statistics metric summary: %w", err)
	}
	defer rows.Close()
	items := make(map[string]MetricSummary)
	for rows.Next() {
		var id string
		var summary MetricSummary
		if err := rows.Scan(append([]any{&id}, metricDest(&summary)...)...); err != nil {
			return nil, fmt.Errorf("scan statistics metric summary: %w", err)
		}
		summary.setPercentiles()
		items[id] = summary
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read statistics metric summary: %w", err)
	}
	return items, nil
}

func (s *Store) HourOfDay(ctx context.Context, q Query) ([]HourSeries, error) {
	column, err := q.Metric.column()
	if err != nil {
		return nil, err
	}
	if q.NodesSet && len(q.NodeIDs) == 0 {
		return []HourSeries{}, nil
	}
	query := `WITH selected_nodes AS (` + selectedNodes + `), hours AS (
 SELECT generate_series(0, 23)::int AS hour
), grouped AS (
 SELECT r.node_id, extract(hour FROM r.ts AT TIME ZONE $4)::int AS hour,
        count(r.` + column + `)::int AS sample_count, avg(r.` + column + `)::float8 AS value
 FROM results r JOIN selected_nodes n ON r.node_id = n.id
 WHERE r.ts >= $2 AND r.ts < $3 AND r.status = 'ok' AND r.` + column + ` IS NOT NULL
 GROUP BY r.node_id, hour
)
SELECT n.id::text, h.hour, coalesce(g.sample_count, 0), g.value
FROM selected_nodes n CROSS JOIN hours h
LEFT JOIN grouped g ON g.node_id = n.id AND g.hour = h.hour
ORDER BY n.id, h.hour`
	rows, err := s.query(ctx, q, query, q.nodeIDs(), q.From, q.To, q.Timezone)
	if err != nil {
		return nil, fmt.Errorf("query statistics hour-of-day: %w", err)
	}
	defer rows.Close()
	items := make([]HourSeries, 0)
	byID := make(map[string]int)
	for rows.Next() {
		var id string
		var hour, count int
		var value *float64
		if err := rows.Scan(&id, &hour, &count, &value); err != nil {
			return nil, fmt.Errorf("scan statistics hour-of-day: %w", err)
		}
		idx, ok := byID[id]
		if !ok {
			idx = len(items)
			byID[id] = idx
			items = append(items, HourSeries{NodeID: id, Hours: make([]HourValue, 0, 24)})
		}
		items[idx].Hours = append(items[idx].Hours, HourValue{Hour: hour, Value: value, Count: count})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read statistics hour-of-day: %w", err)
	}
	return items, nil
}

func (s *Store) Heatmap(ctx context.Context, q Query, nodeID string) (Heatmap, error) {
	column, err := q.Metric.column()
	if err != nil {
		return Heatmap{}, err
	}
	query := `WITH target_node AS (
 SELECT id,primary_engine FROM nodes WHERE id = $1::uuid
), cells AS (
 SELECT weekdays.weekday, hours.hour
 FROM generate_series(0, 6) AS weekdays(weekday)
 CROSS JOIN generate_series(0, 23) AS hours(hour)
), grouped AS (
 SELECT r.node_id, (extract(isodow FROM r.ts AT TIME ZONE $4)::int - 1) AS weekday,
        extract(hour FROM r.ts AT TIME ZONE $4)::int AS hour,
        count(r.` + column + `)::int AS sample_count, avg(r.` + column + `)::float8 AS value
 FROM results r JOIN target_node n ON r.node_id = n.id
 WHERE r.ts >= $2 AND r.ts < $3 AND r.status = 'ok' AND r.` + column + ` IS NOT NULL
 GROUP BY r.node_id, weekday, hour
)
SELECT n.id::text, c.weekday, c.hour, coalesce(g.sample_count, 0), g.value
FROM target_node n CROSS JOIN cells c
LEFT JOIN grouped g ON g.node_id = n.id AND g.weekday = c.weekday AND g.hour = c.hour
ORDER BY c.weekday, c.hour`
	rows, err := s.query(ctx, q, query, nodeID, q.From, q.To, q.Timezone)
	if err != nil {
		return Heatmap{}, fmt.Errorf("query statistics heatmap: %w", err)
	}
	defer rows.Close()
	result := Heatmap{NodeID: nodeID, Metric: q.Metric, Timezone: q.Timezone, Cells: make([]HeatCell, 0, 168)}
	for rows.Next() {
		var id string
		var cell HeatCell
		if err := rows.Scan(&id, &cell.Weekday, &cell.Hour, &cell.Count, &cell.Value); err != nil {
			return Heatmap{}, fmt.Errorf("scan statistics heatmap: %w", err)
		}
		result.Cells = append(result.Cells, cell)
	}
	if err := rows.Err(); err != nil {
		return Heatmap{}, fmt.Errorf("read statistics heatmap: %w", err)
	}
	if len(result.Cells) == 0 {
		return Heatmap{}, ErrNotFound
	}
	return result, nil
}

func (s *Store) Distribution(ctx context.Context, q Query) ([]Distribution, error) {
	column, err := q.Metric.column()
	if err != nil {
		return nil, err
	}
	if q.NodesSet && len(q.NodeIDs) == 0 {
		return []Distribution{}, nil
	}
	query := distributionSQL(column)
	rows, err := s.query(ctx, q, query, q.nodeIDs(), q.From, q.To, q.Bins)
	if err != nil {
		return nil, fmt.Errorf("query statistics distribution: %w", err)
	}
	defer rows.Close()
	items := make([]Distribution, 0)
	byID := make(map[string]int)
	for rows.Next() {
		var id string
		var total int
		var min, max, width *float64
		var bin *int
		var count int
		if err := rows.Scan(&id, &total, &min, &max, &width, &bin, &count); err != nil {
			return nil, fmt.Errorf("scan statistics distribution: %w", err)
		}
		idx, ok := byID[id]
		if !ok {
			idx = len(items)
			byID[id] = idx
			items = append(items, Distribution{NodeID: id, Metric: q.Metric, Bins: make([]Bin, 0), Count: total})
		}
		if bin == nil || total == 0 || min == nil || max == nil || width == nil {
			continue
		}
		low, high := *min, *max
		if low == high {
			low -= *width / 2
			high += *width / 2
		}
		step := (high - low) / float64(q.Bins)
		from := low + float64(*bin-1)*step
		to := low + float64(*bin)*step
		if *bin == q.Bins {
			to = high
		}
		items[idx].Bins = append(items[idx].Bins, Bin{From: from, To: to, Count: count, Percent: float64(count) / float64(total) * 100})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read statistics distribution: %w", err)
	}
	return items, nil
}

func distributionSQL(column string) string {
	return `WITH selected_nodes AS (` + selectedNodes + `), metric_values AS MATERIALIZED (
 SELECT r.node_id, r.` + column + ` AS value
 FROM results r JOIN selected_nodes n ON r.node_id = n.id
 WHERE r.ts >= $2 AND r.ts < $3 AND r.status = 'ok' AND r.` + column + ` IS NOT NULL
), bounds AS (
 SELECT n.id, count(v.value)::int AS samples, min(v.value) AS low, max(v.value) AS high,
        CASE WHEN min(v.value) = max(v.value) THEN greatest(abs(min(v.value)) * 0.02, 1.0::float8)
             ELSE max(v.value) - min(v.value) END AS width
 FROM selected_nodes n LEFT JOIN metric_values v ON v.node_id = n.id
 GROUP BY n.id
), bucket_counts AS (
 SELECT b.id, least($4, greatest(1, width_bucket(v.value,
        CASE WHEN b.low = b.high THEN b.low - b.width / 2 ELSE b.low END,
        CASE WHEN b.low = b.high THEN b.high + b.width / 2 ELSE b.high END, $4))) AS bin,
        count(*)::int AS bin_count
 FROM bounds b JOIN metric_values v ON v.node_id = b.id
 GROUP BY b.id, bin
)
SELECT n.id::text, b.samples, b.low, b.high, b.width, bins.bin, coalesce(c.bin_count, 0)
FROM selected_nodes n JOIN bounds b ON b.id = n.id
LEFT JOIN LATERAL (SELECT generate_series(1, $4)::int AS bin WHERE b.samples > 0) bins ON true
LEFT JOIN bucket_counts c ON c.id = n.id AND c.bin = bins.bin
ORDER BY n.id, bins.bin`
}

// summarizeValues applies the UI's percentile interpolation and population standard deviation.
func summarizeValues(values []float64) MetricSummary {
	if len(values) == 0 {
		return MetricSummary{}
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	mean := 0.0
	for _, value := range sorted {
		mean += value
	}
	mean /= float64(len(sorted))
	variance := 0.0
	for _, value := range sorted {
		delta := value - mean
		variance += delta * delta
	}
	std := math.Sqrt(variance / float64(len(sorted)))
	p50 := number(percentile(sorted, .5))
	return MetricSummary{N: len(sorted), Mean: number(mean), Median: p50, Min: number(sorted[0]), P50: p50,
		Max: number(sorted[len(sorted)-1]), P5: number(percentile(sorted, .05)), P95: number(percentile(sorted, .95)),
		P99: number(percentile(sorted, .99)), Std: number(std)}
}

func percentile(sorted []float64, p float64) float64 {
	index := float64(len(sorted)-1) * p
	low := int(math.Floor(index))
	high := int(math.Ceil(index))
	return sorted[low] + (sorted[high]-sorted[low])*(index-float64(low))
}

func combinedSeries(series []Series, metric Metric) Series {
	length := len(series[0].Points)
	combined := Series{Name: "Combined total", Metric: metric, Unit: metric.unit(), Points: make([]Point, 0, length)}
	values := make([]float64, 0, length)
	for pointIndex := 0; pointIndex < length; pointIndex++ {
		point := Point{TS: series[0].Points[pointIndex].TS}
		hasValue := false
		for _, item := range series {
			value := item.Points[pointIndex].Value
			if value == nil {
				continue
			}
			hasValue = true
			point.Count += item.Points[pointIndex].Count
			if point.Value == nil {
				sum := *value
				point.Value = &sum
			} else {
				sum := *point.Value + *value
				point.Value = &sum
			}
		}
		if !hasValue {
			point.Value = nil
			point.Count = 0
		} else {
			values = append(values, *point.Value)
		}
		combined.Points = append(combined.Points, point)
	}
	combined.Summary = summarizeValues(values)
	return combined
}
