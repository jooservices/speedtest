package alerts

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jooservices/speedtest/internal/center/agentapi"
)

type fakeTx struct {
	settings map[string]bool
	channels map[string][]string
	nodes    map[string]bool
	nodeName string
	limits   [][2]any
	fail     string
	inserted int
}

func (f *fakeTx) Begin(context.Context) (pgx.Tx, error) { return f, nil }
func (f *fakeTx) Commit(context.Context) error          { return nil }
func (f *fakeTx) Rollback(context.Context) error        { return nil }
func (f *fakeTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, nil
}
func (f *fakeTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults { return nil }
func (f *fakeTx) LargeObjects() pgx.LargeObjects                         { return pgx.LargeObjects{} }
func (f *fakeTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	return nil, nil
}
func (f *fakeTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (f *fakeTx) Conn() *pgx.Conn { return nil }

func (f *fakeTx) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	if f.fail == "limits" && strings.Contains(sql, "alert_rules") {
		return nil, errors.New("limits")
	}
	rows := make([][]any, 0, len(f.limits))
	for _, row := range f.limits {
		rows = append(rows, []any{row[0], row[1]})
	}
	return &alertRows{rows: rows}, nil
}

func (f *fakeTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "FROM settings"):
		if f.fail == "settings-missing" {
			return alertRow{err: pgx.ErrNoRows}
		}
		if f.fail == "settings" {
			return alertRow{err: errors.New("settings")}
		}
		raw, _ := json.Marshal(map[string]string{"centerUrl": "https://center.example"})
		return alertRow{vals: []any{raw}}
	case strings.Contains(sql, "notification_event_settings") && strings.Contains(sql, "channels"):
		if f.fail == "channels" {
			return alertRow{err: errors.New("channels")}
		}
		ch, ok := f.channels[args[0].(string)]
		if !ok {
			return alertRow{err: pgx.ErrNoRows}
		}
		return alertRow{vals: []any{ch}}
	case strings.Contains(sql, "notification_event_settings"):
		if f.fail == "enabled" {
			return alertRow{err: errors.New("enabled")}
		}
		v, ok := f.settings[args[0].(string)]
		if !ok {
			return alertRow{err: pgx.ErrNoRows}
		}
		return alertRow{vals: []any{v}}
	case strings.Contains(sql, "notification_event_node_settings"):
		v, ok := f.nodes[args[0].(string)+"|"+args[1].(string)]
		if !ok {
			return alertRow{err: pgx.ErrNoRows}
		}
		return alertRow{vals: []any{v}}
	case strings.Contains(sql, "FROM nodes") && strings.Contains(sql, "expected_down"):
		if f.fail == "node" {
			return alertRow{err: errors.New("node")}
		}
		return alertRow{vals: []any{f.nodeName, 300.0, 100.0}}
	case strings.Contains(sql, "FROM nodes"):
		if f.fail == "node" {
			return alertRow{err: errors.New("node")}
		}
		return alertRow{vals: []any{f.nodeName}}
	case strings.Contains(sql, "INSERT INTO alert_events"):
		if f.fail == "insert" {
			return alertRow{err: errors.New("insert")}
		}
		f.inserted++
		return alertRow{vals: []any{"evt-1"}}
	case strings.Contains(sql, "FROM results"):
		if f.fail == "results" {
			return alertRow{err: errors.New("results")}
		}
		down, up, ping := 200.0, 80.0, 10.0
		return alertRow{vals: []any{"rid", "2026-11-08T01:00:00Z", "ok", &down, &up, &ping, (*float64)(nil), (*float64)(nil), (*string)(nil), false}}
	}
	return alertRow{err: errors.New("unexpected: " + sql)}
}

type alertRow struct {
	vals []any
	err  error
}

func (r alertRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i := range dest {
		switch d := dest[i].(type) {
		case *string:
			*d = r.vals[i].(string)
		case *bool:
			*d = r.vals[i].(bool)
		case *float64:
			*d = r.vals[i].(float64)
		case **float64:
			*d = r.vals[i].(*float64)
		case **string:
			*d = r.vals[i].(*string)
		case *[]byte:
			*d = r.vals[i].([]byte)
		case *[]string:
			*d = r.vals[i].([]string)
		case *time.Time:
			*d = r.vals[i].(time.Time)
		}
	}
	return nil
}

type alertRows struct {
	rows [][]any
	i    int
}

func (r *alertRows) Close()                                       {}
func (r *alertRows) Err() error                                   { return nil }
func (r *alertRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *alertRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *alertRows) Next() bool {
	if r.i >= len(r.rows) {
		return false
	}
	r.i++
	return true
}
func (r *alertRows) Scan(dest ...any) error {
	vals := r.rows[r.i-1]
	for i := range dest {
		switch d := dest[i].(type) {
		case *string:
			*d = vals[i].(string)
		case *float64:
			*d = vals[i].(float64)
		}
	}
	return nil
}
func (r *alertRows) Values() ([]any, error) { return nil, nil }
func (r *alertRows) RawValues() [][]byte    { return nil }
func (r *alertRows) Conn() *pgx.Conn        { return nil }
func (r *alertRows) TypeMap() *pgtype.Map   { return nil }

