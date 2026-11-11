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
	_, ok := catalog.LookupEvent(key)
	if !ok {
		return false, ErrEventNotFound
	}
	store := &EventStore{DB: tx}
	event, err := store.GetEvent(ctx, key)
	if err != nil {
		return false, err
	}
	if event.NodeScoped && nodeID != "" {
		enabled, err := store.NodeEnabled(ctx, key, nodeID)
		if err != nil {
			return false, err
		}
		event.Enabled = enabled
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
	channels := event.Channels
	if exclude != "" {
		channels, err = s.escalationChannel(ctx, tx, event, exclude)
		if err != nil {
			return false, err
		}
	}
	send := event.Enabled && len(channels) > 0 && eventWindowAllows(event, event.Severity, data, ts)
	if exclude != "" && !send {
		return false, nil
	}
	nodeArg := any(nil)
	if nodeID != "" {
		nodeArg = nodeID
	}
	id := uuid.NewString()
	if _, err := tx.Exec(ctx, `INSERT INTO alert_events(id,node_id,severity,ts,resolved_at,title,detail,acked,event_key,payload)
		VALUES($1,$2,$3,$4,$4,$5,$6,true,$7,$8)`, id, nodeArg, event.Severity, ts, event.Title, event.Description, key, marshalEventPayload(data)); err != nil {
		return false, err
	}
	if send {
		if err := enqueueEvent(ctx, tx, id, channels); err != nil {
			return false, err
		}
	}
	return send, nil
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
