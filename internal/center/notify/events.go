package notify

import (
	"context"
	"encoding/json"
	"errors"
	"math"
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
	Key                 string             `json:"key"`
	Category            string             `json:"category"`
	Title               string             `json:"title"`
	Description         string             `json:"description"`
	Enabled             bool               `json:"enabled"`
	Severity            string             `json:"severity"`
	Channels            []string           `json:"channels"`
	NodeScoped          bool               `json:"nodeScoped"`
	EngineScoped        bool               `json:"engineScoped"`
	AlertMetric         string             `json:"alertMetric,omitempty"`
	Thresholds          map[string]float64 `json:"thresholds"`
	CooldownSeconds     int                `json:"cooldownSeconds"`
	RepeatSeconds       int                `json:"repeatSeconds"`
	MaintenanceStart    string             `json:"maintenanceStart"`
	MaintenanceEnd      string             `json:"maintenanceEnd"`
	MaintenanceTimezone string             `json:"maintenanceTimezone"`
	ISPScope            []string           `json:"ispScopes"`
	Placeholders        []string           `json:"placeholders"`
	Customized          bool               `json:"customized"`
}

// EventPatch updates owner-level event settings.
type EventPatch struct {
	Enabled             *bool
	Severity            *string
	Channels            []string
	Thresholds          map[string]float64
	CooldownSeconds     *int
	RepeatSeconds       *int
	MaintenanceStart    *string
	MaintenanceEnd      *string
	MaintenanceTimezone *string
	ISPScope            []string
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
	args, err := prepareEventWrite(base, patch)
	if err != nil {
		return EventView{}, err
	}
	return s.persistEventPatch(ctx, key, base, args)
}

type eventWriteArgs struct{ values []any }

func prepareEventWrite(base catalog.Event, patch EventPatch) (eventWriteArgs, error) {
	severity, channels := base.Severity, slices.Clone(base.Channels)
	if patch.Severity != nil {
		severity = strings.TrimSpace(*patch.Severity)
	}
	if patch.Channels != nil {
		channels = slices.Clone(patch.Channels)
	}
	if err := validateEventWrite(severity, channels); err != nil {
		return eventWriteArgs{}, err
	}
	if err := validateRuntimePatch(base, patch); err != nil {
		return eventWriteArgs{}, err
	}
	var values [7]any
	if patch.Enabled != nil {
		values[0] = *patch.Enabled
	}
	if patch.Severity != nil {
		values[2] = severity
	}
	if patch.Channels != nil {
		values[4] = channels
	}
	if patch.Thresholds != nil {
		raw, err := json.Marshal(patch.Thresholds)
		if err != nil {
			return eventWriteArgs{}, ErrInvalidEvent
		}
		values[5] = raw
	}
	if patch.CooldownSeconds != nil {
		values[6] = *patch.CooldownSeconds
	}
	return eventWriteArgs{values: valuesToSQL(base, patch, values)}, nil
}

func valuesToSQL(base catalog.Event, patch EventPatch, values [7]any) []any {
	var repeatArg, startArg, endArg, zoneArg, scopesArg any
	if patch.RepeatSeconds != nil {
		repeatArg = *patch.RepeatSeconds
	}
	if patch.MaintenanceStart != nil {
		startArg = strings.TrimSpace(*patch.MaintenanceStart)
	}
	if patch.MaintenanceEnd != nil {
		endArg = strings.TrimSpace(*patch.MaintenanceEnd)
	}
	if patch.MaintenanceTimezone != nil {
		zoneArg = strings.TrimSpace(*patch.MaintenanceTimezone)
		if zoneArg == "" {
			zoneArg = "UTC"
		}
	}
	if patch.ISPScope != nil {
		scopesArg = normalizeScopes(patch.ISPScope)
	}
	return []any{base.Key, values[0], base.Enabled, values[2], base.Severity, values[4], base.Channels,
		values[5], values[6], repeatArg, startArg, endArg, zoneArg, scopesArg}
}

func normalizeScopes(scopes []string) []string {
	out := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		out = append(out, strings.TrimSpace(scope))
	}
	return out
}

