package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jooservices/speedtest/internal/center/catalog"
	"github.com/jooservices/speedtest/internal/center/notify/template"
)

type eventPolicyQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// PolicyAllows applies event cooldown/repeat settings to a new notification
// intent, using the outbox as the durable timestamp of the last queued send.
func PolicyAllows(ctx context.Context, q eventPolicyQuerier, key, nodeID string, ts time.Time, cooldown, repeat int) (bool, error) {
	interval := cooldown
	if repeat > interval {
		interval = repeat
	}
	if interval <= 0 {
		return true, nil
	}
	var last *time.Time
	err := q.QueryRow(ctx, `SELECT max(e.ts) FROM alert_events e
		WHERE e.event_key=$1 AND e.node_id IS NOT DISTINCT FROM NULLIF($2,'')::uuid AND e.ts<=$3
		AND EXISTS (SELECT 1 FROM alert_notification_outbox o WHERE o.event_id=e.id)`, key, nodeID, ts).Scan(&last)
	if err != nil {
		return false, err
	}
	return last == nil || ts.Sub(*last) >= time.Duration(interval)*time.Second, nil
}

// PublishEvent records a catalog event and hands its configured channels to the durable outbox.
func (s *Store) PublishEvent(ctx context.Context, key, nodeID string, payload map[string]string) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := s.publishEventTx(ctx, tx, key, nodeID, payload, s.now(), ""); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// PublishEventTx adds a catalog event to a caller-owned transaction.
func (s *Store) PublishEventTx(ctx context.Context, tx pgx.Tx, key, nodeID string, payload map[string]string) error {
	_, err := s.publishEventTx(ctx, tx, key, nodeID, payload, s.now(), "")
	return err
}

func (s *Store) publishEventTx(ctx context.Context, tx pgx.Tx, key, nodeID string, payload map[string]string, ts time.Time, exclude string) (bool, error) {
	return s.publishEventTxWithChannelFilter(ctx, tx, key, nodeID, payload, ts, exclude, nil)
}

func (s *Store) publishEventTxWithChannelFilter(ctx context.Context, tx pgx.Tx, key, nodeID string, payload map[string]string, ts time.Time, exclude string, allowedChannels []string) (bool, error) {
	event, err := publicationEvent(ctx, tx, key, nodeID)
	if err != nil {
		return false, err
	}
	duplicate, err := releaseAlreadyNotified(ctx, tx, key, nodeID, payload)
	if err != nil {
		return false, err
	}
	if duplicate {
		return false, nil
	}
	data, err := buildEventPayload(ctx, tx, event, nodeID, payload, ts)
	if err != nil {
		return false, err
	}
	event, err = s.restrictEventChannels(ctx, tx, event, allowedChannels)
	if err != nil {
		return false, err
	}
	channels, send, err := s.eventDelivery(ctx, tx, event, key, nodeID, data, ts, exclude)
	if err != nil {
		return false, err
	}
	if exclude != "" && !send {
		return false, nil
	}
	if err := recordEventTx(ctx, tx, event, nodeID, data, ts, channels, send); err != nil {
		return false, err
	}
	return send, nil
}

func (s *Store) restrictEventChannels(ctx context.Context, tx pgx.Tx, event EventView, allowed []string) (EventView, error) {
	if !event.Enabled || allowed == nil {
		return event, nil
	}
	channels, err := configuredChannels(ctx, s, tx, event.Channels, allowed)
	if err != nil {
		return EventView{}, err
	}
	event.Channels = channels
	return event, nil
}

func publicationEvent(ctx context.Context, tx pgx.Tx, key, nodeID string) (EventView, error) {
	_, ok := catalog.LookupEvent(key)
	if !ok {
		return EventView{}, ErrEventNotFound
	}
	store := &EventStore{DB: tx}
	event, err := store.GetEvent(ctx, key)
	if err != nil {
		return EventView{}, err
	}
	if event.NodeScoped && nodeID != "" {
		enabled, err := store.NodeEnabled(ctx, key, nodeID)
		if err != nil {
			return EventView{}, err
		}
		event.Enabled = enabled
	}
	return event, nil
}

