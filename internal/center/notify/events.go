package notify

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jooservices/speedtest/internal/center/catalog"
	"github.com/jooservices/speedtest/internal/center/notify/template"
)

var (
	// ErrEventNotFound is returned for unknown catalog keys.
	ErrEventNotFound = errors.New("notification event not found")
	// ErrTemplateNotFound is returned when a channel has no default template.
	ErrTemplateNotFound = errors.New("notification template not found")
	// ErrInvalidEvent is returned for invalid event or template writes.
	ErrInvalidEvent = errors.New("invalid notification event input")
)

// EventView is the effective event configuration (catalog merged with overrides).
type EventView struct {
	Key          string   `json:"key"`
	Category     string   `json:"category"`
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	Enabled      bool     `json:"enabled"`
	Severity     string   `json:"severity"`
	Channels     []string `json:"channels"`
	NodeScoped   bool     `json:"nodeScoped"`
	EngineScoped bool     `json:"engineScoped"`
	Placeholders []string `json:"placeholders"`
	Customized   bool     `json:"customized"`
}

// EventPatch updates owner-level event settings.
type EventPatch struct {
	Enabled  *bool
	Severity *string
	Channels []string
}

// TemplateView is one channel template with default tracking.
type TemplateView struct {
	EventKey  string `json:"eventKey"`
	Channel   string `json:"channel"`
	Body      string `json:"body"`
	IsDefault bool   `json:"isDefault"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// TemplatePreview is a rendered sample for the Settings editor.
type TemplatePreview struct {
	Channel string            `json:"channel"`
	Body    string            `json:"body"`
	Sample  map[string]string `json:"sample"`
}

// eventDB is the PostgreSQL surface EventStore needs (*pgxpool.Pool satisfies it).
type eventDB interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// EventStore reads and writes catalog overrides.
type EventStore struct {
	DB eventDB
}

// ListEvents returns every catalog event with effective settings.
func (s *EventStore) ListEvents(ctx context.Context) ([]EventView, error) {
	overrides, err := s.loadSettings(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]EventView, 0, len(catalog.Events()))
	for _, e := range catalog.Events() {
		out = append(out, mergeEvent(e, overrides[e.Key]))
	}
	return out, nil
}

// GetEvent returns one effective event.
func (s *EventStore) GetEvent(ctx context.Context, key string) (EventView, error) {
	base, ok := catalog.LookupEvent(key)
	if !ok {
		return EventView{}, ErrEventNotFound
	}
	overrides, err := s.loadSettings(ctx)
	if err != nil {
		return EventView{}, err
	}
	return mergeEvent(base, overrides[key]), nil
}

// UpdateEvent stores owner overrides for one event.
func (s *EventStore) UpdateEvent(ctx context.Context, key string, patch EventPatch) (EventView, error) {
	base, ok := catalog.LookupEvent(key)
	if !ok {
		return EventView{}, ErrEventNotFound
	}
	current := mergeEvent(base, eventOverride{})
	if existing, err := s.loadSettings(ctx); err != nil {
		return EventView{}, err
	} else if o, ok := existing[key]; ok {
		current = mergeEvent(base, o)
	}
	enabled, severity, channels := current.Enabled, current.Severity, slices.Clone(current.Channels)
	if patch.Enabled != nil {
		enabled = *patch.Enabled
	}
	if patch.Severity != nil {
		severity = strings.TrimSpace(*patch.Severity)
	}
	if patch.Channels != nil {
		channels = slices.Clone(patch.Channels)
	}
	if err := validateEventWrite(severity, channels); err != nil {
		return EventView{}, err
	}
	_, err := s.DB.Exec(ctx, `INSERT INTO notification_event_settings(event_key,enabled,severity,channels,updated_at)
		VALUES($1,$2,$3,$4,now())
		ON CONFLICT(event_key) DO UPDATE SET enabled=EXCLUDED.enabled,severity=EXCLUDED.severity,channels=EXCLUDED.channels,updated_at=now()`,
		key, enabled, severity, channels)
	if err != nil {
		return EventView{}, err
	}
	return s.GetEvent(ctx, key)
}

// GetTemplate returns the effective template body for event/channel.
func (s *EventStore) GetTemplate(ctx context.Context, key, channel string) (TemplateView, error) {
	def, ok := template.DefaultBody(key, channel)
	if !ok {
		return templateLookupError(key)
	}
	var body string
	var updated time.Time
	err := s.DB.QueryRow(ctx, `SELECT body,updated_at FROM notification_templates WHERE event_key=$1 AND channel=$2`, key, channel).
		Scan(&body, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return TemplateView{EventKey: key, Channel: channel, Body: def, IsDefault: true}, nil
	}
	if err != nil {
		return TemplateView{}, err
	}
	return TemplateView{EventKey: key, Channel: channel, Body: body, IsDefault: body == def, UpdatedAt: updated.UTC().Format(time.RFC3339Nano)}, nil
}

// PutTemplate validates and stores a custom template body.
func (s *EventStore) PutTemplate(ctx context.Context, key, channel, body string) (TemplateView, error) {
	if _, ok := template.DefaultBody(key, channel); !ok {
		return templateLookupError(key)
	}
	body = strings.ReplaceAll(body, "\r\n", "\n")
	if err := template.Validate(key, body); err != nil {
		return TemplateView{}, err
	}
	_, err := s.DB.Exec(ctx, `INSERT INTO notification_templates(event_key,channel,body,updated_at)
		VALUES($1,$2,$3,now())
		ON CONFLICT(event_key,channel) DO UPDATE SET body=EXCLUDED.body,updated_at=now()`, key, channel, body)
	if err != nil {
		return TemplateView{}, err
	}
	return s.GetTemplate(ctx, key, channel)
}

// ResetTemplate deletes the override so the catalog default applies again.
func (s *EventStore) ResetTemplate(ctx context.Context, key, channel string) (TemplateView, error) {
	if _, ok := template.DefaultBody(key, channel); !ok {
		return templateLookupError(key)
	}
	if _, err := s.DB.Exec(ctx, `DELETE FROM notification_templates WHERE event_key=$1 AND channel=$2`, key, channel); err != nil {
		return TemplateView{}, err
	}
	return s.GetTemplate(ctx, key, channel)
}

// PreviewTemplate renders body (or the stored/default body) with sample data.
func (s *EventStore) PreviewTemplate(ctx context.Context, key, channel, body string) (TemplatePreview, error) {
	if body == "" {
		view, err := s.GetTemplate(ctx, key, channel)
		if err != nil {
			return TemplatePreview{}, err
		}
		body = view.Body
	}
	if err := template.Validate(key, body); err != nil {
		return TemplatePreview{}, err
	}
	sample := template.Sample(key)
	rendered, err := template.Render(channel, body, sample)
	if err != nil {
		return TemplatePreview{}, err
	}
	return TemplatePreview{Channel: channel, Body: rendered, Sample: sample}, nil
}

// NodeEnabled reports whether a node-scoped event is on for the node.
func (s *EventStore) NodeEnabled(ctx context.Context, key, nodeID string) (bool, error) {
	view, err := s.GetEvent(ctx, key)
	if err != nil {
		return false, err
	}
	if !view.Enabled {
		return false, nil
	}
	if !view.NodeScoped {
		return true, nil
	}
	var enabled bool
	err = s.DB.QueryRow(ctx, `SELECT enabled FROM notification_event_node_settings WHERE event_key=$1 AND node_id=$2`, key, nodeID).Scan(&enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	return enabled, err
}

// SetNodeEnabled stores a per-node override for a node-scoped event.
func (s *EventStore) SetNodeEnabled(ctx context.Context, key, nodeID string, enabled bool) error {
	view, err := s.GetEvent(ctx, key)
	if err != nil {
		return err
	}
	if !view.NodeScoped {
		return ErrInvalidEvent
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO notification_event_node_settings(event_key,node_id,enabled,updated_at)
		VALUES($1,$2,$3,now())
		ON CONFLICT(event_key,node_id) DO UPDATE SET enabled=EXCLUDED.enabled,updated_at=now()`, key, nodeID, enabled)
	return err
}

