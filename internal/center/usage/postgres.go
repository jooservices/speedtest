package usage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jooservices/speedtest/internal/center/agentapi"
)

var ErrNotFound = errors.New("usage resource not found")
var ErrPrecondition = errors.New("usage precondition required")
var ErrStale = errors.New("usage resource changed")

var nodeIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// DB is the PostgreSQL pool subset used by accounting and policy hooks.
type DB interface {
	Begin(context.Context) (pgx.Tx, error)
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Store is the usage query API and transactional budget policy hook.
type Store struct {
	DB              DB
	Now             func() time.Time
	budgetMu        sync.RWMutex
	budgetKnown     bool
	budgetExists    bool
	budgetCheckedAt time.Time
}

const budgetDiscoveryTTL = time.Second

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func validNodeID(id string) bool { return nodeIDPattern.MatchString(id) }

func appendFilter(where *[]string, args *[]any, expression string, value any) {
	*args = append(*args, value)
	*where = append(*where, fmt.Sprintf(expression, len(*args)))
}

func usageFilters(q Query, withRange bool) (string, []any) {
	where, args := usageFilterConditions(q, "r", withRange)
	if len(where) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(where, " AND "), args
}

func usageFilterConditions(q Query, alias string, withRange bool) ([]string, []any) {
	var where []string
	var args []any
	if q.NodeID != "" {
		appendFilter(&where, &args, alias+".node_id = $%d", q.NodeID)
	}
	if q.Engine != "" {
		appendFilter(&where, &args, alias+".engine = $%d", q.Engine)
	}
	if q.ISPID != "" {
		appendFilter(&where, &args, alias+".isp_id = $%d", q.ISPID)
	}
	if withRange {
		appendFilter(&where, &args, alias+".ts >= $%d", q.From)
		appendFilter(&where, &args, alias+".ts < $%d", q.To)
	}
	return where, args
}

func addBytes(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

func (s *Store) Usage(ctx context.Context, q Query) (Response, error) {
	now := s.now()
	q, err := q.Normalize(now)
	if err != nil || q.NodeID != "" && !validNodeID(q.NodeID) || q.ISPID != "" && !validNodeID(q.ISPID) {
		return Response{}, ErrInvalid
	}
	result := Response{From: q.From, To: q.To, Group: q.Group}
	monthStart := MonthStart(now)
	previousStart := monthStart.AddDate(0, -1, 0)
	if result.ByNodeEngine, err = s.nodeEngineTotals(ctx, q, monthStart, previousStart, now); err != nil {
		return Response{}, err
	}
	for _, item := range result.ByNodeEngine {
		result.Totals = sumBytes(result.Totals, item.Bytes)
		result.MonthToDate = sumBytes(result.MonthToDate, item.MonthToDate)
		result.PreviousMonth = sumBytes(result.PreviousMonth, item.PreviousMonth)
	}
	result.ProjectedMonthEnd = projectBytes(result.MonthToDate, now)
	result.Comparison = comparison(result.MonthToDate.Total, result.PreviousMonth.Total, result.ProjectedMonthEnd.Total)
	if result.Series, err = s.series(ctx, q); err != nil {
		return Response{}, err
	}
	result.Budgets, err = s.budgetStatus(ctx, q.NodeID, monthStart, now)
	return result, err
}

func sumBytes(a, b Bytes) Bytes {
	a.Down, a.Up = addBytes(a.Down, b.Down), addBytes(a.Up, b.Up)
	a.Total = addBytes(a.Down, a.Up)
	return a
}

func projectBytes(b Bytes, now time.Time) Bytes {
	down, up := ProjectMonthEnd(b.Down, now), ProjectMonthEnd(b.Up, now)
	return Bytes{Down: down, Up: up, Total: addBytes(down, up)}
}

func comparison(current, previous, projected int64) Comparison {
	result := Comparison{CurrentMonthToDate: current, PreviousMonth: previous, ProjectedMonthEnd: projected}
	if previous > 0 {
		change := (float64(projected) - float64(previous)) / float64(previous) * 100
		result.ChangePercent = &change
	}
	return result
}

func (s *Store) nodeEngineTotals(ctx context.Context, q Query, monthStart, previousStart, now time.Time) ([]NodeEngine, error) {
	dimensions, args := usageFilterConditions(q, "r", false)
	fromParam, toParam, monthParam, nowParam, previousParam := len(args)+1, len(args)+2, len(args)+3, len(args)+4, len(args)+5
	args = append(args, q.From, q.To, monthStart, now, previousStart)
	query := fmt.Sprintf(`SELECT r.node_id::text,n.name,r.engine,COALESCE(r.isp_id::text,''),COALESCE(i.name,''),
 COALESCE(sum(r.bytes_down) FILTER (WHERE r.ts >= $%d AND r.ts < $%d),0),
 COALESCE(sum(r.bytes_up) FILTER (WHERE r.ts >= $%d AND r.ts < $%d),0),
 COALESCE(sum(r.bytes_down) FILTER (WHERE r.ts >= $%d AND r.ts < $%d),0),
 COALESCE(sum(r.bytes_up) FILTER (WHERE r.ts >= $%d AND r.ts < $%d),0),
 COALESCE(sum(r.bytes_down) FILTER (WHERE r.ts >= $%d AND r.ts < $%d),0),
 COALESCE(sum(r.bytes_up) FILTER (WHERE r.ts >= $%d AND r.ts < $%d),0)
	 FROM results r JOIN nodes n ON n.id=r.node_id LEFT JOIN isps i ON i.id=r.isp_id
 WHERE ((r.ts >= $%d AND r.ts < $%d) OR (r.ts >= $%d AND r.ts < $%d))%s
 AND (r.bytes_down IS NOT NULL OR r.bytes_up IS NOT NULL)
	 GROUP BY r.node_id,n.name,r.engine,r.isp_id,i.name ORDER BY lower(n.name),r.engine,lower(COALESCE(i.name,'')),r.isp_id`,
		fromParam, toParam, fromParam, toParam,
		monthParam, nowParam, monthParam, nowParam,
		previousParam, monthParam, previousParam, monthParam,
		fromParam, toParam, previousParam, nowParam, filterSuffix(dimensions))
	rows, err := s.DB.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []NodeEngine{}
	for rows.Next() {
		var item NodeEngine
		if err := rows.Scan(&item.NodeID, &item.NodeName, &item.Engine, &item.ISPID, &item.ISPName, &item.Down, &item.Up,
			&item.MonthToDate.Down, &item.MonthToDate.Up, &item.PreviousMonth.Down, &item.PreviousMonth.Up); err != nil {
			return nil, err
		}
		item.Total = addBytes(item.Down, item.Up)
		item.MonthToDate.Total = addBytes(item.MonthToDate.Down, item.MonthToDate.Up)
		item.ProjectedMonthEnd = projectBytes(item.MonthToDate, now)
		item.PreviousMonth.Total = addBytes(item.PreviousMonth.Down, item.PreviousMonth.Up)
		items = append(items, item)
	}
	return items, rows.Err()
}

func filterSuffix(conditions []string) string {
	if len(conditions) == 0 {
		return ""
	}
	return " AND " + strings.Join(conditions, " AND ")
}

func (s *Store) series(ctx context.Context, q Query) ([]Bucket, error) {
	conditions, args := usageFilterConditions(q, "r", true)
	groupParam := len(args) + 1
	args = append(args, q.Group)
	query := fmt.Sprintf(`SELECT r.node_id::text,r.engine,COALESCE(r.isp_id::text,''),COALESCE(i.name,''),date_trunc($%d,r.ts AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',COALESCE(sum(r.bytes_down),0),COALESCE(sum(r.bytes_up),0)
	 FROM results r LEFT JOIN isps i ON i.id=r.isp_id WHERE (r.bytes_down IS NOT NULL OR r.bytes_up IS NOT NULL)%s GROUP BY r.node_id,r.engine,r.isp_id,i.name,5 ORDER BY 5,r.node_id,r.engine,r.isp_id`, groupParam, filterSuffix(conditions))
	rows, err := s.DB.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Bucket{}
	for rows.Next() {
		var item Bucket
		if err := rows.Scan(&item.NodeID, &item.Engine, &item.ISPID, &item.ISPName, &item.Start, &item.Down, &item.Up); err != nil {
			return nil, err
		}
		item.Start = item.Start.UTC()
		item.Total = addBytes(item.Down, item.Up)
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) budgets(ctx context.Context) ([]Budget, error) {
	rows, err := s.DB.Query(ctx, `SELECT scope_key,COALESCE(node_id::text,''),monthly_bytes,action,widen_multiplier,revision
 FROM usage_budgets ORDER BY scope_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Budget{}
	for rows.Next() {
		var item Budget
		if err := rows.Scan(&item.ScopeKey, &item.NodeID, &item.MonthlyBytes, &item.Action, &item.WidenMultiplier, &item.Revision); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// Budgets returns configured limits and a stable collection ETag.
func (s *Store) Budgets(ctx context.Context) ([]Budget, string, error) {
	items, err := s.budgets(ctx)
	if err != nil {
		return nil, "", err
	}
	return items, budgetsETag(items), nil
}

func budgetsETag(items []Budget) string {
	h := sha256.New()
	for _, b := range items {
		_, _ = fmt.Fprintf(h, "%s:%d:%s:%d:%d;", b.ScopeKey, b.MonthlyBytes, b.Action, b.WidenMultiplier, b.Revision)
	}
	return `"usage-budgets-` + hex.EncodeToString(h.Sum(nil))[:16] + `"`
}

func (s *Store) lockBudgets(ctx context.Context, tx pgx.Tx, ifMatch string) ([]Budget, error) {
	if ifMatch == "" {
		return nil, ErrPrecondition
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('speedtest:usage-budgets'))`); err != nil {
		return nil, err
	}
	items, err := s.budgets(ctx)
	if err != nil {
		return nil, err
	}
	if budgetsETag(items) != ifMatch {
		return nil, ErrStale
	}
	return items, nil
}

// LockBudgetEvaluation serializes budget and schedule policy changes in a stable DB lock order.
func LockBudgetEvaluation(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('speedtest:usage-budget-evaluation'))`)
	return err
}

func lockResultsForBudgetChange(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `LOCK TABLE results IN SHARE MODE`)
	return err
}

func lockBudgetLifecycle(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('speedtest:usage-budget-lifecycle'))`)
	return err
}

// ReplaceBudget creates or updates a single node or global budget conditionally.
func (s *Store) ReplaceBudget(ctx context.Context, b Budget, ifMatch string) ([]Budget, string, error) {
	switch {
	case b.NodeID == "":
		b.ScopeKey = "global"
	case validNodeID(b.NodeID):
		b.ScopeKey = strings.ToLower(b.NodeID)
	default:
		return nil, "", ErrInvalid
	}
	if b.WidenMultiplier == 0 {
		b.WidenMultiplier = 2
	}
	if err := ValidateBudget(b); err != nil {
		return nil, "", err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockResultsForBudgetChange(ctx, tx); err != nil {
		return nil, "", err
	}
	if err := lockBudgetLifecycle(ctx, tx); err != nil {
		return nil, "", err
	}
	if err := LockBudgetEvaluation(ctx, tx); err != nil {
		return nil, "", err
	}
	items, err := s.lockBudgets(ctx, tx, ifMatch)
	if err != nil {
		return nil, "", err
	}
	var revision int64 = 1
	for _, existing := range items {
		if existing.ScopeKey == b.ScopeKey {
			revision = existing.Revision + 1
		}
	}
	if err := refreshCurrentMonthUsage(ctx, tx, s.now()); err != nil {
		return nil, "", err
	}
	_, err = tx.Exec(ctx, `INSERT INTO usage_budgets(scope_key,node_id,monthly_bytes,action,widen_multiplier,revision,updated_at)
 VALUES($1,NULLIF($2,'')::uuid,$3,$4,$5,$6,now()) ON CONFLICT(scope_key) DO UPDATE SET
 monthly_bytes=EXCLUDED.monthly_bytes,action=EXCLUDED.action,widen_multiplier=EXCLUDED.widen_multiplier,revision=EXCLUDED.revision,updated_at=now()`,
		b.ScopeKey, b.NodeID, b.MonthlyBytes, b.Action, b.WidenMultiplier, revision)
	if err != nil {
		return nil, "", err
	}
	now := s.now()
	if err := s.evaluateBudgets(ctx, tx, b.NodeID, MonthStart(now), now); err != nil {
		return nil, "", err
	}
	s.setBudgetCache(true)
	if err := tx.Commit(ctx); err != nil {
		s.invalidateBudgetCache()
		return nil, "", err
	}
	return s.Budgets(ctx)
}

func refreshCurrentMonthUsage(ctx context.Context, tx pgx.Tx, now time.Time) error {
	start := MonthStart(now)
	end := start.AddDate(0, 1, 0)
	if _, err := tx.Exec(ctx, `DELETE FROM usage_monthly_totals WHERE period_start=$1::date`, start); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO usage_monthly_totals(period_start,node_id,engine,bytes_down,bytes_up)
 SELECT $1::date,node_id,engine,COALESCE(sum(bytes_down),0),COALESCE(sum(bytes_up),0)
 FROM results WHERE ts >= $1 AND ts < $2 GROUP BY node_id,engine`, start, end)
	return err
}

// DeleteBudget removes one configured limit under the collection ETag.
func (s *Store) DeleteBudget(ctx context.Context, scope, ifMatch string) ([]Budget, string, error) {
	if scope != "global" && !validNodeID(scope) {
		return nil, "", ErrNotFound
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockResultsForBudgetChange(ctx, tx); err != nil {
		return nil, "", err
	}
	if err := lockBudgetLifecycle(ctx, tx); err != nil {
		return nil, "", err
	}
	if err := LockBudgetEvaluation(ctx, tx); err != nil {
		return nil, "", err
	}
	if _, err := s.lockBudgets(ctx, tx, ifMatch); err != nil {
		return nil, "", err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM usage_budgets WHERE scope_key=$1`, strings.ToLower(scope))
	if err != nil {
		return nil, "", err
	}
	if tag.RowsAffected() == 0 {
		return nil, "", ErrNotFound
	}
	var budgetsRemain bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM usage_budgets)`).Scan(&budgetsRemain); err != nil {
		return nil, "", err
	}
	if err := refreshCurrentMonthUsage(ctx, tx, s.now()); err != nil {
		return nil, "", err
	}
	if err := s.evaluateBudgets(ctx, tx, "", MonthStart(s.now()), s.now()); err != nil {
		return nil, "", err
	}
	s.setBudgetCache(budgetsRemain)
	if err := tx.Commit(ctx); err != nil {
		s.invalidateBudgetCache()
		return nil, "", err
	}
	return s.Budgets(ctx)
}

func (s *Store) budgetStatus(ctx context.Context, selectedNode string, from, to time.Time) ([]BudgetStatus, error) {
	items, err := s.budgets(ctx)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return []BudgetStatus{}, nil
	}
	rows, err := s.DB.Query(ctx, `SELECT node_id::text,COALESCE(sum(bytes_down),0),COALESCE(sum(bytes_up),0) FROM usage_monthly_totals
	 WHERE period_start=$1::date GROUP BY node_id`, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	perNode := map[string]int64{}
	var global int64
	for rows.Next() {
		var id string
		var down, up int64
		if err := rows.Scan(&id, &down, &up); err != nil {
			return nil, err
		}
		used := addBytes(down, up)
		perNode[id] = used
		global = addBytes(global, used)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	statuses := []BudgetStatus{}
	for _, b := range items {
		if selectedNode != "" && b.ScopeKey != "global" && b.ScopeKey != selectedNode {
			continue
		}
		used := global
		if b.ScopeKey != "global" {
			used = perNode[b.NodeID]
		}
		ratio := float64(used) / float64(b.MonthlyBytes)
		atMaximum, err := s.budgetAtMaximumInterval(ctx, b, from)
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, BudgetStatus{ScopeKey: b.ScopeKey, NodeID: b.NodeID, MonthlyBytes: b.MonthlyBytes,
			UsedBytes: used, Ratio: ratio, Action: b.Action, AtMaximumInterval: atMaximum, Alert: ratio >= BudgetAlertRatio})
	}
	return statuses, nil
}

func (s *Store) budgetAtMaximumInterval(ctx context.Context, budget Budget, period time.Time) (bool, error) {
	var atMaximum bool
	err := s.DB.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM engine_schedules WHERE enabled AND interval_minutes=1440 AND ($1='global' OR node_id=NULLIF($2,'')::uuid)
		UNION ALL
		SELECT 1 FROM usage_budget_overrides WHERE period_start=$3 AND original_interval_minutes=1440 AND original_enabled AND NOT applied_enabled AND ($1='global' OR node_id=NULLIF($2,'')::uuid))`, budget.ScopeKey, budget.NodeID, period).Scan(&atMaximum)
	return atMaximum, err
}

