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
	"github.com/jooservices/speedtest/internal/shared/failureclass"
)

const signalBatchSize = 100

type ruleEvaluationBatchKey struct{}

type ruleEvaluationBatch struct {
	lastEvaluated map[string]time.Time
}

func (b *ruleEvaluationBatch) accept(ruleID string, ts time.Time) bool {
	if last, ok := b.lastEvaluated[ruleID]; ok && !ts.After(last) {
		return false
	}
	if b.lastEvaluated == nil {
		b.lastEvaluated = make(map[string]time.Time)
	}
	b.lastEvaluated[ruleID] = ts
	return true
}

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
	if result.Status == "failed" && result.FailureClass == failureclass.RateLimited {
		return nil
	}
	// The result was validated at ingestion, so its timestamp parses; the stored value is only a fallback.
	ts, err := time.Parse(time.RFC3339Nano, result.TS)
	if err != nil {
		if err := tx.QueryRow(ctx, `SELECT ts FROM results WHERE id=$1 AND node_id=$2`, result.ID, node).Scan(&ts); err != nil {
			return err
		}
	}
	if err := s.evaluateResult(ctx, tx, node, result, ts); err != nil {
		return err
	}
	if err := s.emitTestOutcome(ctx, tx, node, result, ts); err != nil {
		return err
	}
	if err := s.emitObservedEvents(ctx, tx, node, result, ts); err != nil {
		return err
	}
	if result.Status == "ok" || result.Status == "degraded" {
		if err := s.emitBandwidthEvents(ctx, tx, node, result, ts); err != nil {
			return err
		}
	}
	if result.IP != nil {
		if err := s.consumeResultSignal(ctx, tx, result.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) evaluateResult(ctx context.Context, tx pgx.Tx, node string, result agentapi.Result, ts time.Time) error {
	rows, err := tx.Query(ctx, `SELECT r.id::text,r.name,r.metric,r.op,
		CASE WHEN r.node_ids IS NULL AND r.engine IS NULL AND es.updated_at>r.updated_at
			THEN COALESCE((es.thresholds->>CASE WHEN r.metric='failed' THEN 'for_tests' ELSE r.metric END)::float8,r.value)
			ELSE r.value END,
		CASE WHEN r.node_ids IS NULL AND r.engine IS NULL AND es.updated_at>r.updated_at
			THEN COALESCE((es.thresholds->>'for_tests')::integer,r.for_tests) ELSE r.for_tests END,r.node_ids IS NULL,
		COALESCE(r.node_ids::text[],ARRAY[]::text[]),r.severity,r.channels,r.enabled,r.revision,
		n.expected_down_mbps,n.expected_up_mbps
		FROM alert_rules r JOIN nodes n ON n.id=$1
		LEFT JOIN notification_event_settings es ON es.event_key=CASE r.metric
			WHEN 'failed' THEN 'test.failed' WHEN 'down_pct' THEN 'quality.download_low'
			WHEN 'up_pct' THEN 'quality.upload_low' WHEN 'ping' THEN 'quality.ping_high'
			WHEN 'jitter' THEN 'quality.jitter_high' WHEN 'loss' THEN 'quality.loss_high' ELSE '' END
		WHERE r.enabled AND COALESCE(r.engine,n.primary_engine)=$2 AND r.metric NOT IN ('ip_changed','offline')
		AND (r.node_ids IS NULL OR $1=ANY(r.node_ids))
		ORDER BY (r.node_ids IS NULL),(r.engine IS NULL),r.id`, node, measurementEngine(result.Engine))
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
	batch, _ := ctx.Value(ruleEvaluationBatchKey{}).(*ruleEvaluationBatch)
	for _, rule := range rules {
		if batch != nil && !batch.accept(rule.ID, ts) {
			continue
		}
		hit, observed := metricObservation(rule.Rule, result, rule.ExpectedDown, rule.ExpectedUp)
		if err := s.advanceRule(ctx, tx, rule.Rule, node, ts, result.ID, hit, observed); err != nil {
			return err
		}
	}
	return nil
}

func metricMatches(rule Rule, result agentapi.Result, expectedDown, expectedUp float64) bool {
	hit, _ := metricObservation(rule, result, expectedDown, expectedUp)
	return hit
}

func metricObservation(rule Rule, result agentapi.Result, expectedDown, expectedUp float64) (hit, observed bool) {
	if rule.Metric == "failed" {
		if result.Status == "failed" && result.FailureClass == "rate_limited" {
			return false, false
		}
		switch result.Status {
		case "failed":
			return true, true
		case "ok":
			return false, true
		default:
			return false, false
		}
	}
	if result.Status == "failed" {
		return false, false
	}
	if result.Status != "ok" && result.Status != "degraded" {
		return false, false
	}
	var value float64
	switch rule.Metric {
	case "down_pct":
		if result.Down == nil || expectedDown <= 0 {
			return false, false
		}
		value = *result.Down / expectedDown * 100
	case "up_pct":
		if result.Up == nil || expectedUp <= 0 {
			return false, false
		}
		value = *result.Up / expectedUp * 100
	case "ping":
		if result.Ping == nil {
			return false, false
		}
		value = *result.Ping
	case "jitter":
		if result.Jitter == nil {
			return false, false
		}
		value = *result.Jitter
	case "loss":
		if result.Loss == nil {
			return false, false
		}
		value = *result.Loss
	default:
		return false, false
	}
	return compare(value, rule.Op, rule.Value), true
}

type ruleState struct {
	count           int
	active          bool
	eventID         *string
	streakStartedAt *time.Time
	lastEvaluatedAt *time.Time
	lastNotifiedAt  *time.Time
}

func (s *Store) advanceRule(ctx context.Context, tx pgx.Tx, rule Rule, node string, ts time.Time, resultID string, hit, observed bool) error {
	if !observed {
		return s.saveLastEvaluated(ctx, tx, rule.ID, node, ts)
	}
	state, err := s.lockRuleState(ctx, tx, rule.ID, node)
	if err != nil {
		return err
	}
	if state.lastEvaluatedAt != nil && !ts.After(*state.lastEvaluatedAt) {
		return nil
	}
	change := advanceStreak(state.count, state.active, hit, rule.ForTests)
	if hit && state.count == 0 {
		started := ts
		state.streakStartedAt = &started
	}
	previousEventID, previousNotifiedAt := state.eventID, state.lastNotifiedAt
	state.eventID, state.lastNotifiedAt, err = s.emitRuleTransition(ctx, tx, rule, node, ts, resultID, state, change, hit)
	if err != nil {
		return err
	}
	if !change.active {
		state.eventID = nil
	}
	if !hit {
		state.streakStartedAt = nil
	}
	if change.count == state.count && change.active == state.active &&
		previousEventID == state.eventID && previousNotifiedAt == state.lastNotifiedAt {
		return s.saveLastEvaluated(ctx, tx, rule.ID, node, ts)
	}
	return s.saveRuleState(ctx, tx, rule.ID, node, ts, change, state)
}

func (s *Store) lockRuleState(ctx context.Context, tx pgx.Tx, ruleID, node string) (ruleState, error) {
	var state ruleState
	if _, err := tx.Exec(ctx, `INSERT INTO alert_state(rule_id,node_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, ruleID, node); err != nil {
		return state, err
	}
	err := tx.QueryRow(ctx, `SELECT consecutive_count,active,active_event_id::text,streak_started_at,last_evaluated_at,last_notified_at
		FROM alert_state WHERE rule_id=$1 AND node_id=$2 FOR UPDATE`, ruleID, node).
		Scan(&state.count, &state.active, &state.eventID, &state.streakStartedAt, &state.lastEvaluatedAt, &state.lastNotifiedAt)
	return state, err
}

func (s *Store) saveRuleState(ctx context.Context, tx pgx.Tx, ruleID, node string, ts time.Time, change streakTransition, state ruleState) error {
	_, err := tx.Exec(ctx, `UPDATE alert_state SET consecutive_count=$3,active=$4,active_event_id=$5,last_evaluated_at=$6,streak_started_at=$7,last_notified_at=$8
		WHERE rule_id=$1 AND node_id=$2`, ruleID, node, change.count, change.active, state.eventID, ts, state.streakStartedAt, state.lastNotifiedAt)
	return err
}

func (s *Store) saveLastEvaluated(ctx context.Context, tx pgx.Tx, ruleID, node string, ts time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO alert_state(rule_id,node_id,last_evaluated_at) VALUES ($1,$2,$3)
		ON CONFLICT(rule_id,node_id) DO UPDATE SET last_evaluated_at=GREATEST(alert_state.last_evaluated_at,EXCLUDED.last_evaluated_at)`, ruleID, node, ts)
	return err
}

func (s *Store) emitRuleTransition(ctx context.Context, tx pgx.Tx, rule Rule, node string, ts time.Time, resultID string, state ruleState, change streakTransition, hit bool) (*string, *time.Time, error) {
	if !change.opened && !change.resolved && !change.recovered && !state.active {
		return state.eventID, state.lastNotifiedAt, nil
	}
	config := effectiveEventSettings{Enabled: true, CooldownSeconds: 0}
	if cat, ok := catalog.EventForAlertMetric(rule.Metric); ok {
		var err error
		config, err = ruleEventSettings(ctx, tx, cat.Key, node, rule.Severity, rule.Channels)
		if err != nil {
			return state.eventID, state.lastNotifiedAt, err
		}
	}
	eventID, lastNotified := state.eventID, state.lastNotifiedAt
	if change.opened {
		allowDelivery := shouldOpenNotification(ts, lastNotified, config.CooldownSeconds)
		created, sent, err := s.insertEvent(ctx, tx, rule, node, ts, resultID, ruleText(rule), state.streakStartedAt, allowDelivery)
		if err != nil {
			return eventID, lastNotified, err
		}
		eventID = &created
		if sent {
			lastNotified = timePointer(ts)
		}
	} else if state.active && hit && config.RepeatSeconds > 0 && shouldRepeatNotification(ts, lastNotified, state.streakStartedAt, config.RepeatSeconds) {
		allowDelivery := shouldOpenNotification(ts, lastNotified, config.CooldownSeconds)
		created, sent, err := s.insertEvent(ctx, tx, rule, node, ts, resultID, ruleText(rule), state.streakStartedAt, allowDelivery)
		if err != nil {
			return eventID, lastNotified, err
		}
		if _, err := tx.Exec(ctx, `UPDATE alert_events SET resolved_at=GREATEST(ts,$2),acked=true WHERE id=$1 AND resolved_at IS NULL`, created, ts); err != nil {
			return eventID, lastNotified, err
		}
		if sent {
			lastNotified = timePointer(ts)
		}
	}
	if change.resolved && eventID != nil {
		if _, err := tx.Exec(ctx, `UPDATE alert_events SET resolved_at=GREATEST(ts,$2) WHERE id=$1 AND resolved_at IS NULL`, *eventID, ts); err != nil {
			return eventID, lastNotified, err
		}
		onset := ts
		if state.streakStartedAt != nil {
			onset = *state.streakStartedAt
		} else if err := tx.QueryRow(ctx, `SELECT ts FROM alert_events WHERE id=$1`, *eventID).Scan(&onset); err != nil {
			return eventID, lastNotified, err
		}
		if err := s.emitRecovery(ctx, tx, rule, node, onset, ts, resultID); err != nil {
			return eventID, lastNotified, err
		}
		eventID = nil
	} else if change.recovered && state.streakStartedAt != nil {
		if err := s.emitRecovery(ctx, tx, rule, node, *state.streakStartedAt, ts, resultID); err != nil {
			return eventID, lastNotified, err
		}
	}
	return eventID, lastNotified, nil
}

func timePointer(value time.Time) *time.Time { return &value }

func shouldOpenNotification(ts time.Time, last *time.Time, cooldownSeconds int) bool {
	return last == nil || cooldownSeconds <= 0 || ts.Sub(*last) >= time.Duration(cooldownSeconds)*time.Second
}

// shouldRepeatNotification: a repeat is due repeatSeconds after the last delivery, or, when nothing was delivered yet (the breach began
// during maintenance, with the event disabled or inside the cooldown), repeatSeconds after the breach began.
func shouldRepeatNotification(ts time.Time, last, onset *time.Time, repeatSeconds int) bool {
	reference := last
	if reference == nil {
		reference = onset
	}
	return reference != nil && ts.Sub(*reference) >= time.Duration(repeatSeconds)*time.Second
}

func (s *Store) insertEvent(ctx context.Context, tx pgx.Tx, rule Rule, node string, ts time.Time, resultID, detail string, onset *time.Time, allowDelivery bool) (string, bool, error) {
	id, err := s.newUUID()
	if err != nil {
		return "", false, err
	}
	eventKey := ""
	if cat, ok := catalog.EventForAlertMetric(rule.Metric); ok {
		eventKey = cat.Key
	}
	channels := rule.Channels
	config := effectiveEventSettings{Enabled: true, Severity: rule.Severity, Channels: channels, MaintenanceTimezone: "UTC"}
	if eventKey != "" {
		resolved, configErr := resolveEventSettings(ctx, tx, eventKey, node, rule.Severity, rule.Channels)
		if configErr != nil {
			return "", false, configErr
		}
		config = resolved
		rule.Severity, channels = config.Severity, config.Channels
	}
	payload, err := s.rulePayload(ctx, tx, eventKey, rule, node, resultID, detail, onset)
	if err != nil {
		return "", false, err
	}
	raw := PayloadJSON(payload)
	var created string
	if resultID == "" {
		err = tx.QueryRow(ctx, `INSERT INTO alert_events(id,rule_id,node_id,severity,ts,title,detail,event_key,payload,isp_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,
				(SELECT r.isp_id FROM results r WHERE r.node_id=$3 AND r.ts<=$5 AND r.isp_id IS NOT NULL ORDER BY r.ts DESC,r.id DESC LIMIT 1))
			RETURNING id::text`, id, rule.ID, node, rule.Severity, ts, rule.Name, detail, eventKey, raw).Scan(&created)
	} else {
		err = tx.QueryRow(ctx, `INSERT INTO alert_events(id,rule_id,node_id,severity,ts,title,detail,trigger_result_id,event_key,payload,isp_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,(SELECT r.isp_id FROM results r WHERE r.id=$8))
			ON CONFLICT (rule_id,node_id,trigger_result_id) WHERE trigger_result_id IS NOT NULL DO NOTHING
			RETURNING id::text`, id, rule.ID, node, rule.Severity, ts, rule.Name, detail, resultID, eventKey, raw).Scan(&created)
		if errors.Is(err, pgx.ErrNoRows) {
			if err = tx.QueryRow(ctx, `SELECT id::text FROM alert_events WHERE rule_id=$1 AND node_id=$2 AND trigger_result_id=$3`, rule.ID, node, resultID).Scan(&created); err != nil {
				return "", false, err
			}
			return created, false, nil
		}
	}
	if err != nil {
		return "", false, err
	}
	send := config.Enabled && allowDelivery && eventDeliveryAllowed(config, rule.Severity, payload, ts)
	if send {
		if err = s.enqueueNotification(ctx, tx, created, channels); err != nil {
			return "", false, err
		}
	}
	return created, send, nil
}

func (s *Store) rulePayload(ctx context.Context, tx pgx.Tx, eventKey string, rule Rule, node, resultID, detail string, onset *time.Time) (map[string]string, error) {
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
	limits, err := s.qualityLimits(ctx, tx, node, result.Engine)
	if err != nil {
		return nil, err
	}
	value, expected, pct := metricValue(rule, result, expectedDown, expectedUp)
	zone, err := ownerTimezone(ctx, tx)
	if err != nil {
		return nil, err
	}
	extra := map[string]string{"value": value, "expected": expected, "pct": pct, "threshold": metricThreshold(rule), "count": strconv.Itoa(rule.ForTests)}
	if onset != nil {
		extra["since"] = formatOwnerTime(*onset, zone)
	}
	if reason := failureReason(result); reason != "" {
		extra["failure_reason"] = reason
	}
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
		id, _, err := s.insertEvent(ctx, tx, rule, item.NodeID, item.TS, item.ResultID, detail, &item.TS, true)
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
		if !item.nodeEnabled || !item.schedOn {
			continue
		}
		threshold := item.anchor.Add(time.Duration(item.interval*3) * time.Minute)
		hit := !now.Before(threshold)
		if err := s.advanceOffline(ctx, tx, item.rule, item.node, item.anchor, threshold, now, hit); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

type offlineAlertState struct {
	count        int
	active       bool
	eventID      *string
	onset        *time.Time
	lastNotified *time.Time
}

func (s *Store) advanceOffline(ctx context.Context, tx pgx.Tx, rule Rule, node string, anchor, threshold, now time.Time, hit bool) error {
	state, err := lockOfflineState(ctx, tx, rule.ID, node)
	if err != nil {
		return err
	}
	change := advanceStreak(state.count, state.active, hit, 1)
	config, err := resolveEventSettings(ctx, tx, "node.offline", node, rule.Severity, rule.Channels)
	if err != nil {
		return err
	}
	if err := s.openOfflineEvent(ctx, tx, rule, node, anchor, threshold, &state, change, config.CooldownSeconds); err != nil {
		return err
	}
	if err := s.repeatOfflineEvent(ctx, tx, rule, node, now, &state, change, config.CooldownSeconds, config.RepeatSeconds); err != nil {
		return err
	}
	if err := s.recoverOfflineEvent(ctx, tx, rule, node, threshold, now, &state, change); err != nil {
		return err
	}
	if !change.active {
		state.eventID = nil
	}
	if !hit {
		state.onset = nil
	}
	_, err = tx.Exec(ctx, `UPDATE alert_state SET consecutive_count=$3,active=$4,active_event_id=$5,last_evaluated_at=$6,streak_started_at=$7,last_notified_at=$8
		WHERE rule_id=$1 AND node_id=$2`, rule.ID, node, change.count, change.active, state.eventID, now, state.onset, state.lastNotified)
	return err
}

func lockOfflineState(ctx context.Context, tx pgx.Tx, ruleID, node string) (offlineAlertState, error) {
	var state offlineAlertState
	if _, err := tx.Exec(ctx, `INSERT INTO alert_state(rule_id,node_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, ruleID, node); err != nil {
		return state, err
	}
	err := tx.QueryRow(ctx, `SELECT consecutive_count,active,active_event_id::text,streak_started_at,last_notified_at
		FROM alert_state WHERE rule_id=$1 AND node_id=$2 FOR UPDATE`, ruleID, node).
		Scan(&state.count, &state.active, &state.eventID, &state.onset, &state.lastNotified)
	return state, err
}

func (s *Store) openOfflineEvent(ctx context.Context, tx pgx.Tx, rule Rule, node string, anchor, threshold time.Time, state *offlineAlertState, change streakTransition, cooldown int) error {
	if !change.opened {
		return nil
	}
	started := anchor
	state.onset = &started
	allow := shouldOpenNotification(threshold, state.lastNotified, cooldown)
	created, sent, err := s.insertEvent(ctx, tx, rule, node, threshold, "", "No heartbeat for three scheduled intervals.", state.onset, allow)
	if err != nil {
		return err
	}
	state.eventID = &created
	if sent {
		state.lastNotified = timePointer(threshold)
	}
	return nil
}

func (s *Store) repeatOfflineEvent(ctx context.Context, tx pgx.Tx, rule Rule, node string, now time.Time, state *offlineAlertState, change streakTransition, cooldown, repeat int) error {
	if !state.active || !change.active || change.opened || repeat <= 0 || !shouldRepeatNotification(now, state.lastNotified, state.onset, repeat) {
		return nil
	}
	allowDelivery := shouldOpenNotification(now, state.lastNotified, cooldown)
	created, sent, err := s.insertEvent(ctx, tx, rule, node, now, "", "Node remains offline.", state.onset, allowDelivery)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE alert_events SET resolved_at=GREATEST(ts,$2),acked=true WHERE id=$1 AND resolved_at IS NULL`, created, now); err != nil {
		return err
	}
	if sent {
		state.lastNotified = timePointer(now)
	}
	return nil
}

func (s *Store) recoverOfflineEvent(ctx context.Context, tx pgx.Tx, rule Rule, node string, threshold, now time.Time, state *offlineAlertState, change streakTransition) error {
	if change.resolved && state.eventID != nil {
		if _, err := tx.Exec(ctx, `UPDATE alert_events SET resolved_at=GREATEST(ts,$2) WHERE id=$1 AND resolved_at IS NULL`, *state.eventID, now); err != nil {
			return err
		}
		onset := threshold
		if state.onset != nil {
			onset = *state.onset
		} else if err := tx.QueryRow(ctx, `SELECT ts FROM alert_events WHERE id=$1`, *state.eventID).Scan(&onset); err != nil {
			return err
		}
		if err := s.emitRecovery(ctx, tx, rule, node, onset, now, ""); err != nil {
			return err
		}
		state.eventID = nil
		return nil
	}
	if change.recovered && state.onset != nil {
		return s.emitRecovery(ctx, tx, rule, node, *state.onset, now, "")
	}
	return nil
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
