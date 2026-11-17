package notify

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jooservices/speedtest/internal/center/notify/template"
)

type memEventDB struct {
	settings     map[string]eventOverride
	templates    map[string]tmplRow
	nodes        map[string]bool
	fail         error
	failSettings bool
	failTemplate bool
}

type tmplRow struct {
	body string
	at   time.Time
}

func newMemEventDB() *memEventDB {
	return &memEventDB{
		settings:  map[string]eventOverride{},
		templates: map[string]tmplRow{},
		nodes:     map[string]bool{},
	}
}

func (m *memEventDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if m.fail != nil {
		return pgconn.CommandTag{}, m.fail
	}
	switch {
	case strings.Contains(sql, "INSERT INTO notification_event_settings"):
		m.settings[args[0].(string)] = eventOverride{Enabled: args[1].(bool), Severity: args[2].(string), Channels: args[3].([]string), Present: true}
	case strings.Contains(sql, "INSERT INTO notification_templates"):
		m.templates[args[0].(string)+"|"+args[1].(string)] = tmplRow{body: args[2].(string), at: time.Now().UTC()}
	case strings.Contains(sql, "DELETE FROM notification_templates"):
		delete(m.templates, args[0].(string)+"|"+args[1].(string))
	case strings.Contains(sql, "INSERT INTO notification_event_node_settings"):
		m.nodes[args[0].(string)+"|"+args[1].(string)] = args[2].(bool)
	}
	return pgconn.CommandTag{}, nil
}

func (m *memEventDB) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	if m.fail != nil {
		return nil, m.fail
	}
	if strings.Contains(sql, "FROM notification_event_settings") {
		if m.failSettings {
			return nil, errors.New("settings query failed")
		}
		rows := make([][]any, 0, len(m.settings))
		for k, v := range m.settings {
			thresholds, _ := json.Marshal(v.Thresholds)
			rows = append(rows, []any{k, v.Enabled, v.Severity, v.Channels, thresholds, v.CooldownSeconds, v.RepeatSeconds,
				v.MaintenanceStart, v.MaintenanceEnd, v.MaintenanceTimezone, v.ISPScope})
		}
		return &memRows{rows: rows}, nil
	}
	return nil, errors.New("unexpected query")
}

func (m *memEventDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if m.fail != nil {
		return memRow{err: m.fail}
	}
	switch {
	case strings.Contains(sql, "INSERT INTO notification_event_settings"):
		key := args[0].(string)
		current, exists := m.settings[key]
		if !exists {
			current = eventOverride{Enabled: args[2].(bool), Severity: args[4].(string), Channels: args[6].([]string),
				Thresholds: map[string]float64{}, MaintenanceTimezone: "UTC", Present: true}
		}
		if args[1] != nil {
			current.Enabled = args[1].(bool)
		}
		if args[3] != nil {
			current.Severity = args[3].(string)
		}
		if args[5] != nil {
			current.Channels = args[5].([]string)
		}
		if args[7] != nil {
			_ = json.Unmarshal(args[7].([]byte), &current.Thresholds)
		}
		if args[8] != nil {
			current.CooldownSeconds = args[8].(int)
		}
		if args[9] != nil {
			current.RepeatSeconds = args[9].(int)
		}
		if args[10] != nil {
			current.MaintenanceStart = args[10].(string)
		}
		if args[11] != nil {
			current.MaintenanceEnd = args[11].(string)
		}
		if args[12] != nil {
			current.MaintenanceTimezone = args[12].(string)
		}
		if args[13] != nil {
			current.ISPScope = args[13].([]string)
		}
		current.Present = true
		m.settings[key] = current
		thresholds, _ := json.Marshal(current.Thresholds)
		return memRow{vals: []any{current.Enabled, current.Severity, current.Channels, thresholds, current.CooldownSeconds,
			current.RepeatSeconds, current.MaintenanceStart, current.MaintenanceEnd, current.MaintenanceTimezone, current.ISPScope}}
	case strings.Contains(sql, "FROM notification_templates"):
		if m.failTemplate {
			return memRow{err: errors.New("template query failed")}
		}
		row, ok := m.templates[args[0].(string)+"|"+args[1].(string)]
		if !ok {
			return memRow{err: pgx.ErrNoRows}
		}
		return memRow{vals: []any{row.body, row.at}}
	case strings.Contains(sql, "FROM notification_event_node_settings"):
		if m.failTemplate {
			return memRow{err: errors.New("node query failed")}
		}
		v, ok := m.nodes[args[0].(string)+"|"+args[1].(string)]
		if !ok {
			return memRow{err: pgx.ErrNoRows}
		}
		return memRow{vals: []any{v}}
	}
	return memRow{err: errors.New("unexpected row query")}
}