const updateEventSettingsSQL = `INSERT INTO notification_event_settings(event_key,enabled,severity,channels,thresholds,cooldown_seconds,repeat_seconds,
	maintenance_start,maintenance_end,maintenance_timezone,isp_scopes,updated_at)
	VALUES($1,COALESCE($2::boolean,$3),COALESCE($4::text,$5),COALESCE($6::text[],$7::text[]),COALESCE($8::jsonb,'{}'::jsonb),
	COALESCE($9::integer,0),COALESCE($10::integer,0),NULLIF($11::text,'')::time,NULLIF($12::text,'')::time,
	COALESCE($13::text,'UTC'),COALESCE($14::text[],'{}'::text[]),now())
	ON CONFLICT(event_key) DO UPDATE SET enabled=COALESCE($2::boolean,notification_event_settings.enabled),
	severity=COALESCE($4::text,notification_event_settings.severity),channels=COALESCE($6::text[],notification_event_settings.channels),
	thresholds=COALESCE($8::jsonb,notification_event_settings.thresholds),cooldown_seconds=COALESCE($9::integer,notification_event_settings.cooldown_seconds),
	repeat_seconds=COALESCE($10::integer,notification_event_settings.repeat_seconds),
	maintenance_start=CASE WHEN $11::text IS NULL THEN notification_event_settings.maintenance_start ELSE NULLIF($11::text,'')::time END,
	maintenance_end=CASE WHEN $12::text IS NULL THEN notification_event_settings.maintenance_end ELSE NULLIF($12::text,'')::time END,
	maintenance_timezone=COALESCE($13::text,notification_event_settings.maintenance_timezone),isp_scopes=COALESCE($14::text[],notification_event_settings.isp_scopes),updated_at=now()
	RETURNING enabled,severity,channels,thresholds,cooldown_seconds,repeat_seconds,
	COALESCE(to_char(maintenance_start,'HH24:MI'),''),COALESCE(to_char(maintenance_end,'HH24:MI'),''),maintenance_timezone,isp_scopes`

