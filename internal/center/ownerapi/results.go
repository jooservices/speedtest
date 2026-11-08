package ownerapi

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var uuidText = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func validUUID(id string) bool { return uuidText.MatchString(id) }

// ResultFilter selects results for the list and the exports.
type ResultFilter struct {
	Engine     string
	AllEngines bool
	// Nodes limits results to these node IDs. NodesSet with no IDs matches nothing.
	Nodes    []string
	NodesSet bool
	// ISPs limits results to these ISP IDs. ISPsSet with no IDs matches nothing.
	ISPs    []string
	ISPsSet bool
	// From is inclusive and To is exclusive.
	From, To time.Time
	// Status is "", "ok", "degraded" or "failed".
	Status string
	// Query is a case-insensitive substring searched in the IP, server, location, node and error.
	Query            string
	MinDown, MaxDown *float64
	// Sort is one of the keys in resultSorts; Dir is "asc" or "desc".
	Sort, Dir string
}

// resultSorts maps the public sort keys to SQL expressions. User input never reaches the query text.
var resultSorts = map[string]string{
	"ts": "r.ts", "node": "lower(n.name)", "down": "r.down_mbps", "up": "r.up_mbps", "ping": "r.ping_ms",
	"jitter": "r.jitter_ms", "loss": "r.loss_percent", "server": "lower(r.server->>'city')", "ip": "r.ip", "status": "r.status",
}

// ValidResultSort reports whether key is an allowed sort key.
func ValidResultSort(key string) bool { _, ok := resultSorts[key]; return ok }

const resultFrom = ` FROM results r JOIN nodes n ON n.id = r.node_id LEFT JOIN ip_addresses a ON a.ip = r.ip`

const resultSelect = `SELECT ` + ResultColumns + `, n.name, n.label, a.source, a.geo`

// likePattern escapes LIKE wildcards in text and wraps it for a substring search.
func likePattern(text string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(text) + "%"
}

// where builds the WHERE clause and its arguments; ok is false when the filter can match nothing.
func (f ResultFilter) where() (clause string, args []any, ok bool) {
	var conds []string
	add := func(cond string, value any) {
		args = append(args, value)
		conds = append(conds, strings.ReplaceAll(cond, "?", fmt.Sprintf("$%d", len(args))))
	}
	if !f.AllEngines {
		if f.Engine == "" {
			conds = append(conds, "r.engine = n.primary_engine")
		} else {
			add("r.engine = ?", f.Engine)
		}
	}
	if f.NodesSet {
		if len(f.Nodes) == 0 {
			return "", nil, false
		}
		add("r.node_id = ANY(?::uuid[])", f.Nodes)
	}
	if f.ISPsSet {
		if len(f.ISPs) == 0 {
			return "", nil, false
		}
		add("r.isp_id = ANY(?::uuid[])", f.ISPs)
	}
	if !f.From.IsZero() {
		add("r.ts >= ?", f.From)
	}
	if !f.To.IsZero() {
		add("r.ts < ?", f.To)
	}
	if f.Status != "" && f.Status != "all" {
		add("r.status = ?", f.Status)
	}
	if f.MinDown != nil {
		add("r.down_mbps >= ?", *f.MinDown)
	}
	if f.MaxDown != nil {
		add("r.down_mbps <= ?", *f.MaxDown)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		args = append(args, likePattern(q))
		p := fmt.Sprintf("$%d", len(args))
		var terms []string
		for _, expr := range []string{"host(r.ip)", "r.server->>'name'", "r.server->>'city'", "r.server->>'sponsor'",
			"a.geo->>'city'", "a.geo->>'organisation'", "a.geo->>'country'", "n.name", "n.label", "r.error"} {
			terms = append(terms, expr+` ILIKE `+p+` ESCAPE '\'`)
		}
		conds = append(conds, "("+strings.Join(terms, " OR ")+")")
	}
	if len(conds) == 0 {
		return "", args, true
	}
	return " WHERE " + strings.Join(conds, " AND "), args, true
}

func (f ResultFilter) orderBy() (string, error) {
	sort := f.Sort
	if sort == "" {
		sort = "ts"
	}
	expr, ok := resultSorts[sort]
	if !ok {
		return "", ErrInvalid
	}
	dir := "DESC"
	switch strings.ToLower(f.Dir) {
	case "", "desc":
	case "asc":
		dir = "ASC"
	default:
		return "", ErrInvalid
	}
	return fmt.Sprintf(" ORDER BY %s %s NULLS LAST, r.id %s", expr, dir, dir), nil
}

// ResultRow is one result with the node name needed by the CSV export.
type ResultRow struct {
	Result    map[string]any
	NodeName  string
	NodeLabel string
}

func scanResultRow(row pgx.Row) (ResultRow, error) {
	var name, label string
	var source *string
	var geo []byte
	view, err := ScanResult(row, &name, &label, &source, &geo)
	if err != nil {
		return ResultRow{}, err
	}
	if source != nil {
		if g := GeoObject(view.IP, *source, geo); g != nil {
			view.Object["geo"] = g
		}
	}
	return ResultRow{Result: view.Object, NodeName: name, NodeLabel: label}, nil
}

// ListResults returns one page of results and the total number of matches.
func (s *Store) ListResults(ctx context.Context, f ResultFilter, page, size int) ([]map[string]any, int, error) {
	order, err := f.orderBy()
	if err != nil {
		return nil, 0, err
	}
	where, args, ok := f.where()
	if !ok {
		return []map[string]any{}, 0, nil
	}
	var total int
	if err := s.DB.QueryRow(ctx, `SELECT count(*)`+resultFrom+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, size, (page-1)*size)
	rows, err := s.DB.Query(ctx, resultSelect+resultFrom+where+order+fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		r, err := scanResultRow(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, r.Result)
	}
	return items, total, rows.Err()
}

// GetResult returns one result with every stored field and the location of its IP.
func (s *Store) GetResult(ctx context.Context, id string) (map[string]any, error) {
	if !validUUID(id) {
		return nil, ErrNotFound
	}
	r, err := scanResultRow(s.DB.QueryRow(ctx, resultSelect+resultFrom+` WHERE r.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return r.Result, nil
}

// ExportResults streams every matching result to emit in the filter's order, one row at a time, so memory
// does not grow with the size of the export. emit returning an error stops the export.
func (s *Store) ExportResults(ctx context.Context, f ResultFilter, emit func(ResultRow) error) error {
	order, err := f.orderBy()
	if err != nil {
		return err
	}
	where, args, ok := f.where()
	if !ok {
		return nil
	}
	rows, err := s.DB.Query(ctx, resultSelect+resultFrom+where+order, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanResultRow(rows)
		if err != nil {
			return err
		}
		if err := emit(r); err != nil {
			return err
		}
	}
	return rows.Err()
}