type memRow struct {
	vals []any
	err  error
}

func (r memRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i := range dest {
		switch d := dest[i].(type) {
		case *string:
			*d = r.vals[i].(string)
		case *bool:
			*d = r.vals[i].(bool)
		case *time.Time:
			*d = r.vals[i].(time.Time)
		case *int:
			*d = r.vals[i].(int)
		case *[]string:
			*d = r.vals[i].([]string)
		case *[]byte:
			*d = r.vals[i].([]byte)
		}
	}
	return nil
}

type memRows struct {
	rows [][]any
	i    int
	err  error
}

func (r *memRows) Close()                                       {}
func (r *memRows) Err() error                                   { return r.err }
func (r *memRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *memRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *memRows) Next() bool {
	if r.i >= len(r.rows) {
		return false
	}
	r.i++
	return true
}
func (r *memRows) Scan(dest ...any) error {
	vals := r.rows[r.i-1]
	for i := range dest {
		switch d := dest[i].(type) {
		case *string:
			*d = vals[i].(string)
		case *bool:
			*d = vals[i].(bool)
		case *[]string:
			*d = vals[i].([]string)
		case *[]byte:
			*d = vals[i].([]byte)
		case *int:
			*d = vals[i].(int)
		}
	}
	return nil
}
func (r *memRows) Values() ([]any, error) { return nil, nil }
func (r *memRows) RawValues() [][]byte    { return nil }
func (r *memRows) Conn() *pgx.Conn        { return nil }
func (r *memRows) TypeMap() *pgtype.Map   { return nil }

func TestEventStoreWithMemoryDB(t *testing.T) {
	db := newMemEventDB()
	store := &EventStore{DB: db}
	ctx := context.Background()

	assertEventListAndPatch(t, store, ctx)
	assertEventTemplateLifecycle(t, store, ctx)
	assertNodeEventSettings(t, store, ctx)
	assertMissingTemplateWrites(t, store, ctx)
	assertTemplateAndNodeFailures(t, store, db, ctx)
	assertSettingsReadFailureBehavior(t, store, db, ctx)
	assertMissingRecordsAndValidation(t, store, ctx)
	assertEventStoreDatabaseFailures(t, store, db, ctx)
}

func assertEventListAndPatch(t *testing.T, store *EventStore, ctx context.Context) {
	t.Helper()
	list, err := store.ListEvents(ctx)
	if err != nil || len(list) < 30 {
		t.Fatalf("list %d %v", len(list), err)
	}
	if _, err := store.GetEvent(ctx, "missing"); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("missing: %v", err)
	}
	off := false
	warn := "warn"
	view, err := store.UpdateEvent(ctx, "test.success", EventPatch{Enabled: &off, Severity: &warn, Channels: []string{"email"}})
	if err != nil || view.Enabled || view.Severity != "warn" || !view.Customized {
		t.Fatalf("update %+v %v", view, err)
	}
	if _, err := store.UpdateEvent(ctx, "test.success", EventPatch{Channels: []string{"bad"}}); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("bad channel: %v", err)
	}
}

