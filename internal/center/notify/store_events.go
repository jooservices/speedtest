package notify

import "context"

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
