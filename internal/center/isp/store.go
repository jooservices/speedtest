package isp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrNotFound reports an unknown ISP or plan.
var ErrNotFound = errors.New("isp not found")

// Pool is the database surface Store needs (pool or test fake).
type Pool interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// Store serves owner ISP reads and plan writes.
type Store struct {
	DB  Pool
	Now func() time.Time
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Query scopes ISP statistics. Engine isolation matches JOOST-71: empty engine
// uses each node's primary engine; engines are never mixed in one average.
type Query struct {
	Engine   string
	From, To time.Time
	ISPIDs   []string
}

// engineJoin returns "r.engine = …" and optional argument for an explicit engine.
func engineJoin(engine string, index int) (string, []any) {
	if engine == "" {
		return "r.engine = n.primary_engine", nil
	}
	return fmt.Sprintf("r.engine = $%d", index), []any{engine}
}

const ispAggSelect = `i.id::text, i.asn, i.organisation, i.name,
	COUNT(DISTINCT r.node_id)::int,
	AVG(r.down_mbps) FILTER (WHERE r.status <> 'failed'),
	percentile_cont(0.95) WITHIN GROUP (ORDER BY r.down_mbps) FILTER (WHERE r.status <> 'failed'),
	AVG(CASE WHEN r.status <> 'failed' THEN r.down_mbps / NULLIF(r.expected_down_mbps,0) * 100 END),
	CASE WHEN COUNT(r.id)=0 THEN NULL ELSE COUNT(r.id) FILTER (WHERE r.status <> 'failed')::float8 / COUNT(r.id) END,
	AVG(r.ping_ms) FILTER (WHERE r.status <> 'failed')`

func scopedResults(fromIdx, toIdx, engineIdx int, engine string) (string, []any) {
	join, extra := engineJoin(engine, engineIdx)
	sql := fmt.Sprintf(`SELECT r.*, n.expected_down_mbps, n.primary_engine
FROM results r JOIN nodes n ON n.id = r.node_id AND %s
WHERE r.ts >= $%d AND r.ts < $%d`, join, fromIdx, toIdx)
	return sql, extra
}

// List returns ISPs with aggregate metrics for the window.
func (s *Store) List(ctx context.Context, q Query) ([]map[string]any, error) {
	scoped, extra := scopedResults(1, 2, 3, q.Engine)
	args := append([]any{q.From, q.To}, extra...)
	rows, err := s.DB.Query(ctx, `SELECT `+ispAggSelect+`
FROM isps i
LEFT JOIN (`+scoped+`) r ON r.isp_id = i.id
GROUP BY i.id
ORDER BY COUNT(r.id) DESC, lower(i.name), i.asn`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanISPRows(rows)
}

func scanISPRows(rows pgx.Rows) ([]map[string]any, error) {
	items := []map[string]any{}
	for rows.Next() {
		item, err := scanISP(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func scanISP(row pgx.Row) (map[string]any, error) {
	var id, org, name string
	var asn uint
	var nodes int
	var avgDown, p95, pct, success, ping *float64
	if err := row.Scan(&id, &asn, &org, &name, &nodes, &avgDown, &p95, &pct, &success, &ping); err != nil {
		return nil, err
	}
	return map[string]any{
		"id": id, "asn": asn, "organisation": org, "name": name, "nodeCount": nodes,
		"avgDown": avgDown, "p95Down": p95, "pctExpected": pct, "successRate": success, "avgPing": ping,
	}, nil
}

// Get returns one ISP with the same aggregate shape as List.
func (s *Store) Get(ctx context.Context, id string, q Query) (map[string]any, error) {
	if !ValidID(id) {
		return nil, ErrNotFound
	}
	scoped, extra := scopedResults(2, 3, 4, q.Engine)
	args := append([]any{id, q.From, q.To}, extra...)
	item, err := scanISP(s.DB.QueryRow(ctx, `SELECT `+ispAggSelect+`
FROM isps i
LEFT JOIN (`+scoped+`) r ON r.isp_id = i.id
WHERE i.id=$1::uuid GROUP BY i.id`, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return item, err
}

// Trend returns average download points, using hourly buckets for rolling-day windows.
func (s *Store) Trend(ctx context.Context, id string, q Query) ([]map[string]any, error) {
	if !ValidID(id) {
		return nil, ErrNotFound
	}
	var exists bool
	if err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM isps WHERE id=$1::uuid)`, id).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	join, extra := engineJoin(q.Engine, 4)
	args := append([]any{id, q.From, q.To}, extra...)
	rows, err := s.DB.Query(ctx, `
SELECT to_timestamp(AVG(EXTRACT(EPOCH FROM r.ts))) AS bucket,
	AVG(r.down_mbps) FILTER (WHERE r.status <> 'failed'),
	AVG(r.up_mbps) FILTER (WHERE r.status <> 'failed'),
	AVG(r.ping_ms) FILTER (WHERE r.status <> 'failed'),
	COUNT(*)::int
FROM results r
JOIN nodes n ON n.id=r.node_id AND `+join+`
WHERE r.isp_id=$1::uuid AND r.ts >= $2 AND r.ts < $3
GROUP BY date_trunc(CASE WHEN $3 - $2 <= interval '24 hours' THEN 'hour' ELSE 'day' END, r.ts AT TIME ZONE 'UTC')
ORDER BY date_trunc(CASE WHEN $3 - $2 <= interval '24 hours' THEN 'hour' ELSE 'day' END, r.ts AT TIME ZONE 'UTC')`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	points := []map[string]any{}
	for rows.Next() {
		var ts time.Time
		var down, up, ping *float64
		var n int
		if err := rows.Scan(&ts, &down, &up, &ping, &n); err != nil {
			return nil, err
		}
		points = append(points, map[string]any{
			"ts": ts.UTC().Format(time.RFC3339Nano), "down": down, "up": up, "ping": ping, "n": n,
		})
	}
	return points, rows.Err()
}

// Compare returns aggregate rows for the requested ISP IDs.
func (s *Store) Compare(ctx context.Context, q Query) ([]map[string]any, error) {
	if len(q.ISPIDs) == 0 {
		return []map[string]any{}, nil
	}
	for _, id := range q.ISPIDs {
		if !ValidID(id) {
			return nil, ErrInvalid
		}
	}
	scoped, extra := scopedResults(2, 3, 4, q.Engine)
	args := append([]any{q.ISPIDs, q.From, q.To}, extra...)
	rows, err := s.DB.Query(ctx, `SELECT `+ispAggSelect+`
FROM isps i
LEFT JOIN (`+scoped+`) r ON r.isp_id = i.id
WHERE i.id = ANY($1::uuid[])
GROUP BY i.id
ORDER BY lower(i.name), i.asn`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanISPRows(rows)
}

// ListPlans returns plans for one ISP.
func (s *Store) ListPlans(ctx context.Context, ispID string) ([]map[string]any, error) {
	if err := s.requireISP(ctx, ispID); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT id::text, name, advertised_down_mbps, advertised_up_mbps, created_at, updated_at
		FROM isp_plans WHERE isp_id=$1::uuid ORDER BY lower(name), id`, ispID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		item, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) requireISP(ctx context.Context, ispID string) error {
	if !ValidID(ispID) {
		return ErrNotFound
	}
	var exists bool
	if err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM isps WHERE id=$1::uuid)`, ispID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	return nil
}

func scanPlan(row pgx.Row) (map[string]any, error) {
	var id, name string
	var down, up float64
	var created, updated time.Time
	if err := row.Scan(&id, &name, &down, &up, &created, &updated); err != nil {
		return nil, err
	}
	return map[string]any{
		"id": id, "name": name, "advertisedDown": down, "advertisedUp": up,
		"createdAt": created.UTC().Format(time.RFC3339Nano), "updatedAt": updated.UTC().Format(time.RFC3339Nano),
	}, nil
}

// CreatePlan inserts a plan under an ISP.
func (s *Store) CreatePlan(ctx context.Context, ispID, name string, down, up float64) (map[string]any, error) {
	if err := s.requireISP(ctx, ispID); err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if !validPlan(name, down, up) {
		return nil, ErrInvalid
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	if _, err = s.DB.Exec(ctx, `INSERT INTO isp_plans(id,isp_id,name,advertised_down_mbps,advertised_up_mbps)
		VALUES ($1::uuid,$2::uuid,$3,$4,$5)`, id, ispID, name, down, up); err != nil {
		if isUnique(err) {
			return nil, ErrInvalid
		}
		return nil, err
	}
	return s.getPlan(ctx, id)
}

// UpdatePlan replaces plan fields.
func (s *Store) UpdatePlan(ctx context.Context, ispID, planID, name string, down, up float64) (map[string]any, error) {
	if !ValidID(ispID) || !ValidID(planID) {
		return nil, ErrNotFound
	}
	name = strings.TrimSpace(name)
	if !validPlan(name, down, up) {
		return nil, ErrInvalid
	}
	var updatedID string
	err := s.DB.QueryRow(ctx, `WITH old AS MATERIALIZED (
		SELECT id FROM isp_plans
		WHERE id=$1::uuid AND isp_id=$2::uuid FOR UPDATE
	), updated AS (
		UPDATE isp_plans p SET name=$3,advertised_down_mbps=$4,advertised_up_mbps=$5,updated_at=$6
		FROM old o WHERE p.id=o.id RETURNING p.id
	), sync_defaults AS (
		UPDATE nodes n SET
			expected_down_mbps=CASE WHEN n.expected_down_isp_default THEN $4 ELSE n.expected_down_mbps END,
			expected_up_mbps=CASE WHEN n.expected_up_isp_default THEN $5 ELSE n.expected_up_mbps END,
			config_revision=n.config_revision+1,updated_at=now()
		FROM updated u WHERE n.isp_plan_id=u.id AND (n.expected_down_isp_default OR n.expected_up_isp_default)
		RETURNING n.id
	)
	SELECT id::text FROM updated`, planID, ispID, name, down, up, s.now()).Scan(&updatedID)
	if err != nil {
		if isUnique(err) {
			return nil, ErrInvalid
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return s.getPlan(ctx, planID)
}

func validPlan(name string, down, up float64) bool {
	return utf8.ValidString(name) && utf8.RuneCountInString(name) > 0 && utf8.RuneCountInString(name) <= 200 &&
		down > 0 && up > 0 && !math.IsNaN(down) && !math.IsNaN(up) && !math.IsInf(down, 0) && !math.IsInf(up, 0)
}

// DeletePlan removes a plan.
func (s *Store) DeletePlan(ctx context.Context, ispID, planID string) error {
	if !ValidID(ispID) || !ValidID(planID) {
		return ErrNotFound
	}
	tag, err := s.DB.Exec(ctx, `DELETE FROM isp_plans WHERE id=$1::uuid AND isp_id=$2::uuid`, planID, ispID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) getPlan(ctx context.Context, id string) (map[string]any, error) {
	item, err := scanPlan(s.DB.QueryRow(ctx, `SELECT id::text, name, advertised_down_mbps, advertised_up_mbps, created_at, updated_at
		FROM isp_plans WHERE id=$1::uuid`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return item, err
}

func isUnique(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

// ByID loads a bare ISP entity.
func ByID(ctx context.Context, db DB, id string) (*Entity, error) {
	if !ValidID(id) {
		return nil, ErrNotFound
	}
	var e Entity
	err := db.QueryRow(ctx, `SELECT id::text, asn, organisation, name FROM isps WHERE id=$1::uuid`, id).
		Scan(&e.ID, &e.ASN, &e.Organisation, &e.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}
