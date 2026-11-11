package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jooservices/speedtest/internal/center/agentapi"
	"github.com/jooservices/speedtest/internal/center/catalog"
	"github.com/jooservices/speedtest/internal/center/notify"
)

type testOutcomeBatchKey struct{}
type alertEventSettingsBatchKey struct{}

func (s *Store) centerURL(ctx context.Context, tx pgx.Tx) (string, error) {
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT value FROM settings WHERE key='display'`).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var view struct {
		CenterURL string `json:"centerUrl"`
	}
	_ = json.Unmarshal(raw, &view)
	return view.CenterURL, nil
}

type notificationQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (s *Store) qualityLimits(ctx context.Context, q notificationQuerier, node, engine string) (QualityLimits, error) {
	limits := DefaultQualityLimits()
	seen := map[string]bool{}
	rows, err := q.Query(ctx, `SELECT r.metric,CASE WHEN r.node_ids IS NULL AND r.engine IS NULL AND es.thresholds_updated_at>r.updated_at
		THEN COALESCE((es.thresholds->>r.metric)::float8,r.value) ELSE r.value END
		FROM alert_rules r
		LEFT JOIN notification_event_settings es ON es.event_key=CASE r.metric
			WHEN 'down_pct' THEN 'quality.download_low' WHEN 'up_pct' THEN 'quality.upload_low'
			WHEN 'ping' THEN 'quality.ping_high' WHEN 'jitter' THEN 'quality.jitter_high'
			WHEN 'loss' THEN 'quality.loss_high' ELSE '' END
		WHERE r.enabled
		AND r.metric IN ('down_pct','up_pct','ping','jitter','loss')
		AND (r.node_ids IS NULL OR $1=ANY(r.node_ids))
		AND COALESCE(r.engine,(SELECT primary_engine FROM nodes WHERE id=$1))=$2
		ORDER BY (r.node_ids IS NULL),(r.engine IS NULL),r.id`, node, measurementEngine(engine))
	if err != nil {
		return limits, err
	}
	defer rows.Close()
	for rows.Next() {
		var metric string
		var value float64
		if err := rows.Scan(&metric, &value); err != nil {
			return limits, err
		}
		if seen[metric] {
			continue
		}
		seen[metric] = true
		switch metric {
		case "down_pct":
			limits.DownloadPct = value
			limits.DownloadSet = true
		case "up_pct":
			limits.UploadPct = value
			limits.UploadSet = true
		case "ping":
			limits.PingMS = value
			limits.PingSet = true
		case "jitter":
			limits.JitterMS = value
			limits.JitterSet = true
		case "loss":
			limits.LossPct = value
			limits.LossSet = true
		}
	}
	if err := rows.Err(); err != nil {
		return limits, err
	}
	return limits, nil
}

type effectiveEventSettings struct {
	Enabled             bool
	Configured          bool
	Severity            string
	Channels            []string
	Thresholds          map[string]float64
	CooldownSeconds     int
	RepeatSeconds       int
	MaintenanceStart    string
	MaintenanceEnd      string
	MaintenanceTimezone string
	ISPScope            []string
}

func resolveEventSettings(ctx context.Context, q notificationQuerier, key, node, fallbackSeverity string, fallbackChannels []string) (effectiveEventSettings, error) {
	event, ok := catalog.LookupEvent(key)
	if !ok {
		return effectiveEventSettings{Enabled: true, Severity: fallbackSeverity, Channels: append([]string(nil), fallbackChannels...)}, nil
	}
	settings := effectiveEventSettings{Enabled: event.Enabled, Severity: event.Severity, Channels: append([]string(nil), event.Channels...),
		Thresholds: cloneThresholdMap(event.Thresholds), CooldownSeconds: event.CooldownSeconds,
		RepeatSeconds: event.RepeatSeconds, MaintenanceTimezone: "UTC"}
	var enabled bool
	var severity string
	var channels []string
	var thresholds []byte
	err := q.QueryRow(ctx, `SELECT enabled,severity,channels,thresholds,cooldown_seconds,repeat_seconds,
		COALESCE(to_char(maintenance_start,'HH24:MI'),''),COALESCE(to_char(maintenance_end,'HH24:MI'),''),maintenance_timezone,isp_scopes
		FROM notification_event_settings WHERE event_key=$1`, key).
		Scan(&enabled, &severity, &channels, &thresholds, &settings.CooldownSeconds, &settings.RepeatSeconds,
			&settings.MaintenanceStart, &settings.MaintenanceEnd, &settings.MaintenanceTimezone, &settings.ISPScope)
	switch {
	case err == nil:
		settings.Configured = true
		settings.Enabled, settings.Severity, settings.Channels = enabled, severity, channels
		if err = json.Unmarshal(thresholds, &settings.Thresholds); err != nil {
			return effectiveEventSettings{}, err
		}
		for k, v := range event.Thresholds {
			if _, ok := settings.Thresholds[k]; !ok {
				settings.Thresholds[k] = v
			}
		}
		// Existing alert_rules settings remain the effective defaults until the
		// owner explicitly saves an event override.
	case !errors.Is(err, pgx.ErrNoRows):
		return effectiveEventSettings{}, err
	default:
		if fallbackSeverity != "" {
			settings.Severity = fallbackSeverity
		}
		if len(fallbackChannels) > 0 {
			settings.Channels = append([]string(nil), fallbackChannels...)
		}
	}
	if event.NodeScoped && node != "" && settings.Enabled {
		var nodeEnabled bool
		err = q.QueryRow(ctx, `SELECT enabled FROM notification_event_node_settings WHERE event_key=$1 AND node_id=$2`, key, node).Scan(&nodeEnabled)
		if err == nil {
			settings.Enabled = nodeEnabled
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return effectiveEventSettings{}, err
		}
	}
	return settings, nil
}

func cloneThresholdMap(in map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func (s *Store) prepareAlertEventSettings(ctx context.Context, q notificationQuerier) (map[string]effectiveEventSettings, error) {
	settings := make(map[string]effectiveEventSettings)
	keys := make([]string, 0)
	for _, event := range catalog.Events() {
		if event.AlertMetric == "" {
			continue
		}
		keys = append(keys, event.Key)
		settings[event.Key] = effectiveEventSettings{
			Enabled: event.Enabled, Severity: event.Severity, Channels: append([]string(nil), event.Channels...),
			Thresholds: cloneThresholdMap(event.Thresholds), CooldownSeconds: event.CooldownSeconds,
			RepeatSeconds: event.RepeatSeconds, MaintenanceTimezone: "UTC",
		}
	}
	rows, err := q.Query(ctx, `/* alert_event_settings_batch */ SELECT event_key,enabled,severity,channels,cooldown_seconds,repeat_seconds,
		COALESCE(to_char(maintenance_start,'HH24:MI'),''),COALESCE(to_char(maintenance_end,'HH24:MI'),''),maintenance_timezone,isp_scopes
		FROM notification_event_settings WHERE event_key=ANY($1::text[])`, keys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var override effectiveEventSettings
		if err := rows.Scan(&key, &override.Enabled, &override.Severity, &override.Channels,
			&override.CooldownSeconds, &override.RepeatSeconds, &override.MaintenanceStart,
			&override.MaintenanceEnd, &override.MaintenanceTimezone, &override.ISPScope); err != nil {
			return nil, err
		}
		override.Configured = true
		settings[key] = override
	}
	return settings, rows.Err()
}

func ruleEventSettings(ctx context.Context, tx pgx.Tx, key, node, severity string, channels []string) (effectiveEventSettings, error) {
	settings, ok := ctx.Value(alertEventSettingsBatchKey{}).(map[string]effectiveEventSettings)
	if ok {
		if config, exists := settings[key]; exists {
			if !config.Configured {
				if severity != "" {
					config.Severity = severity
				}
				if len(channels) > 0 {
					config.Channels = append([]string(nil), channels...)
				}
			}
			return config, nil
		}
	}
	return resolveEventSettings(ctx, tx, key, node, severity, channels)
}

func eventDeliveryAllowed(event effectiveEventSettings, severity string, payload map[string]string, ts time.Time) bool {
	if len(event.ISPScope) > 0 {
		isp := strings.TrimSpace(payload["isp"])
		matched := false
		for _, allowed := range event.ISPScope {
			if strings.EqualFold(isp, strings.TrimSpace(allowed)) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if severity == "crit" || event.MaintenanceStart == "" || event.MaintenanceEnd == "" {
		return true
	}
	start, startErr := time.Parse("15:04", event.MaintenanceStart)
	end, endErr := time.Parse("15:04", event.MaintenanceEnd)
	location, zoneErr := time.LoadLocation(event.MaintenanceTimezone)
	if startErr != nil || endErr != nil || zoneErr != nil {
		return true
	}
	local := ts.In(location)
	minute := local.Hour()*60 + local.Minute()
	from, to := start.Hour()*60+start.Minute(), end.Hour()*60+end.Minute()
	if from == to {
		return false
	}
	if from < to {
		return minute < from || minute >= to
	}
	return minute < from && minute >= to
}

func (s *Store) eventChannels(ctx context.Context, tx pgx.Tx, key string, fallback []string) ([]string, error) {
	var channels []string
	err := tx.QueryRow(ctx, `SELECT channels FROM notification_event_settings WHERE event_key=$1`, key).Scan(&channels)
	if errors.Is(err, pgx.ErrNoRows) {
		if e, ok := catalog.LookupEvent(key); ok && len(e.Channels) > 0 {
			return append([]string(nil), e.Channels...), nil
		}
		return append([]string(nil), fallback...), nil
	}
	if err != nil {
		return nil, err
	}
	if len(channels) == 0 {
		return append([]string(nil), fallback...), nil
	}
	return channels, nil
}

func (s *Store) catalogEnabled(ctx context.Context, tx pgx.Tx, key, nodeID string) (bool, error) {
	e, ok := catalog.LookupEvent(key)
	if !ok {
		return false, nil
	}
	enabled := e.Enabled
	var override bool
	err := tx.QueryRow(ctx, `SELECT enabled FROM notification_event_settings WHERE event_key=$1`, key).Scan(&override)
	if err == nil {
		enabled = override
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if !enabled || !e.NodeScoped {
		return enabled, nil
	}
	err = tx.QueryRow(ctx, `SELECT enabled FROM notification_event_node_settings WHERE event_key=$1 AND node_id=$2`, key, nodeID).Scan(&override)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	return override, err
}

func (s *Store) insertCatalogEvent(ctx context.Context, tx pgx.Tx, key, nodeID, severity, title, detail, resultID string, channels []string, payload map[string]string, ts time.Time) (string, error) {
	if len(channels) == 0 {
		var err error
		channels, err = s.eventChannels(ctx, tx, key, []string{"telegram"})
		if err != nil {
			return "", err
		}
	}
	eventSettings, err := resolveEventSettings(ctx, tx, key, nodeID, severity, channels)
	if err != nil {
		return "", err
	}
	return s.insertCatalogEventWithSettings(ctx, tx, key, nodeID, title, detail, resultID, payload, ts, eventSettings)
}

func (s *Store) insertCatalogEventWithSettings(ctx context.Context, tx pgx.Tx, key, nodeID, title, detail, resultID string, payload map[string]string, ts time.Time, eventSettings effectiveEventSettings) (string, error) {
	severity, channels := eventSettings.Severity, eventSettings.Channels
	send := eventSettings.Enabled && eventDeliveryAllowed(eventSettings, severity, payload, ts)
	if send {
		var err error
		send, err = notify.PolicyAllows(ctx, tx, key, nodeID, ts, eventSettings.CooldownSeconds, eventSettings.RepeatSeconds)
		if err != nil {
			return "", err
		}
	}
	id, err := s.newUUID()
	if err != nil {
		return "", err
	}
	raw := PayloadJSON(payload)
	// Instant notifications are resolved and acked so they do not stay in open alert activity.
	var query string
	var args []any
	if resultID == "" {
		query = `INSERT INTO alert_events(id,node_id,severity,ts,resolved_at,title,detail,acked,event_key,payload,isp_id)
			VALUES ($1,$2,$3,$4,$4,$5,$6,true,$7,$8,
				(SELECT r.isp_id FROM results r WHERE r.node_id=$2 AND r.ts<=$4 AND r.isp_id IS NOT NULL ORDER BY r.ts DESC,r.id DESC LIMIT 1))`
		args = []any{id, nodeID, severity, ts, title, detail, key, raw}
	} else {
		query = `INSERT INTO alert_events(id,node_id,severity,ts,resolved_at,title,detail,acked,trigger_result_id,event_key,payload,isp_id)
			VALUES ($1,$2,$3,$4,$4,$5,$6,true,$7,$8,$9,(SELECT r.isp_id FROM results r WHERE r.id=$7))`
		args = []any{id, nodeID, severity, ts, title, detail, resultID, key, raw}
	}
	if send {
		args = append(args, channels)
		query = "WITH inserted AS (" + query + " RETURNING id) " +
			"INSERT INTO alert_notification_outbox(event_id,channels) SELECT id,$" + strconv.Itoa(len(args)) + " FROM inserted ON CONFLICT(event_id) DO NOTHING"
	}
	if _, err = tx.Exec(ctx, query, args...); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) emitRecovery(ctx context.Context, tx pgx.Tx, rule Rule, node string, openedAt, resolvedAt time.Time, resultID string) error {
	cat, ok := catalog.EventForAlertMetric(rule.Metric)
	if !ok || cat.RecoveryKey == "" {
		return nil
	}
	rec, _ := catalog.LookupEvent(cat.RecoveryKey)
	config, err := resolveEventSettings(ctx, tx, cat.RecoveryKey, node, rec.Severity, rule.Channels)
	if err != nil || !config.Enabled {
		return err
	}
	var nodeName string
	var expectedDown, expectedUp float64
	if err := tx.QueryRow(ctx, `SELECT name,expected_down_mbps,expected_up_mbps FROM nodes WHERE id=$1`, node).
		Scan(&nodeName, &expectedDown, &expectedUp); err != nil {
		return err
	}
	var result agentapi.Result
	if resultID != "" {
		result, err = loadResult(ctx, tx, resultID)
		if err != nil {
			return err
		}
	}
	limits, err := s.qualityLimits(ctx, tx, node, result.Engine)
	if err != nil {
		return err
	}
	zone, err := ownerTimezone(ctx, tx)
	if err != nil {
		return err
	}
	extra := map[string]string{"duration": formatDuration(resolvedAt.Sub(openedAt)), "since": formatOwnerTime(openedAt, zone)}
	if reason := failureReason(result); reason != "" {
		extra["failure_reason"] = reason
	}
	url, err := s.centerURL(ctx, tx)
	if err != nil {
		return err
	}
	payload := BuildPayload(cat.RecoveryKey, nodeName, url, config.Severity, result, expectedDown, expectedUp, limits, extra)
	_, err = s.insertCatalogEvent(ctx, tx, cat.RecoveryKey, node, config.Severity, rec.Title, "Recovered after "+extra["duration"], resultID, config.Channels, payload, resolvedAt)
	return err
}

func (s *Store) emitTestOutcome(ctx context.Context, tx pgx.Tx, node string, result agentapi.Result, ts time.Time) error {
	keys := testOutcomeKeys(result)
	for _, key := range keys {
		if err := s.emitOneTestOutcome(ctx, tx, node, result, ts, key); err != nil {
			return err
		}
	}
	return nil
}

func testOutcomeKeys(result agentapi.Result) []string {
	keys := make([]string, 0, 2)
	success := result.Status == "ok" || result.Status == "degraded"
	if success {
		keys = append(keys, "test.success")
	}
	if result.Manual {
		keys = append(keys, "test.manual_finished")
	}
	return keys
}

func (s *Store) emitOneTestOutcome(ctx context.Context, tx pgx.Tx, node string, result agentapi.Result, ts time.Time, key string) error {
	ev, ok := catalog.LookupEvent(key)
	if !ok {
		return nil
	}
	configs, _ := ctx.Value(testOutcomeBatchKey{}).(map[string]testOutcomeConfig)
	configKey := eventConfigKey(key, result.Engine)
	config, found := configs[configKey]
	var err error
	if !found {
		config, err = s.testOutcomeConfig(ctx, tx, node, ev, result.Engine)
	}
	if err != nil || !config.Enabled {
		return err
	}
	extra := map[string]string{"since": formatOwnerTime(ts, config.Timezone)}
	if reason := failureReason(result); reason != "" {
		extra["failure_reason"] = reason
	}
	payload := BuildPayload(key, config.NodeName, config.CenterURL, config.Severity, result, config.ExpectedDown, config.ExpectedUp, config.Limits, extra)
	detail := "Test finished"
	if result.Error != nil {
		detail = *result.Error
	}
	_, err = s.insertCatalogEventWithSettings(ctx, tx, key, node, ev.Title, detail, result.ID, payload, ts, config.EventSettings)
	return err
}

// PrepareResultBatch loads notification settings once for all successful results in a request.
func (s *Store) PrepareResultBatch(ctx context.Context, node string, results []agentapi.Result) (context.Context, error) {
	ctx = context.WithValue(ctx, ruleEvaluationBatchKey{}, &ruleEvaluationBatch{lastEvaluated: make(map[string]time.Time)})
	ctx, err := s.prepareObservedBatch(ctx, node, results)
	if err != nil {
		return ctx, err
	}
	alertSettings, err := s.prepareAlertEventSettings(ctx, s.DB)
	if err != nil {
		return ctx, err
	}
	ctx = context.WithValue(ctx, alertEventSettingsBatchKey{}, alertSettings)
	events := make(map[string]catalog.Event, 3)
	for _, result := range results {
		for _, key := range testOutcomeKeys(result) {
			if event, ok := catalog.LookupEvent(key); ok {
				events[eventConfigKey(key, result.Engine)] = event
			}
		}
	}
	if len(events) == 0 {
		return ctx, nil
	}
	configs := make(map[string]testOutcomeConfig, len(events))
	for mapKey, event := range events {
		engine := strings.TrimPrefix(mapKey, event.Key+"|")
		config, err := s.testOutcomeConfig(ctx, s.DB, node, event, engine)
		if err != nil {
			return ctx, err
		}
		configs[mapKey] = config
	}
	ctx = context.WithValue(ctx, testOutcomeBatchKey{}, configs)
	for _, result := range results {
		if result.Status != "ok" && result.Status != "degraded" {
			continue
		}
		configured, err := configuredBandwidthBudget(ctx, s.DB)
		if err != nil {
			return ctx, err
		}
		return context.WithValue(ctx, bandwidthBudgetBatchKey{}, configured), nil
	}
	return ctx, nil
}

type testOutcomeConfig struct {
	NodeName, CenterURL      string
	ExpectedDown, ExpectedUp float64
	Enabled                  bool
	Severity                 string
	Channels                 []string
	Limits                   QualityLimits
	Timezone                 string
	EventSettings            effectiveEventSettings
}

func eventConfigKey(key, engine string) string { return key + "|" + measurementEngine(engine) }

func (s *Store) testOutcomeConfig(ctx context.Context, q notificationQuerier, node string, event catalog.Event, engine string) (testOutcomeConfig, error) {
	var config testOutcomeConfig
	effective, err := resolveEventSettings(ctx, q, event.Key, node, "", nil)
	if err != nil {
		return config, err
	}
	config.Enabled, config.Severity, config.Channels = effective.Enabled, effective.Severity, effective.Channels
	config.EventSettings = effective
	if !config.Enabled {
		return config, nil
	}
	if err := q.QueryRow(ctx, `/* test_outcome_config */
		SELECT n.name,n.expected_down_mbps,n.expected_up_mbps,
			COALESCE((SELECT value->>'centerUrl' FROM settings WHERE key='display'),'')
		FROM nodes n WHERE n.id=$1`, node).
		Scan(&config.NodeName, &config.ExpectedDown, &config.ExpectedUp, &config.CenterURL); err != nil {
		return config, err
	}
	config.Timezone, err = ownerTimezone(ctx, q)
	if err != nil {
		return config, err
	}
	config.Limits, err = s.qualityLimits(ctx, q, node, engine)
	return config, err
}

func ownerTimezone(ctx context.Context, q notificationQuerier) (string, error) {
	var zone string
	if err := q.QueryRow(ctx, `SELECT COALESCE(
		(SELECT value #>> '{}' FROM settings WHERE key='notifyQuietTimezone'),
		(SELECT value #>> '{}' FROM settings WHERE key='timezone'),
		'Asia/Bangkok')`).Scan(&zone); errors.Is(err, pgx.ErrNoRows) {
		return "Asia/Bangkok", nil
	} else if err != nil {
		return "", err
	}
	if zone == "" {
		return "Asia/Bangkok", nil
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return "Asia/Bangkok", nil
	}
	return zone, nil
}

func formatOwnerTime(value time.Time, zone string) string {
	location, err := time.LoadLocation(zone)
	if err != nil {
		location = time.UTC
	}
	return value.In(location).Format(time.RFC3339)
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	return d.Round(time.Minute).String()
}

func loadResult(ctx context.Context, tx pgx.Tx, id string) (agentapi.Result, error) {
	var r agentapi.Result
	var down, up, ping, jitter, loss *float64
	var ip *string
	var rawServer []byte
	err := tx.QueryRow(ctx, `SELECT id::text,ts::text,status,down_mbps,up_mbps,ping_ms,jitter_ms,loss_percent,host(ip),manual,
		server,engine_isp,error,failure_class,engine FROM results WHERE id=$1`, id).
		Scan(&r.ID, &r.TS, &r.Status, &down, &up, &ping, &jitter, &loss, &ip, &r.Manual, &rawServer, &r.EngineISP, &r.Error, &r.FailureClass, &r.Engine)
	if err != nil {
		return agentapi.Result{}, err
	}
	r.Down, r.Up, r.Ping, r.Jitter, r.Loss, r.IP = down, up, ping, jitter, loss, ip
	if len(rawServer) > 0 && string(rawServer) != "null" {
		var server agentapi.Server
		if err := json.Unmarshal(rawServer, &server); err != nil {
			return agentapi.Result{}, err
		}
		r.Server = &server
	}
	return r, nil
}