// ResolveBody returns the template body and effective severity/channels for delivery.
func (s *EventStore) ResolveBody(ctx context.Context, key, channel string) (body, severity string, channels []string, err error) {
	view, err := s.GetEvent(ctx, key)
	if err != nil {
		return "", "", nil, err
	}
	tpl, err := s.GetTemplate(ctx, key, channel)
	if err != nil {
		return "", "", nil, err
	}
	return tpl.Body, view.Severity, slices.Clone(view.Channels), nil
}

type eventOverride struct {
	Enabled  bool
	Severity string
	Channels []string
	Present  bool
}

func (s *EventStore) loadSettings(ctx context.Context) (map[string]eventOverride, error) {
	rows, err := s.DB.Query(ctx, `SELECT event_key,enabled,severity,channels FROM notification_event_settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]eventOverride{}
	for rows.Next() {
		var key, severity string
		var enabled bool
		var channels []string
		if err := rows.Scan(&key, &enabled, &severity, &channels); err != nil {
			return nil, err
		}
		out[key] = eventOverride{Enabled: enabled, Severity: severity, Channels: channels, Present: true}
	}
	return out, rows.Err()
}

func mergeEvent(base catalog.Event, o eventOverride) EventView {
	view := EventView{
		Key: base.Key, Category: base.Category, Title: base.Title, Description: base.Description,
		Enabled: base.Enabled, Severity: base.Severity, Channels: slices.Clone(base.Channels),
		NodeScoped: base.NodeScoped, EngineScoped: base.EngineScoped,
		Placeholders: slices.Clone(base.Placeholders),
	}
	if o.Present {
		view.Enabled, view.Severity, view.Channels = o.Enabled, o.Severity, slices.Clone(o.Channels)
		view.Customized = true
	}
	return view
}

func validateEventWrite(severity string, channels []string) error {
	if severity != "info" && severity != "warn" && severity != "crit" {
		return ErrInvalidEvent
	}
	if len(channels) < 1 || len(channels) > len(catalog.Channels()) {
		return ErrInvalidEvent
	}
	seen := map[string]bool{}
	for _, ch := range channels {
		if _, ok := catalog.LookupChannel(ch); !ok || seen[ch] {
			return ErrInvalidEvent
		}
		seen[ch] = true
	}
	return nil
}

func templateLookupError(key string) (TemplateView, error) {
	if !catalog.ValidEventKey(key) {
		return TemplateView{}, ErrEventNotFound
	}
	return TemplateView{}, ErrTemplateNotFound
}