func assertEventTemplateLifecycle(t *testing.T, store *EventStore, ctx context.Context) {
	assertDefaultAndMissingEventTemplates(t, store, ctx)
	assertTemplateWrites(t, store, ctx)
	assertTemplatePreviewAndReset(t, store, ctx)
}

func assertDefaultAndMissingEventTemplates(t *testing.T, store *EventStore, ctx context.Context) {
	t.Helper()
	tpl, err := store.GetTemplate(ctx, "test.success", "telegram")
	if err != nil || !tpl.IsDefault {
		t.Fatalf("default tpl %+v %v", tpl, err)
	}
	if _, err := store.GetTemplate(ctx, "missing", "email"); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("missing event tpl: %v", err)
	}
	if _, err := store.GetTemplate(ctx, "test.success", "nope"); !errors.Is(err, ErrTemplateNotFound) {
		t.Fatalf("missing channel: %v", err)
	}
}

func assertTemplateWrites(t *testing.T, store *EventStore, ctx context.Context) {
	t.Helper()
	saved, err := store.PutTemplate(ctx, "test.success", "telegram", "Hi {node}")
	if err != nil || saved.IsDefault || saved.Body != "Hi {node}" {
		t.Fatalf("put %+v %v", saved, err)
	}
	if _, err := store.PutTemplate(ctx, "test.success", "telegram", "x {secret}"); !errors.Is(err, template.ErrUnknownPlaceholder) {
		t.Fatalf("placeholder: %v", err)
	}
}

func assertTemplatePreviewAndReset(t *testing.T, store *EventStore, ctx context.Context) {
	t.Helper()
	prev, err := store.PreviewTemplate(ctx, "test.success", "telegram", "")
	if err != nil || !strings.Contains(prev.Body, "Viettel") {
		t.Fatalf("preview %+v %v", prev, err)
	}
	reset, err := store.ResetTemplate(ctx, "test.success", "telegram")
	if err != nil || !reset.IsDefault {
		t.Fatalf("reset %+v %v", reset, err)
	}
}

func assertNodeEventSettings(t *testing.T, store *EventStore, ctx context.Context) {
	assertGlobalEventDisabledForNode(t, store, ctx)
	assertNodeOverrideDisabled(t, store, ctx)
	assertNonScopedNodeEventEnabled(t, store, ctx)
	assertNodeEventScopeGuard(t, store, ctx)
	assertResolvedNodeEventBody(t, store, ctx)
}

func assertGlobalEventDisabledForNode(t *testing.T, store *EventStore, ctx context.Context) {
	t.Helper()
	on, err := store.NodeEnabled(ctx, "test.success", "n1")
	if err != nil || on { // globally disabled
		if err != nil {
			t.Fatal(err)
		}
		if on {
			t.Fatal("expected off")
		}
	}
}

func assertNodeOverrideDisabled(t *testing.T, store *EventStore, ctx context.Context) {
	t.Helper()
	en := true
	if _, err := store.UpdateEvent(ctx, "test.success", EventPatch{Enabled: &en}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetNodeEnabled(ctx, "test.success", "n1", false); err != nil {
		t.Fatal(err)
	}
	on, err := store.NodeEnabled(ctx, "test.success", "n1")
	if err != nil || on {
		t.Fatalf("node %v %v", on, err)
	}
}

func assertNonScopedNodeEventEnabled(t *testing.T, store *EventStore, ctx context.Context) {
	t.Helper()
	on, err := store.NodeEnabled(ctx, "node.offline", "n1")
	if err != nil || !on {
		t.Fatalf("non-scoped %v %v", on, err)
	}
}

func assertNodeEventScopeGuard(t *testing.T, store *EventStore, ctx context.Context) {
	t.Helper()
	if err := store.SetNodeEnabled(ctx, "node.offline", "n1", false); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("scoped guard: %v", err)
	}
}

