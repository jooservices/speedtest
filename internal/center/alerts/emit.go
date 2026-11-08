package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jooservices/speedtest/internal/center/agentapi"
	"github.com/jooservices/speedtest/internal/center/catalog"
)

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

func (s *Store) qualityLimits(ctx context.Context, tx pgx.Tx, node string) (QualityLimits, error) {
	limits := DefaultQualityLimits()
	rows, err := tx.Query(ctx, `SELECT metric,value FROM alert_rules WHERE enabled AND metric IN ('down_pct','up_pct','ping','jitter','loss')
		AND (node_ids IS NULL OR $1=ANY(node_ids))`, node)
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
		switch metric {
		case "down_pct":
			limits.DownloadPct = value
		case "up_pct":
			limits.UploadPct = value
		case "ping":
			limits.PingMS = value
		case "jitter":
			limits.JitterMS = value
		case "loss":
			limits.LossPct = value
		}
	}
	return limits, rows.Err()
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
	id, err := s.newUUID()
	if err != nil {
		return "", err
	}
	raw := PayloadJSON(payload)
	// Instant notifications are resolved and acked so they do not stay in open alert activity.
	var created string
	if resultID == "" {
		err = tx.QueryRow(ctx, `INSERT INTO alert_events(id,node_id,severity,ts,resolved_at,title,detail,acked,event_key,payload)
			VALUES ($1,$2,$3,$4,$4,$5,$6,true,$7,$8) RETURNING id::text`, id, nodeID, severity, ts, title, detail, key, raw).Scan(&created)
	} else {
		err = tx.QueryRow(ctx, `INSERT INTO alert_events(id,node_id,severity,ts,resolved_at,title,detail,acked,trigger_result_id,event_key,payload)
			VALUES ($1,$2,$3,$4,$4,$5,$6,true,$7,$8,$9) RETURNING id::text`, id, nodeID, severity, ts, title, detail, resultID, key, raw).Scan(&created)
	}
	if err != nil {
		return "", err
	}
	if err = s.enqueueNotification(ctx, tx, created, channels); err != nil {
		return "", err
	}
	return created, nil
}

func (s *Store) emitRecovery(ctx context.Context, tx pgx.Tx, rule Rule, node string, openedAt, resolvedAt time.Time, resultID string) error {
	cat, ok := catalog.EventForAlertMetric(rule.Metric)
	if !ok || cat.RecoveryKey == "" {
		return nil
	}
	on, err := s.catalogEnabled(ctx, tx, cat.RecoveryKey, node)
	if err != nil || !on {
		return err
	}
	rec, _ := catalog.LookupEvent(cat.RecoveryKey)
	var nodeName string
	var expectedDown, expectedUp float64
	if err := tx.QueryRow(ctx, `SELECT name,expected_down_mbps,expected_up_mbps FROM nodes WHERE id=$1`, node).
		Scan(&nodeName, &expectedDown, &expectedUp); err != nil {
		return err
	}
	limits, err := s.qualityLimits(ctx, tx, node)
	if err != nil {
		return err
	}
	extra := map[string]string{"duration": formatDuration(resolvedAt.Sub(openedAt)), "since": openedAt.UTC().Format(time.RFC3339)}
	var result agentapi.Result
	if resultID != "" {
		var loadErr error
		result, loadErr = loadResult(ctx, tx, resultID)
		if loadErr != nil {
			return loadErr
		}
	}
	url, err := s.centerURL(ctx, tx)
	if err != nil {
		return err
	}
	payload := BuildPayload(cat.RecoveryKey, nodeName, url, rec.Severity, result, expectedDown, expectedUp, limits, extra)
	channels, err := s.eventChannels(ctx, tx, cat.RecoveryKey, rule.Channels)
	if err != nil {
		return err
	}
	_, err = s.insertCatalogEvent(ctx, tx, cat.RecoveryKey, node, rec.Severity, rec.Title, "Recovered after "+extra["duration"], resultID, channels, payload, resolvedAt)
	return err
}

func (s *Store) emitTestOutcome(ctx context.Context, tx pgx.Tx, node string, result agentapi.Result, ts time.Time, expectedDown, expectedUp float64) error {
	key := "test.success"
	if result.Manual {
		key = "test.manual_finished"
	}
	if result.Status != "ok" && result.Status != "degraded" {
		return nil
	}
	on, err := s.catalogEnabled(ctx, tx, key, node)
	if err != nil || !on {
		return err
	}
	ev, _ := catalog.LookupEvent(key)
	var nodeName string
	if err := tx.QueryRow(ctx, `SELECT name FROM nodes WHERE id=$1`, node).Scan(&nodeName); err != nil {
		return err
	}
	limits, err := s.qualityLimits(ctx, tx, node)
	if err != nil {
		return err
	}
	url, err := s.centerURL(ctx, tx)
	if err != nil {
		return err
	}
	payload := BuildPayload(key, nodeName, url, ev.Severity, result, expectedDown, expectedUp, limits, nil)
	channels, err := s.eventChannels(ctx, tx, key, ev.Channels)
	if err != nil {
		return err
	}
	_, err = s.insertCatalogEvent(ctx, tx, key, node, ev.Severity, ev.Title, "Test finished", result.ID, channels, payload, ts)
	return err
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
	err := tx.QueryRow(ctx, `SELECT id::text,ts::text,status,down_mbps,up_mbps,ping_ms,jitter_ms,loss_percent,host(ip),manual
		FROM results WHERE id=$1`, id).Scan(&r.ID, &r.TS, &r.Status, &down, &up, &ping, &jitter, &loss, &ip, &r.Manual)
	if err != nil {
		return agentapi.Result{}, err
	}
	r.Down, r.Up, r.Ping, r.Jitter, r.Loss, r.IP = down, up, ping, jitter, loss, ip
	return r, nil
}
