package notify

import (
	"context"
	"slices"

	"github.com/google/uuid"
	"github.com/jooservices/speedtest/internal/center/notify/template"
)

const templateTestDetail = "Template test"

// TestNotificationEvent queues one sample through the saved template and the real channel transport.
func (s *Store) TestNotificationEvent(ctx context.Context, key, channel string) (Delivery, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Delivery{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	store := &EventStore{DB: tx}
	event, err := store.GetEvent(ctx, key)
	if err != nil {
		return Delivery{}, err
	}
	if !event.Enabled || !slices.Contains(event.Channels, channel) {
		return Delivery{}, ErrInvalidEvent
	}
	if _, _, _, err := s.configurationTx(ctx, tx, channel); err != nil {
		return Delivery{}, err
	}
	body, err := store.GetTemplate(ctx, key, channel)
	if err != nil {
		return Delivery{}, err
	}
	payload := template.Sample(key)
	payload["event"], payload["severity"] = event.Title, event.Severity
	payload["severity_icon"] = template.SeverityIcon(event.Severity)
	if _, err := template.Render(channel, body.Body, payload); err != nil {
		return Delivery{}, err
	}
	now, eventID, deliveryID := s.now(), uuid.NewString(), uuid.NewString()
	if _, err := tx.Exec(ctx, `INSERT INTO alert_events(id,node_id,severity,ts,resolved_at,title,detail,acked,event_key,payload)
		VALUES($1,NULL,$2,$3,$3,$4,$5,true,$6,$7)`, eventID, event.Severity, now, event.Title, templateTestDetail, key, marshalEventPayload(payload)); err != nil {
		return Delivery{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO notification_deliveries(id,event_id,channel,created_at,next_attempt_at)
		VALUES($1,$2,$3,$4,$4)`, deliveryID, eventID, channel, now); err != nil {
		return Delivery{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Delivery{}, err
	}
	return s.Delivery(ctx, deliveryID)
}
