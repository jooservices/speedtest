package alerts

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jooservices/speedtest/internal/center/agentapi"
	"github.com/jooservices/speedtest/internal/center/catalog"
	"github.com/jooservices/speedtest/internal/shared/errtrack"
)

const signalBatchSize = 100

type signal struct {
	ResultID string
	NodeID   string
	TS       time.Time
	FromIP   string
	ToIP     string
	City     string
}

type ruleForEvaluation struct {
	Rule
	ExpectedDown float64
	ExpectedUp   float64
}

// AfterResultStored evaluates measurement rules and consumes a matching IP
// change signal in the result's transaction. Duplicate result replays never
// reach this hook because agentapi.Postgres rejects them before calling hooks.
func (s *Store) AfterResultStored(ctx context.Context, tx pgx.Tx, node string, result agentapi.Result) error {
	// The result was validated at ingestion, so its timestamp parses; the stored value is only a fallback.
	ts, err := time.Parse(time.RFC3339Nano, result.TS)
	if err != nil {
		if err := tx.QueryRow(ctx, `SELECT ts FROM results WHERE id=$1 AND node_id=$2`, result.ID, node).Scan(&ts); err != nil {
			return err
		}
	}
	var expectedDown, expectedUp float64
	if err := tx.QueryRow(ctx, `SELECT expected_down_mbps,expected_up_mbps FROM nodes WHERE id=$1`, node).Scan(&expectedDown, &expectedUp); err != nil {
		return err
	}
	if err := s.evaluateResult(ctx, tx, node, result, ts); err != nil {
		return err
	}
	if err := s.emitTestOutcome(ctx, tx, node, result, ts, expectedDown, expectedUp); err != nil {
		return err
	}
	if result.IP != nil {
		if err := s.consumeResultSignal(ctx, tx, result.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) evaluateResult(ctx context.Context, tx pgx.Tx, node string, result agentapi.Result, ts time.Time) error {
	rows, err := tx.Query(ctx, `SELECT r.id::text,r.name,r.metric,r.op,r.value,r.for_tests,r.node_ids IS NULL,
		COALESCE(r.node_ids::text[],ARRAY[]::text[]),r.severity,r.channels,r.enabled,r.revision,
		n.expected_down_mbps,n.expected_up_mbps
		FROM alert_rules r JOIN nodes n ON n.id=$1
		WHERE r.enabled AND COALESCE(r.engine,n.primary_engine)=$2 AND r.metric NOT IN ('ip_changed','offline')
		AND (r.node_ids IS NULL OR $1=ANY(r.node_ids)) ORDER BY r.id`, node, measurementEngine(result.Engine))
	if err != nil {
		return err
	}
	var rules []ruleForEvaluation
	for rows.Next() {
		var current ruleForEvaluation
		var all bool
		if err := rows.Scan(&current.ID, &current.Name, &current.Metric, &current.Op, &current.Value,
			&current.ForTests, &all, &current.NodeIDs, &current.Severity, &current.Channels,
			&current.Enabled, &current.Revision, &current.ExpectedDown, &current.ExpectedUp); err != nil {
			rows.Close()
			return err
		}
		current.AllNodes = all
		rules = append(rules, current)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, rule := range rules {
		hit := metricMatches(rule.Rule, result, rule.ExpectedDown, rule.ExpectedUp)
		if err := s.advanceRule(ctx, tx, rule.Rule, node, ts, result.ID, hit); err != nil {
			return err
		}
	}
	return nil
}

func metricMatches(rule Rule, result agentapi.Result, expectedDown, expectedUp float64) bool {
	if rule.Metric == "failed" {
		return result.Status == "failed"
	}
	if result.Status == "failed" {
		return false
	}
	var value float64
	switch rule.Metric {
	case "down_pct":
		if result.Down == nil || expectedDown <= 0 {
			return false
		}
		value = *result.Down / expectedDown * 100
	case "up_pct":
		if result.Up == nil || expectedUp <= 0 {
			return false
		}
		value = *result.Up / expectedUp * 100
	case "ping":
		if result.Ping == nil {
			return false
		}
		value = *result.Ping
	case "jitter":
		if result.Jitter == nil {
			return false
		}
		value = *result.Jitter
	case "loss":
		if result.Loss == nil {
			return false
		}
		value = *result.Loss
	default:
		return false
	}
	return compare(value, rule.Op, rule.Value)
}

func (s *Store) advanceRule(ctx context.Context, tx pgx.Tx, rule Rule, node string, ts time.Time, resultID string, hit bool) error {
	if _, err := tx.Exec(ctx, `INSERT INTO alert_state(rule_id,node_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, rule.ID, node); err != nil {
		return err
	}
	var count int
	var active bool
	var activeEventID *string
	if err := tx.QueryRow(ctx, `SELECT consecutive_count,active,active_event_id::text FROM alert_state WHERE rule_id=$1 AND node_id=$2 FOR UPDATE`, rule.ID, node).
		Scan(&count, &active, &activeEventID); err != nil {
		return err
	}
	change := advanceStreak(count, active, hit, rule.ForTests)
	eventID := activeEventID
	if change.opened {
		createdID, err := s.insertEvent(ctx, tx, rule, node, ts, resultID, ruleText(rule))
		if err != nil {
			return err
		}
		eventID = &createdID
	}
	if change.resolved && eventID != nil {
		var openedAt time.Time
		if err := tx.QueryRow(ctx, `SELECT ts FROM alert_events WHERE id=$1`, *eventID).Scan(&openedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE alert_events SET resolved_at=GREATEST(ts,$2) WHERE id=$1 AND resolved_at IS NULL`, *eventID, ts); err != nil {
			return err
		}
		if err := s.emitRecovery(ctx, tx, rule, node, openedAt, ts, resultID); err != nil {
			return err
		}
		eventID = nil
	}
	if !change.active {
		eventID = nil
	}
	_, err := tx.Exec(ctx, `UPDATE alert_state SET consecutive_count=$3,active=$4,active_event_id=$5,last_evaluated_at=$6
		WHERE rule_id=$1 AND node_id=$2`, rule.ID, node, change.count, change.active, eventID, ts)
	return err
}

func (s *Store) insertEvent(ctx context.Context, tx pgx.Tx, rule Rule, node string, ts time.Time, resultID, detail string) (string, error) {
	id, err := s.newUUID()
	if err != nil {
		return "", err
	}
	eventKey := ""
	if cat, ok := catalog.EventForAlertMetric(rule.Metric); ok {
		eventKey = cat.Key
	}
	payload, err := s.rulePayload(ctx, tx, eventKey, rule, node, resultID, detail)
	if err != nil {
		return "", err
	}
	raw := PayloadJSON(payload)
	var created string
	if resultID == "" {
		err = tx.QueryRow(ctx, `INSERT INTO alert_events(id,rule_id,node_id,severity,ts,title,detail,event_key,payload)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id::text`, id, rule.ID, node, rule.Severity, ts, rule.Name, detail, eventKey, raw).Scan(&created)
	} else {
		err = tx.QueryRow(ctx, `INSERT INTO alert_events(id,rule_id,node_id,severity,ts,title,detail,trigger_result_id,event_key,payload)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			ON CONFLICT (rule_id,node_id,trigger_result_id) WHERE trigger_result_id IS NOT NULL DO NOTHING
			RETURNING id::text`, id, rule.ID, node, rule.Severity, ts, rule.Name, detail, resultID, eventKey, raw).Scan(&created)
		if errors.Is(err, pgx.ErrNoRows) {
			if err = tx.QueryRow(ctx, `SELECT id::text FROM alert_events WHERE rule_id=$1 AND node_id=$2 AND trigger_result_id=$3`, rule.ID, node, resultID).Scan(&created); err != nil {
				return "", err
			}
			return created, nil
		}
	}
	if err != nil {
		return "", err
	}
	if err = s.enqueueNotification(ctx, tx, created, rule.Channels); err != nil {
		return "", err
	}
	return created, nil
}

func (s *Store) rulePayload(ctx context.Context, tx pgx.Tx, eventKey string, rule Rule, node, resultID, detail string) (map[string]string, error) {
	var nodeName string
	var expectedDown, expectedUp float64
	if err := tx.QueryRow(ctx, `SELECT name,expected_down_mbps,expected_up_mbps FROM nodes WHERE id=$1`, node).Scan(&nodeName, &expectedDown, &expectedUp); err != nil {
		return nil, err
	}
	result := agentapi.Result{}
	if resultID != "" {
		var loadErr error
		result, loadErr = loadResult(ctx, tx, resultID)
		if loadErr != nil {
			return nil, loadErr
		}
	}
	limits, err := s.qualityLimits(ctx, tx, node)
	if err != nil {
		return nil, err
	}
	value, expected, pct := metricValue(rule, result, expectedDown, expectedUp)
	extra := map[string]string{"value": value, "expected": expected, "pct": pct, "threshold": formatNum(rule.Value), "count": strconv.Itoa(rule.ForTests)}
	_ = detail
	url, err := s.centerURL(ctx, tx)
	if err != nil {
		return nil, err
	}
	return BuildPayload(eventKey, nodeName, url, rule.Severity, result, expectedDown, expectedUp, limits, extra), nil
}

func (s *Store) enqueueNotification(ctx context.Context, tx pgx.Tx, eventID string, selected []string) error {
	_, err := tx.Exec(ctx, `INSERT INTO alert_notification_outbox(event_id,channels) VALUES ($1,$2) ON CONFLICT(event_id) DO NOTHING`, eventID, selected)
	return err
}

func (s *Store) consumeResultSignal(ctx context.Context, tx pgx.Tx, resultID string) error {
	var item signal
	err := tx.QueryRow(ctx, `SELECT s.result_id::text,s.node_id::text,s.ts,host(s.from_ip),host(s.to_ip),COALESCE(a.geo->>'city','')
		FROM ip_change_signals s LEFT JOIN ip_addresses a ON a.ip=s.to_ip
		WHERE s.result_id=$1 AND s.consumed_at IS NULL`, resultID).
		Scan(&item.ResultID, &item.NodeID, &item.TS, &item.FromIP, &item.ToIP, &item.City)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = s.processSignal(ctx, tx, item); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE ip_change_signals SET consumed_at=clock_timestamp() WHERE result_id=$1 AND consumed_at IS NULL`, resultID)
	return err
}

func (s *Store) processSignal(ctx context.Context, tx pgx.Tx, item signal) error {
	rows, err := tx.Query(ctx, `SELECT id::text,name,metric,op,value,for_tests,node_ids IS NULL,
		COALESCE(node_ids::text[],ARRAY[]::text[]),severity,channels,enabled,revision
		FROM alert_rules WHERE enabled AND metric='ip_changed' AND COALESCE(engine,(SELECT primary_engine FROM nodes WHERE id=$1))=(SELECT engine FROM results WHERE id=$2)
		AND (node_ids IS NULL OR $1=ANY(node_ids)) ORDER BY id`, item.NodeID, item.ResultID)
	if err != nil {
		return err
	}
	var matched []Rule
	for rows.Next() {
		var r Rule
		var all bool
		if err := rows.Scan(&r.ID, &r.Name, &r.Metric, &r.Op, &r.Value, &r.ForTests, &all, &r.NodeIDs, &r.Severity, &r.Channels, &r.Enabled, &r.Revision); err != nil {
			rows.Close()
			return err
		}
		r.AllNodes = all
		matched = append(matched, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	detail := "Now " + item.ToIP
	if item.City != "" {
		detail += " (" + item.City + ")"
	}
	for _, rule := range matched {
		id, err := s.insertEvent(ctx, tx, rule, item.NodeID, item.TS, item.ResultID, detail)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE alert_events SET payload = payload || $2::jsonb WHERE id=$1`, id,
			PayloadJSON(map[string]string{"ip": item.ToIP, "old_ip": item.FromIP, "city": item.City})); err != nil {
			return err
		}
	}
	return nil
}