// Advice estimates daily and monthly traffic from the last 30 days of actual results.
func (s *Store) Advice(ctx context.Context, nodeID, engine string, interval int, budget int64) (Advice, error) {
	if !validNodeID(nodeID) || engine != "ookla" && engine != "librespeed" || interval < 5 || interval > 1440 || budget < 0 {
		return Advice{}, ErrInvalid
	}
	var average float64
	var samples int
	err := s.DB.QueryRow(ctx, `SELECT COALESCE(avg((bytes_down::numeric+bytes_up::numeric)),0)::float8,count(*) FROM results
	 WHERE node_id=$1 AND engine=$2 AND status IN ('ok','degraded') AND ts >= $3 AND bytes_down IS NOT NULL AND bytes_up IS NOT NULL`, nodeID, engine, s.now().Add(-30*24*time.Hour)).Scan(&average, &samples)
	if err != nil {
		return Advice{}, err
	}
	requestedBudget := budget
	constraints, err := s.adviceConstraints(ctx, nodeID, requestedBudget)
	if err != nil {
		return Advice{}, err
	}
	var scope string
	if len(constraints) > 0 {
		budget, scope = constraints[0].budget, constraints[0].scope
	}
	result := Advice{NodeID: nodeID, Engine: engine, IntervalMinutes: interval, BudgetBytes: budget, BudgetScope: scope, SharedMeasurementsAvailable: true}
	if samples == 0 {
		result.RecommendationStatus = "unavailable"
		return result, nil
	}
	result.MeasurementAvailable = true
	result.AverageBytesPerTest = floatPointer(average)
	perDay := average * 1440 / float64(interval)
	result.EstimatedBytesPerDay, result.EstimatedBytesPerMonth = floatPointer(perDay), floatPointer(perDay*30)
	budget, scope, remaining, complete, err := s.adviceAllowance(ctx, nodeID, engine, requestedBudget)
	if err != nil {
		return Advice{}, err
	}
	result.BudgetBytes, result.BudgetScope = budget, scope
	result.SharedMeasurementsAvailable = complete
	if budget == 0 || !complete {
		result.RecommendationStatus = "unavailable"
		return result, nil
	}
	if remaining <= 0 {
		result.RecommendationStatus = "infeasible"
		return result, nil
	}
	result.RemainingBudgetBytes = remaining
	result.RecommendedInterval = RecommendedInterval(average, remaining)
	result.RecommendationStatus = recommendationStatus(average, remaining, result.RecommendedInterval)
	return result, nil
}