func newEmitFixture() (*Store, *fakeTx, context.Context) {
	tx := &fakeTx{
		nodeName: "Fiber",
		settings: map[string]bool{"test.success": true, "test.manual_finished": true, "test.recovered": true, "node.online": true, "quality.recovered": true},
		channels: map[string][]string{"test.success": {"telegram"}, "empty": {}},
		limits:   [][2]any{{"down_pct", 65.0}, {"ping", 30.0}, {"up_pct", 40.0}, {"jitter", 10.0}, {"loss", 2.0}},
	}
	return &Store{Random: rand.Reader}, tx, context.Background()
}

func TestEmitHelpersAndOutcomes(t *testing.T) {
	store, tx, ctx := newEmitFixture()
	if got, err := store.centerURL(ctx, tx); err != nil || got != "https://center.example" {
		t.Fatalf("centerURL %q %v", got, err)
	}
	limits, err := store.qualityLimits(ctx, tx, "n1")
	if err != nil || limits.DownloadPct != 65 || limits.PingMS != 30 {
		t.Fatalf("limits %+v %v", limits, err)
	}
	tx.fail = "limits"
	if _, err := store.qualityLimits(ctx, tx, "n1"); err == nil {
		t.Fatal("expected limits failure")
	}
	tx.fail = ""

	ch, err := store.eventChannels(ctx, tx, "test.success", []string{"email"})
	if err != nil || len(ch) == 0 || ch[0] != "telegram" {
		t.Fatalf("channels %v %v", ch, err)
	}
	ch, err = store.eventChannels(ctx, tx, "quality.recovered", []string{"email"})
	if err != nil || ch[0] != "telegram" {
		t.Fatalf("catalog default %v %v", ch, err)
	}
	ch, err = store.eventChannels(ctx, tx, "empty", []string{"slack"})
	if err != nil || ch[0] != "slack" {
		t.Fatalf("empty override %v %v", ch, err)
	}
	ch, err = store.eventChannels(ctx, tx, "unknown.key", []string{"email"})
	if err != nil || ch[0] != "email" {
		t.Fatalf("fallback %v %v", ch, err)
	}

	on, err := store.catalogEnabled(ctx, tx, "test.success", "n1")
	if err != nil || !on {
		t.Fatalf("enabled %v %v", on, err)
	}
	on, err = store.catalogEnabled(ctx, tx, "missing", "n1")
	if err != nil || on {
		t.Fatalf("missing %v %v", on, err)
	}
	tx.nodes = map[string]bool{"test.success|n1": false}
	on, err = store.catalogEnabled(ctx, tx, "test.success", "n1")
	if err != nil || on {
		t.Fatalf("node off %v %v", on, err)
	}
	tx.nodes = nil

	before := tx.inserted
	if _, err := store.insertCatalogEvent(ctx, tx, "test.success", "n1", "info", "T", "D", "", nil, map[string]string{"node": "Fiber"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.insertCatalogEvent(ctx, tx, "test.success", "n1", "info", "T", "D", "rid", []string{"telegram"}, map[string]string{"node": "Fiber"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if tx.inserted != before+2 {
		t.Fatalf("insert count %d", tx.inserted)
	}

	down := 200.0
	result := agentapi.Result{ID: "rid", Status: "ok", Down: &down, TS: "2026-11-08T01:00:00Z"}
	before = tx.inserted
	if err := store.emitTestOutcome(ctx, tx, "n1", result, time.Now().UTC(), 300, 100); err != nil || tx.inserted != before+1 {
		t.Fatalf("success %v inserted=%d", err, tx.inserted-before)
	}
	result.Manual = true
	if err := store.emitTestOutcome(ctx, tx, "n1", result, time.Now().UTC(), 300, 100); err != nil {
		t.Fatal(err)
	}
	if err := store.emitTestOutcome(ctx, tx, "n1", agentapi.Result{Status: "failed"}, time.Now().UTC(), 300, 100); err != nil {
		t.Fatal(err)
	}
	tx.settings["test.success"] = false
	if err := store.emitTestOutcome(ctx, tx, "n1", result, time.Now().UTC(), 300, 100); err != nil {
		t.Fatal(err)
	}
	tx.settings["test.success"] = true

	before = tx.inserted
	if err := store.emitRecovery(ctx, tx, Rule{Metric: "failed", Channels: []string{"telegram"}}, "n1", time.Now().Add(-time.Hour), time.Now(), "rid"); err != nil {
		t.Fatal(err)
	}
	if err := store.emitRecovery(ctx, tx, Rule{Metric: "offline", Channels: []string{"telegram"}}, "n1", time.Now().Add(-time.Minute), time.Now(), ""); err != nil {
		t.Fatal(err)
	}
	if err := store.emitRecovery(ctx, tx, Rule{Metric: "ip_changed"}, "n1", time.Now(), time.Now(), ""); err != nil {
		t.Fatal(err)
	}
	tx.settings["test.recovered"] = false
	if err := store.emitRecovery(ctx, tx, Rule{Metric: "failed", Channels: []string{"telegram"}}, "n1", time.Now(), time.Now(), ""); err != nil {
		t.Fatal(err)
	}
	if tx.inserted < before+2 {
		t.Fatalf("recovery inserts %d", tx.inserted-before)
	}
	if formatDuration(-30*time.Second) == "" || formatDuration(2*time.Hour) == "" {
		t.Fatal("duration")
	}
	if _, err := loadResult(ctx, tx, "rid"); err != nil {
		t.Fatal(err)
	}

	tx.fail = "insert"
	if _, err := store.insertCatalogEvent(ctx, tx, "test.success", "n1", "info", "T", "D", "rid", []string{"telegram"}, map[string]string{}, time.Now().UTC()); err == nil {
		t.Fatal("expected insert failure")
	}
	tx.fail = "channels"
	if _, err := store.insertCatalogEvent(ctx, tx, "test.success", "n1", "info", "T", "D", "", nil, map[string]string{}, time.Now().UTC()); err == nil {
		t.Fatal("expected channels failure")
	}
	tx.fail = "node"
	if err := store.emitTestOutcome(ctx, tx, "n1", agentapi.Result{ID: "rid", Status: "ok", Down: &down}, time.Now().UTC(), 300, 100); err == nil {
		t.Fatal("expected node failure")
	}
	tx.settings["test.success"] = true
	tx.settings["test.recovered"] = true
	tx.settings["node.online"] = true
	tx.fail = "settings-missing"
	if got, err := store.centerURL(ctx, tx); err != nil || got != "" {
		t.Fatalf("missing display settings: %q %v", got, err)
	}
	tx.fail = "settings"
	if _, err := store.centerURL(ctx, tx); err == nil {
		t.Fatal("expected settings failure")
	}
	tx.fail = "enabled"
	if err := store.emitTestOutcome(ctx, tx, "n1", agentapi.Result{ID: "rid", Status: "ok", Down: &down}, time.Now().UTC(), 300, 100); err == nil {
		t.Fatal("expected enabled failure")
	}
	tx.fail = "limits"
	if err := store.emitTestOutcome(ctx, tx, "n1", agentapi.Result{ID: "rid", Status: "ok", Down: &down}, time.Now().UTC(), 300, 100); err == nil {
		t.Fatal("expected limits failure on success")
	}
	if err := store.emitRecovery(ctx, tx, Rule{Metric: "failed", Channels: []string{"telegram"}}, "n1", time.Now(), time.Now(), ""); err == nil {
		t.Fatal("expected limits failure on recovery")
	}
	tx.fail = "results"
	if err := store.emitRecovery(ctx, tx, Rule{Metric: "failed", Channels: []string{"telegram"}}, "n1", time.Now(), time.Now(), "rid"); err == nil {
		t.Fatal("expected results failure on recovery")
	}
	tx.fail = "settings"
	if err := store.emitTestOutcome(ctx, tx, "n1", agentapi.Result{ID: "rid", Status: "ok", Down: &down}, time.Now().UTC(), 300, 100); err == nil {
		t.Fatal("expected center URL failure on success")
	}
	if err := store.emitRecovery(ctx, tx, Rule{Metric: "failed", Channels: []string{"telegram"}}, "n1", time.Now(), time.Now(), ""); err == nil {
		t.Fatal("expected center URL failure on recovery")
	}
	tx.fail = "channels"
	if err := store.emitTestOutcome(ctx, tx, "n1", agentapi.Result{ID: "rid", Status: "ok", Down: &down}, time.Now().UTC(), 300, 100); err == nil {
		t.Fatal("expected channels failure on success")
	}
	if err := store.emitRecovery(ctx, tx, Rule{Metric: "failed", Channels: []string{"telegram"}}, "n1", time.Now(), time.Now(), ""); err == nil {
		t.Fatal("expected channels failure on recovery")
	}
	tx.fail = "node"
	if err := store.emitRecovery(ctx, tx, Rule{Metric: "failed", Channels: []string{"telegram"}}, "n1", time.Now(), time.Now(), ""); err == nil {
		t.Fatal("expected node failure on recovery")
	}
	tx.fail = ""
}