func (s *EventStore) persistEventPatch(ctx context.Context, key string, base catalog.Event, args eventWriteArgs) (EventView, error) {
	var enabled bool
	var storedSeverity string
	var storedChannels []string
	var storedThresholds []byte
	var cooldown, repeat int
	var maintenanceStart, maintenanceEnd, timezone string
	var scopes []string
	err := s.DB.QueryRow(ctx, updateEventSettingsSQL, args.values...).
		Scan(&enabled, &storedSeverity, &storedChannels, &storedThresholds, &cooldown, &repeat, &maintenanceStart, &maintenanceEnd, &timezone, &scopes)
	if err != nil {
		return EventView{}, err
	}
	var thresholds map[string]float64
	if err := json.Unmarshal(storedThresholds, &thresholds); err != nil {
		return EventView{}, err
	}
	return mergeEvent(base, eventOverride{Enabled: enabled, Severity: storedSeverity, Channels: storedChannels,
		Thresholds: thresholds, CooldownSeconds: cooldown, RepeatSeconds: repeat, MaintenanceStart: maintenanceStart,
		MaintenanceEnd: maintenanceEnd, MaintenanceTimezone: timezone, ISPScope: scopes, Present: true}), nil
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
	if err := template.ValidateForChannel(key, channel, body); err != nil {
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
	if err := template.ValidateForChannel(key, channel, body); err != nil {
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
	Enabled             bool
	Severity            string
	Channels            []string
	Thresholds          map[string]float64
	CooldownSeconds     int
	RepeatSeconds       int
	MaintenanceStart    string
	MaintenanceEnd      string
	MaintenanceTimezone string
	ISPScope            []string
	Present             bool
}

func (s *EventStore) loadSettings(ctx context.Context) (map[string]eventOverride, error) {
	rows, err := s.DB.Query(ctx, `SELECT event_key,enabled,severity,channels,thresholds,cooldown_seconds,repeat_seconds,
		COALESCE(to_char(maintenance_start,'HH24:MI'),''),COALESCE(to_char(maintenance_end,'HH24:MI'),''),maintenance_timezone,isp_scopes
		FROM notification_event_settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]eventOverride{}
	for rows.Next() {
		var key, severity string
		var enabled bool
		var channels, scopes []string
		var thresholdsRaw []byte
		var cooldown, repeat int
		var maintenanceStart, maintenanceEnd, timezone string
		if err := rows.Scan(&key, &enabled, &severity, &channels, &thresholdsRaw, &cooldown, &repeat, &maintenanceStart, &maintenanceEnd, &timezone, &scopes); err != nil {
			return nil, err
		}
		var thresholds map[string]float64
		if err := json.Unmarshal(thresholdsRaw, &thresholds); err != nil {
			return nil, err
		}
		out[key] = eventOverride{Enabled: enabled, Severity: severity, Channels: channels, Thresholds: thresholds,
			CooldownSeconds: cooldown, RepeatSeconds: repeat, MaintenanceStart: maintenanceStart,
			MaintenanceEnd: maintenanceEnd, MaintenanceTimezone: timezone, ISPScope: scopes, Present: true}
	}
	return out, rows.Err()
}

func mergeEvent(base catalog.Event, o eventOverride) EventView {
	view := EventView{
		Key: base.Key, Category: base.Category, Title: base.Title, Description: base.Description,
		Enabled: base.Enabled, Severity: base.Severity, Channels: slices.Clone(base.Channels),
		NodeScoped: base.NodeScoped, EngineScoped: base.EngineScoped,
		AlertMetric: base.AlertMetric, Thresholds: cloneThresholds(base.Thresholds), MaintenanceTimezone: "UTC",
		CooldownSeconds: base.CooldownSeconds, RepeatSeconds: base.RepeatSeconds,
		ISPScope: []string{}, Placeholders: slices.Clone(base.Placeholders),
	}
	if o.Present {
		view.Enabled, view.Severity, view.Channels = o.Enabled, o.Severity, slices.Clone(o.Channels)
		for key, value := range o.Thresholds {
			view.Thresholds[key] = value
		}
		view.CooldownSeconds, view.RepeatSeconds = o.CooldownSeconds, o.RepeatSeconds
		view.MaintenanceStart, view.MaintenanceEnd, view.MaintenanceTimezone = o.MaintenanceStart, o.MaintenanceEnd, o.MaintenanceTimezone
		view.ISPScope = append([]string{}, o.ISPScope...)
		view.Customized = true
	}
	return view
}

func cloneThresholds(in map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func validateRuntimePatch(event catalog.Event, patch EventPatch) error {
	if patch.Thresholds != nil {
		for key, value := range patch.Thresholds {
			if !allowedThreshold(event, key) || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 ||
				(strings.HasSuffix(key, "_pct") && value > 100) ||
				((key == "for_tests" || key == "attempts") && (value < 1 || value > 100 || math.Trunc(value) != value)) {
				return ErrInvalidEvent
			}
		}
	}
	if patch.CooldownSeconds != nil && (*patch.CooldownSeconds < 0 || *patch.CooldownSeconds > 604800) {
		return ErrInvalidEvent
	}
	if patch.RepeatSeconds != nil && (*patch.RepeatSeconds < 0 || *patch.RepeatSeconds > 604800) {
		return ErrInvalidEvent
	}
	if (patch.MaintenanceStart == nil) != (patch.MaintenanceEnd == nil) {
		return ErrInvalidEvent
	}
	if patch.MaintenanceStart != nil {
		start, end := strings.TrimSpace(*patch.MaintenanceStart), strings.TrimSpace(*patch.MaintenanceEnd)
		if (start == "") != (end == "") {
			return ErrInvalidEvent
		}
		if start != "" {
			if _, err := time.Parse("15:04", start); err != nil {
				return ErrInvalidEvent
			}
			if _, err := time.Parse("15:04", end); err != nil {
				return ErrInvalidEvent
			}
		}
	}
	zone := ""
	if patch.MaintenanceTimezone != nil {
		zone = strings.TrimSpace(*patch.MaintenanceTimezone)
	}
	if zone != "" {
		if _, err := time.LoadLocation(zone); err != nil {
			return ErrInvalidEvent
		}
	}
	if patch.ISPScope != nil {
		if len(patch.ISPScope) > 100 {
			return ErrInvalidEvent
		}
		seen := map[string]bool{}
		for _, value := range patch.ISPScope {
			value = strings.TrimSpace(value)
			if value == "" || len(value) > 120 || seen[strings.ToLower(value)] {
				return ErrInvalidEvent
			}
			seen[strings.ToLower(value)] = true
		}
	}
	return nil
}

func allowedThreshold(event catalog.Event, key string) bool {
	if _, ok := event.Thresholds[key]; ok {
		return true
	}
	if event.AlertMetric == "" {
		return false
	}
	if key == event.AlertMetric {
		return true
	}
	return key == "for_tests" && (event.AlertMetric == "failed" || event.Category == "quality")
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