func floatPointer(value float64) *float64 { return &value }

type adviceConstraint struct {
	scope  string
	budget int64
}

func (s *Store) adviceAllowance(ctx context.Context, node, engine string, requested int64) (int64, string, int64, bool, error) {
	constraints, err := s.adviceConstraints(ctx, node, requested)
	if err != nil || len(constraints) == 0 {
		return 0, "", 0, true, err
	}
	var chosen adviceConstraint
	var chosenRemaining int64
	complete := true
	for _, constraint := range constraints {
		other, measured, err := s.otherEnabledMonthlyUsage(ctx, node, engine, constraint.scope)
		if err != nil {
			return 0, "", 0, false, err
		}
		if !measured {
			complete = false
			continue
		}
		remaining := constraint.budget - int64(math.Ceil(other))
		if remaining < 0 {
			remaining = 0
		}
		if chosen.scope == "" || remaining < chosenRemaining {
			chosen, chosenRemaining = constraint, remaining
		}
	}
	if !complete {
		return chosen.budget, chosen.scope, chosenRemaining, false, nil
	}
	return chosen.budget, chosen.scope, chosenRemaining, true, nil
}

func (s *Store) adviceConstraints(ctx context.Context, node string, requested int64) ([]adviceConstraint, error) {
	rows, err := s.DB.Query(ctx, `SELECT scope_key,monthly_bytes FROM usage_budgets WHERE scope_key=$1 OR scope_key='global' ORDER BY scope_key`, node)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var local, global int64
	for rows.Next() {
		var scope string
		var amount int64
		if err := rows.Scan(&scope, &amount); err != nil {
			return nil, err
		}
		if scope == "global" {
			global = amount
		} else {
			local = amount
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if requested > 0 {
		local = requested
	}
	constraints := make([]adviceConstraint, 0, 2)
	if local > 0 {
		constraints = append(constraints, adviceConstraint{scope: node, budget: local})
	}
	if global > 0 {
		constraints = append(constraints, adviceConstraint{scope: "global", budget: global})
	}
	return constraints, nil
}

func (s *Store) otherEnabledMonthlyUsage(ctx context.Context, node, engine, scope string) (float64, bool, error) {
	rows, err := s.DB.Query(ctx, `SELECT es.node_id::text,es.engine,es.interval_minutes,
		COALESCE(avg(r.bytes_down::numeric+r.bytes_up::numeric) FILTER (WHERE r.id IS NOT NULL),0)::float8,count(r.id)
		FROM engine_schedules es LEFT JOIN results r ON r.node_id=es.node_id AND r.engine=es.engine
		AND r.status IN ('ok','degraded') AND r.ts >= $3 AND r.bytes_down IS NOT NULL AND r.bytes_up IS NOT NULL
		WHERE es.enabled AND ($4::boolean OR es.node_id=$1) AND NOT (es.node_id=$1 AND es.engine=$2)
		GROUP BY es.node_id,es.engine,es.interval_minutes`, node, engine, s.now().Add(-30*24*time.Hour), scope == "global")
	if err != nil {
		return 0, false, err
	}
	defer rows.Close()
	return sumOtherSchedules(rows, node, engine)
}

func sumOtherSchedules(rows pgx.Rows, node, engine string) (float64, bool, error) {
	var total float64
	complete := true
	for rows.Next() {
		var otherNode, otherEngine string
		var interval int
		var average float64
		var samples int
		if err := rows.Scan(&otherNode, &otherEngine, &interval, &average, &samples); err != nil {
			return 0, false, err
		}
		if otherNode == node && otherEngine == engine || interval < 1 {
			continue
		}
		if samples == 0 {
			complete = false
			continue
		}
		total += average * (30 * 24 * 60) / float64(interval)
	}
	return total, complete, rows.Err()
}

// PendingNotificationEvents reads the durable extension queue for JOOST-78 consumers.
func (s *Store) PendingNotificationEvents(ctx context.Context, limit int) ([]Event, error) {
	if limit < 1 || limit > 100 {
		return nil, ErrInvalid
	}
	rows, err := s.DB.Query(ctx, `SELECT id::text,scope_key,COALESCE(node_id::text,''),period_start,event_type,used_bytes,budget_bytes,ratio
 FROM usage_budget_events WHERE published_at IS NULL ORDER BY created_at,id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []Event{}
	for rows.Next() {
		var event Event
		if err := rows.Scan(&event.ID, &event.ScopeKey, &event.NodeID, &event.PeriodStart, &event.Type, &event.UsedBytes, &event.BudgetBytes, &event.Ratio); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

// MarkNotificationEventPublished acknowledges delivery by the application notification extension.
func (s *Store) MarkNotificationEventPublished(ctx context.Context, id string) error {
	if !validNodeID(id) {
		return ErrNotFound
	}
	tag, err := s.DB.Exec(ctx, `UPDATE usage_budget_events SET published_at=now() WHERE id=$1 AND published_at IS NULL`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func randomID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:]), nil
}

func (s *Store) AfterResultStored(ctx context.Context, tx pgx.Tx, node string, _ agentapi.Result) error {
	active, err := s.hasBudgets(ctx, tx)
	if err != nil || !active {
		return err
	}
	if err := LockBudgetEvaluation(ctx, tx); err != nil {
		return err
	}
	active, err = s.hasBudgets(ctx, tx)
	if err != nil || !active {
		return err
	}
	now := s.now()
	return s.evaluateBudgets(ctx, tx, node, MonthStart(now), now)
}

func (s *Store) hasBudgets(ctx context.Context, tx pgx.Tx) (bool, error) {
	now := time.Now()
	s.budgetMu.RLock()
	fresh, cachedPresent := s.budgetKnown && now.Sub(s.budgetCheckedAt) < budgetDiscoveryTTL, s.budgetExists
	s.budgetMu.RUnlock()
	if fresh {
		return cachedPresent, nil
	}
	var present bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM usage_budgets)`).Scan(&present); err != nil {
		return false, err
	}
	s.budgetMu.Lock()
	s.budgetKnown, s.budgetExists, s.budgetCheckedAt = true, present, now
	s.budgetMu.Unlock()
	return present, nil
}

func (s *Store) setBudgetCache(present bool) {
	s.budgetMu.Lock()
	s.budgetKnown, s.budgetExists, s.budgetCheckedAt = true, present, time.Now()
	s.budgetMu.Unlock()
}

func (s *Store) invalidateBudgetCache() {
	s.budgetMu.Lock()
	s.budgetKnown = false
	s.budgetMu.Unlock()
}

// ResetExpiredSchedules restores budget overrides after a month boundary.
func (s *Store) ResetExpiredSchedules(ctx context.Context) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := LockBudgetEvaluation(ctx, tx); err != nil {
		return err
	}
	if err := restoreExpiredOverrides(ctx, tx, MonthStart(s.now())); err != nil {
		return err
	}
	if err := s.evaluateBudgets(ctx, tx, "", MonthStart(s.now()), s.now()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Loop checks for month changes independently of test ingestion, including paused nodes.
func (s *Store) Loop(ctx context.Context, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	if err := s.ResetExpiredSchedules(ctx); err != nil && ctx.Err() == nil {
		logger.Error("usage budget reset failed", "error", err)
	}
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.ResetExpiredSchedules(ctx); err != nil && ctx.Err() == nil {
				logger.Error("usage budget reset failed", "error", err)
			}
		}
	}
}

func restoreExpiredOverrides(ctx context.Context, tx pgx.Tx, current time.Time) error {
	rows, err := tx.Query(ctx, `SELECT node_id::text,engine,period_start,original_interval_minutes,original_enabled
 FROM usage_budget_overrides WHERE period_start < $1 ORDER BY period_start,node_id,engine FOR UPDATE`, current)
	if err != nil {
		return err
	}
	type prior struct {
		node, engine string
		period       time.Time
		interval     int
		enabled      bool
	}
	var expired []prior
	for rows.Next() {
		var item prior
		if err := rows.Scan(&item.node, &item.engine, &item.period, &item.interval, &item.enabled); err != nil {
			rows.Close()
			return err
		}
		expired = append(expired, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, item := range expired {
		changed, err := restoreOverride(ctx, tx, item.node, item.engine, item.period, item.interval, item.enabled)
		if err != nil {
			return err
		}
		if changed {
			if err := recordPolicyAudit(ctx, tx, item.node, "usage_budget.reset", "Restored the schedule at the start of a new budget month"); err != nil {
				return err
			}
		}
	}
	_, err = tx.Exec(ctx, `DELETE FROM usage_budget_overrides WHERE period_start < $1`, current)
	return err
}

func (s *Store) evaluateBudgets(ctx context.Context, tx pgx.Tx, node string, period, now time.Time) error {
	if err := restoreExpiredOverrides(ctx, tx, period); err != nil {
		return err
	}
	var targetNodes []string
	if node != "" {
		var globalBudget int64
		var hasGlobal bool
		if err := tx.QueryRow(ctx, `SELECT monthly_bytes FROM usage_budgets WHERE scope_key='global'`).Scan(&globalBudget); err == nil {
			hasGlobal = true
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if hasGlobal {
			var used int64
			if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(bytes_down),0)+COALESCE(sum(bytes_up),0) FROM usage_monthly_totals WHERE period_start=$1::date`, period).Scan(&used); err != nil {
				return err
			}
			if used >= globalBudget {
				node = ""
			}
		}
	}
	if node != "" {
		targetNodes = append(targetNodes, node)
	} else {
		rows, err := tx.Query(ctx, `SELECT id::text FROM nodes ORDER BY id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			targetNodes = append(targetNodes, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	for _, target := range targetNodes {
		if err := s.reconcileNodePolicy(ctx, tx, target, period, now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) reconcileNodePolicy(ctx context.Context, tx pgx.Tx, node string, period, _ time.Time) error {
	var global, local int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(bytes_down),0)+COALESCE(sum(bytes_up),0) FROM usage_monthly_totals WHERE period_start=$1::date`, period).Scan(&global); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(bytes_down),0)+COALESCE(sum(bytes_up),0) FROM usage_monthly_totals WHERE node_id=$1 AND period_start=$2::date`, node, period).Scan(&local); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT scope_key,COALESCE(node_id::text,''),monthly_bytes,action,widen_multiplier
		FROM usage_budgets WHERE scope_key='global' OR node_id=$1 ORDER BY scope_key FOR UPDATE`, node)
	if err != nil {
		return err
	}
	budgets := []Budget{}
	for rows.Next() {
		var budget Budget
		if err := rows.Scan(&budget.ScopeKey, &budget.NodeID, &budget.MonthlyBytes, &budget.Action, &budget.WidenMultiplier); err != nil {
			rows.Close()
			return err
		}
		budgets = append(budgets, budget)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	action, multiplier := "", 0
	for _, budget := range budgets {
		used := local
		if budget.ScopeKey == "global" {
			used = global
		}
		if err := recordBudgetThresholdEvents(ctx, tx, budget, used, period); err != nil {
			return err
		}
		if used < budget.MonthlyBytes {
			continue
		}
		if budget.Action == "pause" {
			action, multiplier = "pause", 0
		} else if action != "pause" && budget.WidenMultiplier > multiplier {
			action, multiplier = "widen", budget.WidenMultiplier
		}
	}
	effects, err := applyEffectiveSchedulePolicy(ctx, tx, node, period, action, multiplier)
	if err != nil {
		return err
	}
	return recordBudgetActionEvents(ctx, tx, budgets, global, local, period, effects)
}

func recordBudgetThresholdEvents(ctx context.Context, tx pgx.Tx, budget Budget, used int64, period time.Time) error {
	ratio := float64(used) / float64(budget.MonthlyBytes)
	if ratio < BudgetAlertRatio {
		return nil
	}
	if err := insertBudgetEvent(ctx, tx, budget, period, "budget_warning", used, ratio); err != nil {
		return err
	}
	if ratio < 1 {
		return nil
	}
	created, err := insertBudgetEventReturning(ctx, tx, budget, period, "budget_limit", used, ratio)
	if err != nil || !created {
		return err
	}
	return recordBudgetLimitAudit(ctx, tx, budget)
}

type policyEffects struct{ widened, paused bool }

func recordBudgetActionEvents(ctx context.Context, tx pgx.Tx, budgets []Budget, global, local int64, period time.Time, effects policyEffects) error {
	for _, budget := range budgets {
		used := local
		if budget.ScopeKey == "global" {
			used = global
		}
		if used < budget.MonthlyBytes {
			continue
		}
		if effects.widened {
			if err := insertBudgetEvent(ctx, tx, budget, period, "interval_widened", used, float64(used)/float64(budget.MonthlyBytes)); err != nil {
				return err
			}
		}
		if effects.paused {
			if err := insertBudgetEvent(ctx, tx, budget, period, "tests_paused", used, float64(used)/float64(budget.MonthlyBytes)); err != nil {
				return err
			}
		}
	}
	return nil
}

func insertBudgetEvent(ctx context.Context, tx pgx.Tx, b Budget, period time.Time, kind string, used int64, ratio float64) error {
	_, err := insertBudgetEventReturning(ctx, tx, b, period, kind, used, ratio)
	return err
}

func insertBudgetEventReturning(ctx context.Context, tx pgx.Tx, b Budget, period time.Time, kind string, used int64, ratio float64) (bool, error) {
	id, err := randomID()
	if err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO usage_budget_events(id,scope_key,node_id,period_start,event_type,used_bytes,budget_bytes,ratio)
 VALUES($1,$2,NULLIF($3,'')::uuid,$4,$5,$6,$7,$8) ON CONFLICT(scope_key,period_start,event_type) DO NOTHING`,
		id, b.ScopeKey, b.NodeID, period, kind, used, b.MonthlyBytes, ratio)
	return tag.RowsAffected() == 1, err
}

func applyEffectiveSchedulePolicy(ctx context.Context, tx pgx.Tx, node string, period time.Time, action string, multiplier int) (policyEffects, error) {
	rows, err := tx.Query(ctx, `SELECT engine,interval_minutes,enabled FROM engine_schedules WHERE node_id=$1 ORDER BY engine FOR UPDATE`, node)
	if err != nil {
		return policyEffects{}, err
	}
	type schedule struct {
		engine   string
		interval int
		enabled  bool
	}
	var schedules []schedule
	for rows.Next() {
		var item schedule
		if err := rows.Scan(&item.engine, &item.interval, &item.enabled); err != nil {
			rows.Close()
			return policyEffects{}, err
		}
		schedules = append(schedules, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return policyEffects{}, err
	}
	rows.Close()
	effects := policyEffects{}
	for _, item := range schedules {
		effect, err := applySchedulePolicy(ctx, tx, node, period, item, action, multiplier)
		if err != nil {
			return policyEffects{}, err
		}
		effects.widened = effects.widened || effect == "interval_widened"
		effects.paused = effects.paused || effect == "tests_paused"
	}
	_, err = tx.Exec(ctx, `DELETE FROM usage_budget_overrides override
		WHERE override.node_id=$1 AND NOT EXISTS (SELECT 1 FROM engine_schedules schedule WHERE schedule.node_id=override.node_id AND schedule.engine=override.engine)`, node)
	return effects, err
}

func applySchedulePolicy(ctx context.Context, tx pgx.Tx, node string, period time.Time, item struct {
	engine   string
	interval int
	enabled  bool
}, action string, multiplier int) (string, error) {
	originalInterval, originalEnabled, hasOverride, err := originalSchedule(ctx, tx, node, period, item)
	if err != nil {
		return "", err
	}
	targetInterval, targetEnabled := originalInterval, originalEnabled
	effect := ""
	switch action {
	case "pause":
		targetEnabled = false
		if originalEnabled {
			effect = "tests_paused"
		}
	case "widen":
		if originalInterval >= 1440 {
			targetEnabled = false
			if originalEnabled {
				effect = "tests_paused"
			}
		} else {
			targetInterval = min(originalInterval*multiplier, 1440)
			effect = "interval_widened"
		}
	}
	changed := targetInterval != item.interval || targetEnabled != item.enabled
	if action == "" {
		if hasOverride {
			if changed {
				if err := updateSchedule(ctx, tx, node, item.engine, targetInterval, targetEnabled); err != nil {
					return "", err
				}
			}
			if _, err := tx.Exec(ctx, `DELETE FROM usage_budget_overrides WHERE node_id=$1 AND engine=$2`, node, item.engine); err != nil {
				return "", err
			}
			if changed {
				return "", recordPolicyAudit(ctx, tx, node, "usage_budget.reconciled", "Restored the owner schedule after the budget policy changed")
			}
		}
		return "", nil
	}
	if err := saveScheduleOverride(ctx, tx, node, period, item, originalInterval, originalEnabled, targetInterval, targetEnabled); err != nil {
		return "", err
	}
	if !changed {
		return effect, nil
	}
	if err := updateSchedule(ctx, tx, node, item.engine, targetInterval, targetEnabled); err != nil {
		return "", err
	}
	return effect, schedulePolicyAudit(ctx, tx, node, targetEnabled)
}

func originalSchedule(ctx context.Context, tx pgx.Tx, node string, period time.Time, item struct {
	engine   string
	interval int
	enabled  bool
}) (int, bool, bool, error) {
	originalInterval, originalEnabled := item.interval, item.enabled
	appliedInterval, appliedEnabled := item.interval, item.enabled
	var oldPeriod time.Time
	err := tx.QueryRow(ctx, `SELECT period_start,original_interval_minutes,original_enabled,applied_interval_minutes,applied_enabled
		FROM usage_budget_overrides WHERE node_id=$1 AND engine=$2 FOR UPDATE`, node, item.engine).Scan(&oldPeriod, &originalInterval, &originalEnabled, &appliedInterval, &appliedEnabled)
	hasOverride := err == nil && oldPeriod.Equal(period)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, false, false, err
	}
	if hasOverride {
		if item.interval != appliedInterval {
			originalInterval = item.interval
		}
		if item.enabled != appliedEnabled {
			originalEnabled = item.enabled
		}
	} else {
		originalInterval, originalEnabled = item.interval, item.enabled
	}
	return originalInterval, originalEnabled, hasOverride, nil
}

func saveScheduleOverride(ctx context.Context, tx pgx.Tx, node string, period time.Time, item struct {
	engine   string
	interval int
	enabled  bool
}, originalInterval int, originalEnabled bool, targetInterval int, targetEnabled bool) error {
	_, err := tx.Exec(ctx, `INSERT INTO usage_budget_overrides(node_id,engine,period_start,original_interval_minutes,original_enabled,applied_interval_minutes,applied_enabled)
		VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(node_id,engine,period_start) DO UPDATE SET
		original_interval_minutes=EXCLUDED.original_interval_minutes,original_enabled=EXCLUDED.original_enabled,
		applied_interval_minutes=EXCLUDED.applied_interval_minutes,applied_enabled=EXCLUDED.applied_enabled`,
		node, item.engine, period, originalInterval, originalEnabled, targetInterval, targetEnabled)
	return err
}

func schedulePolicyAudit(ctx context.Context, tx pgx.Tx, node string, enabled bool) error {
	if enabled {
		return recordPolicyAudit(ctx, tx, node, "usage_budget.interval_widened", "Reconciled the interval with the active monthly bandwidth budgets")
	}
	return recordPolicyAudit(ctx, tx, node, "usage_budget.tests_paused", "Paused tests under the active monthly bandwidth budgets")
}

func updateSchedule(ctx context.Context, tx pgx.Tx, node, engine string, interval int, enabled bool) error {
	_, err := tx.Exec(ctx, `UPDATE engine_schedules SET interval_minutes=$3,offset_minutes=offset_minutes % $3,enabled=$4,revision=revision+1
		WHERE node_id=$1 AND engine=$2`, node, engine, interval, enabled)
	return err
}

// ReconcileScheduleEdit reapplies current budgets after an owner schedule edit in the same transaction.
func ReconcileScheduleEdit(ctx context.Context, tx pgx.Tx, node string, now time.Time) error {
	if err := LockBudgetEvaluation(ctx, tx); err != nil {
		return err
	}
	period := MonthStart(now)
	if err := restoreExpiredOverrides(ctx, tx, period); err != nil {
		return err
	}
	return (&Store{}).reconcileNodePolicy(ctx, tx, node, period, now)
}

func restoreOverride(ctx context.Context, tx pgx.Tx, node, engine string, period time.Time, originalInterval int, originalEnabled bool) (bool, error) {
	var currentInterval, appliedInterval int
	var currentEnabled, appliedEnabled bool
	err := tx.QueryRow(ctx, `SELECT interval_minutes,enabled FROM engine_schedules WHERE node_id=$1 AND engine=$2`, node, engine).Scan(&currentInterval, &currentEnabled)
	if errors.Is(err, pgx.ErrNoRows) {
		_, deleteErr := tx.Exec(ctx, `DELETE FROM usage_budget_overrides WHERE node_id=$1 AND engine=$2 AND period_start=$3`, node, engine, period)
		return false, deleteErr
	}
	if err != nil {
		return false, err
	}
	if err := tx.QueryRow(ctx, `SELECT applied_interval_minutes,applied_enabled FROM usage_budget_overrides WHERE node_id=$1 AND engine=$2 AND period_start=$3`, node, engine, period).Scan(&appliedInterval, &appliedEnabled); err != nil {
		return false, err
	}
	if currentInterval != appliedInterval {
		originalInterval = currentInterval
	}
	if currentEnabled != appliedEnabled {
		originalEnabled = currentEnabled
	}
	changed := originalInterval != currentInterval || originalEnabled != currentEnabled
	if changed {
		if err := updateSchedule(ctx, tx, node, engine, originalInterval, originalEnabled); err != nil {
			return false, err
		}
	}
	_, err = tx.Exec(ctx, `DELETE FROM usage_budget_overrides WHERE node_id=$1 AND engine=$2 AND period_start=$3`, node, engine, period)
	return changed, err
}

func recordPolicyAudit(ctx context.Context, tx pgx.Tx, node, action, summary string) error {
	id, err := randomID()
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_log(id,actor,action,target_type,target_id,summary)
 VALUES($1,'system:usage-budget',$2,'node',$3,$4)`, id, action, node, summary)
	return err
}

func recordBudgetLimitAudit(ctx context.Context, tx pgx.Tx, b Budget) error {
	id, err := randomID()
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_log(id,actor,action,target_type,target_id,summary)
 VALUES($1,'system:usage-budget','usage_budget.limit','usage_budget',$2,'Monthly bandwidth budget reached; applied the configured schedule policy')`, id, b.ScopeKey)
	return err
}

var _ agentapi.ResultHook = (*Store)(nil)