func assertResolvedNodeEventBody(t *testing.T, store *EventStore, ctx context.Context) {
	t.Helper()
	body, sev, ch, err := store.ResolveBody(ctx, "quality.download_low", "email")
	if err != nil || body == "" || sev == "" || len(ch) == 0 {
		t.Fatalf("resolve %q %q %v %v", body, sev, ch, err)
	}
}

func assertMissingTemplateWrites(t *testing.T, store *EventStore, ctx context.Context) {
	t.Helper()
	if _, err := store.PutTemplate(ctx, "missing", "email", "Hi {node}"); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("put missing event: %v", err)
	}
	if _, err := store.PutTemplate(ctx, "test.success", "nope", "Hi {node}"); !errors.Is(err, ErrTemplateNotFound) {
		t.Fatalf("put missing channel: %v", err)
	}
}

func assertTemplateAndNodeFailures(t *testing.T, store *EventStore, db *memEventDB, ctx context.Context) {
	t.Helper()
	db.failTemplate = true
	if _, _, _, err := store.ResolveBody(ctx, "test.success", "telegram"); err == nil {
		t.Fatal("resolve template failure")
	}
	if _, err := store.NodeEnabled(ctx, "test.success", "n1"); err == nil {
		t.Fatal("node query failure")
	}
	db.failTemplate = false
}

func assertSettingsReadFailureBehavior(t *testing.T, store *EventStore, db *memEventDB, ctx context.Context) {
	t.Helper()
	db.failSettings = true
	en := true
	if _, err := store.UpdateEvent(ctx, "test.success", EventPatch{Enabled: &en}); err != nil {
		t.Fatalf("atomic field patch should not read all settings first: %v", err)
	}
	db.failSettings = false
}

func assertMissingRecordsAndValidation(t *testing.T, store *EventStore, ctx context.Context) {
	t.Helper()
	if _, err := store.ResetTemplate(ctx, "missing", "email"); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("reset missing event: %v", err)
	}
	if _, err := store.ResetTemplate(ctx, "test.success", "nope"); !errors.Is(err, ErrTemplateNotFound) {
		t.Fatalf("reset missing channel: %v", err)
	}
	if _, err := store.PreviewTemplate(ctx, "test.success", "telegram", "bad {secret}"); !errors.Is(err, template.ErrUnknownPlaceholder) {
		t.Fatalf("preview bad: %v", err)
	}
	if _, err := store.PreviewTemplate(ctx, "missing", "email", ""); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("preview missing: %v", err)
	}
	if _, _, _, err := store.ResolveBody(ctx, "missing", "email"); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("resolve missing: %v", err)
	}
	if _, err := store.NodeEnabled(ctx, "missing", "n1"); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("node missing: %v", err)
	}
}

func assertEventStoreDatabaseFailures(t *testing.T, store *EventStore, db *memEventDB, ctx context.Context) {
	t.Helper()
	db.fail = errors.New("db down")
	en := true
	if _, err := store.ListEvents(ctx); err == nil {
		t.Fatal("expected list failure")
	}
	if _, err := store.GetEvent(ctx, "test.success"); err == nil {
		t.Fatal("expected get failure")
	}
	if _, err := store.UpdateEvent(ctx, "test.success", EventPatch{Enabled: &en}); err == nil {
		t.Fatal("expected update failure")
	}
	if _, err := store.GetTemplate(ctx, "test.success", "telegram"); err == nil {
		t.Fatal("expected get template failure")
	}
	if _, err := store.PutTemplate(ctx, "test.success", "telegram", "Hi {node}"); err == nil {
		t.Fatal("expected put failure")
	}
	if _, err := store.ResetTemplate(ctx, "test.success", "telegram"); err == nil {
		t.Fatal("expected reset failure")
	}
	if err := store.SetNodeEnabled(ctx, "test.success", "n1", true); err == nil {
		t.Fatal("expected set node failure")
	}
	if _, _, _, err := store.ResolveBody(ctx, "test.success", "telegram"); err == nil {
		t.Fatal("expected resolve failure")
	}
}
