package notify

import (
	"context"

	"github.com/jooservices/speedtest/internal/center/audit"
)

func (s *Store) eventAPI() *EventStore {
	if s.EventsDB != nil {
		return &EventStore{DB: s.EventsDB}
	}
	return &EventStore{DB: s.DB}
}

// ListNotificationEvents returns the effective catalog for the Settings UI.
func (s *Store) ListNotificationEvents(ctx context.Context) ([]EventView, error) {
	return s.eventAPI().ListEvents(ctx)
}

// GetNotificationEvent returns one effective catalog event.
func (s *Store) GetNotificationEvent(ctx context.Context, key string) (EventView, error) {
	return s.eventAPI().GetEvent(ctx, key)
}

// UpdateNotificationEvent stores owner overrides for one event.
func (s *Store) UpdateNotificationEvent(ctx context.Context, key string, patch EventPatch) (EventView, error) {
	return s.eventAPI().UpdateEvent(ctx, key, patch)
}

// GetNotificationTemplate returns one channel template.
func (s *Store) GetNotificationTemplate(ctx context.Context, key, channel string) (TemplateView, error) {
	return s.eventAPI().GetTemplate(ctx, key, channel)
}

// PutNotificationTemplate stores a custom template body.
func (s *Store) PutNotificationTemplate(ctx context.Context, key, channel, body string) (TemplateView, error) {
	return s.eventAPI().PutTemplate(ctx, key, channel, body)
}

// ResetNotificationTemplate restores the catalog default.
func (s *Store) ResetNotificationTemplate(ctx context.Context, key, channel string) (TemplateView, error) {
	return s.eventAPI().ResetTemplate(ctx, key, channel)
}

// PreviewNotificationTemplate renders a body with sample data.
func (s *Store) PreviewNotificationTemplate(ctx context.Context, key, channel, body string) (TemplatePreview, error) {
	return s.eventAPI().PreviewTemplate(ctx, key, channel, body)
}

// SetNotificationNodeEnabled stores a per-node enable override.
func (s *Store) SetNotificationNodeEnabled(ctx context.Context, key, nodeID string, enabled bool) error {
	return s.eventAPI().SetNodeEnabled(ctx, key, nodeID, enabled)
}

// NotificationNodeEnabled returns the effective node override for an event.
func (s *Store) NotificationNodeEnabled(ctx context.Context, key, nodeID string) (bool, error) {
	return s.eventAPI().NodeEnabled(ctx, key, nodeID)
}

func auditedEventChange[T any](ctx context.Context, s *Store, entry audit.Entry, change func(*EventStore) (T, error)) (T, error) {
	var zero T
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return zero, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	value, err := change(&EventStore{DB: tx})
	if err != nil {
		return zero, err
	}
	if err := audit.RecordTx(ctx, tx, entry); err != nil {
		return zero, err
	}
	if err := tx.Commit(ctx); err != nil {
		return zero, err
	}
	return value, nil
}

// UpdateNotificationEventAudited commits an event override and its audit row atomically.
func (s *Store) UpdateNotificationEventAudited(ctx context.Context, key string, patch EventPatch, entry audit.Entry) (EventView, error) {
	return auditedEventChange(ctx, s, entry, func(store *EventStore) (EventView, error) { return store.UpdateEvent(ctx, key, patch) })
}

// SetNotificationNodeEnabledAudited commits a node override and its audit row atomically.
func (s *Store) SetNotificationNodeEnabledAudited(ctx context.Context, key, node string, enabled bool, entry audit.Entry) error {
	_, err := auditedEventChange(ctx, s, entry, func(store *EventStore) (struct{}, error) {
		return struct{}{}, store.SetNodeEnabled(ctx, key, node, enabled)
	})
	return err
}

// PutNotificationTemplateAudited commits a template and its audit row atomically.
func (s *Store) PutNotificationTemplateAudited(ctx context.Context, key, channel, body string, entry audit.Entry) (TemplateView, error) {
	return auditedEventChange(ctx, s, entry, func(store *EventStore) (TemplateView, error) { return store.PutTemplate(ctx, key, channel, body) })
}

// ResetNotificationTemplateAudited commits a template reset and its audit row atomically.
func (s *Store) ResetNotificationTemplateAudited(ctx context.Context, key, channel string, entry audit.Entry) (TemplateView, error) {
	return auditedEventChange(ctx, s, entry, func(store *EventStore) (TemplateView, error) { return store.ResetTemplate(ctx, key, channel) })
}