// DrainSignals consumes at most one bounded batch of durable IP changes. It is
// safe to run alongside ingestion because workers lock rows with SKIP LOCKED.
func (s *Store) DrainSignals(ctx context.Context) (int, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	rows, err := tx.Query(ctx, `SELECT s.result_id::text,s.node_id::text,s.ts,host(s.from_ip),host(s.to_ip),COALESCE(a.geo->>'city','')
		FROM ip_change_signals s LEFT JOIN ip_addresses a ON a.ip=s.to_ip
		WHERE s.consumed_at IS NULL ORDER BY s.ts,s.result_id LIMIT $1 FOR UPDATE OF s SKIP LOCKED`, signalBatchSize)
	if err != nil {
		return 0, err
	}
	items := make([]signal, 0, signalBatchSize)
	for rows.Next() {
		var item signal
		if err := rows.Scan(&item.ResultID, &item.NodeID, &item.TS, &item.FromIP, &item.ToIP, &item.City); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, item := range items {
		if err := s.processSignal(ctx, tx, item); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `UPDATE ip_change_signals SET consumed_at=clock_timestamp() WHERE result_id=$1 AND consumed_at IS NULL`, item.ResultID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(items), nil
}

// EvaluateOffline opens or resolves offline rule events from schedule heartbeats.
func (s *Store) EvaluateOffline(ctx context.Context) error {
	now := s.now()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	rows, err := tx.Query(ctx, `SELECT r.id::text,r.name,r.metric,r.op,r.value,r.for_tests,r.node_ids IS NULL,
		COALESCE(r.node_ids::text[],ARRAY[]::text[]),r.severity,r.channels,r.enabled,r.revision,
		n.id::text,n.enabled,s.enabled,s.interval_minutes,COALESCE(n.last_seen_at,n.created_at)
		FROM alert_rules r JOIN nodes n ON r.node_ids IS NULL OR n.id=ANY(r.node_ids)
		JOIN engine_schedules s ON s.node_id=n.id AND s.engine=COALESCE(r.engine,n.primary_engine) WHERE r.enabled AND r.metric='offline'
		ORDER BY r.id,n.id`)
	if err != nil {
		return err
	}
	type offlineState struct {
		rule        Rule
		node        string
		nodeEnabled bool
		schedOn     bool
		interval    int
		anchor      time.Time
	}
	items := make([]offlineState, 0)
	for rows.Next() {
		var item offlineState
		var all bool
		if err := rows.Scan(&item.rule.ID, &item.rule.Name, &item.rule.Metric, &item.rule.Op, &item.rule.Value,
			&item.rule.ForTests, &all, &item.rule.NodeIDs, &item.rule.Severity, &item.rule.Channels,
			&item.rule.Enabled, &item.rule.Revision, &item.node, &item.nodeEnabled, &item.schedOn, &item.interval, &item.anchor); err != nil {
			rows.Close()
			return err
		}
		item.rule.AllNodes = all
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, item := range items {
		threshold := item.anchor.Add(time.Duration(item.interval*3) * time.Minute)
		hit := item.nodeEnabled && item.schedOn && !now.Before(threshold)
		if err := s.advanceOffline(ctx, tx, item.rule, item.node, threshold, now, hit); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) advanceOffline(ctx context.Context, tx pgx.Tx, rule Rule, node string, threshold, now time.Time, hit bool) error {
	if _, err := tx.Exec(ctx, `INSERT INTO alert_state(rule_id,node_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, rule.ID, node); err != nil {
		return err
	}
	var count int
	var active bool
	var eventID *string
	if err := tx.QueryRow(ctx, `SELECT consecutive_count,active,active_event_id::text FROM alert_state WHERE rule_id=$1 AND node_id=$2 FOR UPDATE`, rule.ID, node).
		Scan(&count, &active, &eventID); err != nil {
		return err
	}
	change := advanceStreak(count, active, hit, 1)
	if change.opened {
		created, err := s.insertEvent(ctx, tx, rule, node, threshold, "", "No heartbeat for three scheduled intervals.")
		if err != nil {
			return err
		}
		eventID = &created
	}
	if change.resolved && eventID != nil {
		var openedAt time.Time
		if err := tx.QueryRow(ctx, `SELECT ts FROM alert_events WHERE id=$1`, *eventID).Scan(&openedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE alert_events SET resolved_at=GREATEST(ts,$2) WHERE id=$1 AND resolved_at IS NULL`, *eventID, now); err != nil {
			return err
		}
		if err := s.emitRecovery(ctx, tx, rule, node, openedAt, now, ""); err != nil {
			return err
		}
		eventID = nil
	}
	if !change.active {
		eventID = nil
	}
	_, err := tx.Exec(ctx, `UPDATE alert_state SET consecutive_count=$3,active=$4,active_event_id=$5,last_evaluated_at=$6
		WHERE rule_id=$1 AND node_id=$2`, rule.ID, node, change.count, change.active, eventID, now)
	return err
}

// Loop drains durable IP signals immediately and then checks offline state on
// a short interval. Errors leave uncommitted work available for retry.
func (s *Store) Loop(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		ok := true
		for {
			count, err := s.DrainSignals(ctx)
			if err != nil {
				ok = false
				s.logger().ErrorContext(ctx, "alert IP signal drain failed", "error", err)
				errtrack.CaptureError(err, map[string]string{"loop": "alerts", "step": "ip-signals"})
				break
			}
			if count < signalBatchSize {
				break
			}
		}
		if err := s.EvaluateOffline(ctx); err != nil {
			ok = false
			s.logger().ErrorContext(ctx, "alert offline check failed", "error", err)
			errtrack.CaptureError(err, map[string]string{"loop": "alerts", "step": "offline-check"})
		}
		if s.ObserveRun != nil {
			s.ObserveRun(ok)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Store) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

func measurementEngine(name string) string {
	if name == "" {
		return "ookla"
	}
	return name
}