func recordEventTx(ctx context.Context, tx pgx.Tx, event EventView, nodeID string, data map[string]string, ts time.Time, channels []string, send bool) error {
	nodeArg := any(nil)
	if nodeID != "" {
		nodeArg = nodeID
	}
	id := uuid.NewString()
	if _, err := tx.Exec(ctx, `INSERT INTO alert_events(id,node_id,severity,ts,resolved_at,title,detail,acked,event_key,payload)
		VALUES($1,$2,$3,$4,$4,$5,$6,true,$7,$8)`, id, nodeArg, event.Severity, ts, event.Title, event.Description, event.Key, marshalEventPayload(data)); err != nil {
		return err
	}
	if send {
		if err := enqueueEvent(ctx, tx, id, channels); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) eventDelivery(ctx context.Context, tx pgx.Tx, event EventView, key, nodeID string, data map[string]string, ts time.Time, exclude string) ([]string, bool, error) {
	channels := event.Channels
	var err error
	if exclude != "" {
		channels, err = s.escalationChannel(ctx, tx, event, exclude)
		if err != nil {
			return nil, false, err
		}
	}
	send := event.Enabled && len(channels) > 0 && eventWindowAllows(event, event.Severity, data, ts)
	if send {
		send, err = PolicyAllows(ctx, tx, key, nodeID, ts, event.CooldownSeconds, event.RepeatSeconds)
	}
	return channels, send, err
}

func releaseAlreadyNotified(ctx context.Context, tx pgx.Tx, key, nodeID string, payload map[string]string) (bool, error) {
	if (key != "agent.update_available" && key != "agent.version_older") || nodeID == "" || payload["version"] == "" {
		return false, nil
	}
	var exists bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM alert_events WHERE node_id=$1 AND event_key=$2 AND payload->>'version'=$3 AND ts >= now()-interval '30 days')`, nodeID, key, payload["version"]).Scan(&exists)
	return exists, err
}

func buildEventPayload(ctx context.Context, tx pgx.Tx, event EventView, nodeID string, payload map[string]string, ts time.Time) (map[string]string, error) {
	data := mapsClone(payload)
	if nodeID != "" && data["node"] == "" {
		var nodeName string
		if err := tx.QueryRow(ctx, `SELECT name FROM nodes WHERE id=$1`, nodeID).Scan(&nodeName); err != nil {
			return nil, err
		}
		data["node"] = nodeName
	}
	data["event"], data["severity"] = event.Title, event.Severity
	data["severity_icon"] = template.SeverityIcon(event.Severity)
	if data["since"] == "" {
		data["since"] = ts.Format(time.RFC3339)
	}
	return data, nil
}

func (s *Store) escalateDeliveryFailure(ctx context.Context, tx pgx.Tx, deliveryID, sourceEventID, failedChannel string, attempts int, now time.Time) error {
	var sourceKey, nodeID string
	err := tx.QueryRow(ctx, `SELECT COALESCE(event_key,''),COALESCE(node_id::text,'') FROM alert_events WHERE id=$1`, sourceEventID).Scan(&sourceKey, &nodeID)
	if err != nil {
		return err
	}
	if sourceKey == "platform.delivery_failing" {
		return nil
	}
	var escalated *time.Time
	if err := tx.QueryRow(ctx, `SELECT escalated_at FROM notification_deliveries WHERE id=$1`, deliveryID).Scan(&escalated); err != nil {
		return err
	}
	if escalated != nil {
		return nil
	}
	payload := map[string]string{"event": sourceKey, "channel": failedChannel, "count": fmt.Sprint(attempts), "failure_reason": "Delivery failed after repeated attempts."}
	sent, err := s.publishEventTx(ctx, tx, "platform.delivery_failing", nodeID, payload, now, failedChannel)
	if err != nil || !sent {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE notification_deliveries SET escalated_at=$2 WHERE id=$1 AND escalated_at IS NULL`, deliveryID, now)
	return err
}

func enqueueEvent(ctx context.Context, tx pgx.Tx, eventID string, channels []string) error {
	_, err := tx.Exec(ctx, `INSERT INTO alert_notification_outbox(event_id,channels) VALUES($1,$2) ON CONFLICT(event_id) DO NOTHING`, eventID, channels)
	return err
}

func marshalEventPayload(data map[string]string) []byte {
	raw, _ := json.Marshal(data)
	return raw
}

func mapsClone(input map[string]string) map[string]string {
	copy := make(map[string]string, len(input)+4)
	for key, value := range input {
		copy[key] = value
	}
	return copy
}

func (s *Store) escalationChannel(ctx context.Context, tx pgx.Tx, event EventView, failed string) ([]string, error) {
	candidates := slices.Clone(event.Channels)
	for _, channel := range []string{"telegram", "email", "slack", "webhook"} {
		if !slices.Contains(candidates, channel) {
			candidates = append(candidates, channel)
		}
	}
	for _, channel := range candidates {
		if channel == failed {
			continue
		}
		if _, _, _, err := s.configurationTx(ctx, tx, channel); err == nil {
			return []string{channel}, nil
		} else if !errors.Is(err, ErrUnconfigured) {
			return nil, err
		}
	}
	return nil, nil
}

func eventWindowAllows(event EventView, severity string, payload map[string]string, ts time.Time) bool {
	if len(event.ISPScope) > 0 {
		matched := slices.ContainsFunc(event.ISPScope, func(scope string) bool {
			return strings.EqualFold(strings.TrimSpace(scope), strings.TrimSpace(payload["isp"]))
		})
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
	minute, from, to := local.Hour()*60+local.Minute(), start.Hour()*60+start.Minute(), end.Hour()*60+end.Minute()
	if from == to {
		return false
	}
	if from < to {
		return minute < from || minute >= to
	}
	return minute < from && minute >= to
}
