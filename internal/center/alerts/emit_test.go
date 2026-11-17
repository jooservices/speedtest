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
	settings             map[string]bool
	channels             map[string][]string
	nodes                map[string]bool
	nodeName             string
	limits               [][2]any
	medianScans          int
	medianConfigReads    int
	serverReads          int
	ispReads             int
	bandwidthConfigReads int
	bandwidthUsageReads  int
	alertSettingsReads   int
	combinedEventWrites  int
	alertStateUpserts    int
	alertStateReads      int
	lastStateWrite       string
	fail                 string
	inserted             int
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
func (f *fakeTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "INSERT INTO alert_state") {
		f.alertStateUpserts++
		f.lastStateWrite = sql
	}
	if strings.Contains(sql, "WITH created AS") || strings.Contains(sql, "INSERT INTO alert_events") {
		if f.fail == "insert" || f.fail == "outbox" {
			return pgconn.CommandTag{}, errors.New(f.fail)
		}
		f.inserted++
	}
	if strings.Contains(sql, "WITH inserted AS") && strings.Contains(sql, "INSERT INTO alert_notification_outbox") {
		f.combinedEventWrites++
	}
	return pgconn.CommandTag{}, nil
}
func (f *fakeTx) Conn() *pgx.Conn { return nil }

func (f *fakeTx) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "alert_event_settings_batch") {
		f.alertSettingsReads++
		return &alertRows{}, nil
	}
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
	if strings.Contains(sql, "FROM alert_state") {
		f.alertStateReads++
	}
	if row, ok := f.bandwidthBudgetRow(sql); ok {
		return row
	}
	f.countQueryRow(sql, args)
	// The groups keep the original first-match order: a query is answered by
	// the first group whose pattern matches it.
	for _, group := range []func(string, []any) (pgx.Row, bool){
		f.queryConfigRow,
		f.querySettingRow,
		f.queryNotificationRow,
		f.queryNodeResultRow,
	} {
		if row, ok := group(sql, args); ok {
			return row
		}
	}
	return alertRow{err: errors.New("unexpected: " + sql)}
}

// bandwidthBudgetRow answers the bandwidth budget flag read.
func (f *fakeTx) bandwidthBudgetRow(sql string) (pgx.Row, bool) {
	if !strings.Contains(sql, "bandwidth_budget_configured") {
		return nil, false
	}
	f.bandwidthConfigReads++
	return alertRow{vals: []any{false}}, true
}

// countQueryRow records the reads the tests assert on.
func (f *fakeTx) countQueryRow(sql string, args []any) {
	if strings.Contains(sql, "FROM results r JOIN nodes n") {
		f.bandwidthUsageReads++
	}
	if strings.Contains(sql, "percentile_cont") {
		f.medianScans++
	}
	if strings.Contains(sql, "engine_isp FROM results") {
		f.ispReads++
	}
	if strings.Contains(sql, "SELECT server_id FROM nodes") {
		f.serverReads++
	}
	if strings.Contains(sql, "SELECT enabled,severity,channels,thresholds") && len(args) > 0 && args[0] == "quality.drop_vs_median" {
		f.medianConfigReads++
	}
}

// queryConfigRow answers test outcome configuration and timezone reads.
func (f *fakeTx) queryConfigRow(sql string, _ []any) (pgx.Row, bool) {
	switch {
	case strings.Contains(sql, "test_outcome_config"):
		if f.fail == "node" || f.fail == "settings" {
			return alertRow{err: errors.New(f.fail)}, true
		}
		centerURL := "https://center.example"
		if f.fail == "settings-missing" {
			centerURL = ""
		}
		return alertRow{vals: []any{f.nodeName, 300.0, 100.0, centerURL}}, true
	case strings.Contains(sql, "SELECT COALESCE(value #>> '{}' ,'UTC')"):
		if f.fail == "timezone" {
			return alertRow{err: errors.New("timezone")}, true
		}
		return alertRow{vals: []any{"UTC"}}, true
	case strings.Contains(sql, "SELECT COALESCE(value #>> '{}','UTC')"):
		if f.fail == "timezone" {
			return alertRow{err: errors.New("timezone")}, true
		}
		return alertRow{vals: []any{"UTC"}}, true
	}
	return nil, false
}

// querySettingRow answers event settings, quiet-hours timezone and center settings reads.
func (f *fakeTx) querySettingRow(sql string, args []any) (pgx.Row, bool) {
	switch {
	case strings.Contains(sql, "SELECT enabled,severity,channels,thresholds"):
		if f.fail == "enabled" || f.fail == "channels" {
			return alertRow{err: errors.New(f.fail)}, true
		}
		key := args[0].(string)
		if _, ok := f.settings[key]; !ok {
			return alertRow{err: pgx.ErrNoRows}, true
		}
		severity := "warn"
		channels := f.channels[key]
		return alertRow{vals: []any{f.settings[key], severity, channels, []byte(`{}`), 0, 0, "", "", "UTC", []string{}}}, true
	case strings.Contains(sql, "notifyQuietTimezone"):
		if f.fail == "timezone" {
			return alertRow{err: errors.New("timezone")}, true
		}
		return alertRow{vals: []any{"Asia/Bangkok"}}, true
	case strings.Contains(sql, "FROM settings"):
		if f.fail == "settings-missing" {
			return alertRow{err: pgx.ErrNoRows}, true
		}
		if f.fail == "settings" {
			return alertRow{err: errors.New("settings")}, true
		}
		raw, _ := json.Marshal(map[string]string{"centerUrl": "https://center.example"})
		return alertRow{vals: []any{raw}}, true
	}
	return nil, false
}

// queryNotificationRow answers notification event and node toggle reads.
func (f *fakeTx) queryNotificationRow(sql string, args []any) (pgx.Row, bool) {
	switch {
	case strings.Contains(sql, "notification_event_settings") && strings.Contains(sql, "channels"):
		if f.fail == "channels" {
			return alertRow{err: errors.New("channels")}, true
		}
		ch, ok := f.channels[args[0].(string)]
		if !ok {
			return alertRow{err: pgx.ErrNoRows}, true
		}
		return alertRow{vals: []any{ch}}, true
	case strings.Contains(sql, "notification_event_settings"):
		if f.fail == "enabled" {
			return alertRow{err: errors.New("enabled")}, true
		}
		v, ok := f.settings[args[0].(string)]
		if !ok {
			return alertRow{err: pgx.ErrNoRows}, true
		}
		return alertRow{vals: []any{v}}, true
	case strings.Contains(sql, "notification_event_node_settings"):
		v, ok := f.nodes[args[0].(string)+"|"+args[1].(string)]
		if !ok {
			return alertRow{err: pgx.ErrNoRows}, true
		}
		return alertRow{vals: []any{v}}, true
	}
	return nil, false
}

// queryNodeResultRow answers node metadata and latest result reads.
func (f *fakeTx) queryNodeResultRow(sql string, args []any) (pgx.Row, bool) {
	switch {
	case strings.Contains(sql, "FROM nodes") && strings.Contains(sql, "expected_down"):
		if f.fail == "node" {
			return alertRow{err: errors.New("node")}, true
		}
		return alertRow{vals: []any{f.nodeName, 300.0, 100.0}}, true
	case strings.Contains(sql, "SELECT server_id FROM nodes"):
		pinned := 7
		return alertRow{vals: []any{&pinned}}, true
	case strings.Contains(sql, "FROM nodes"):
		if f.fail == "node" {
			return alertRow{err: errors.New("node")}, true
		}
		return alertRow{vals: []any{f.nodeName}}, true
	case strings.Contains(sql, "FROM results"):
		if f.fail == "results" {
			return alertRow{err: errors.New("results")}, true
		}
		down, up, ping := 200.0, 80.0, 10.0
		return alertRow{vals: []any{"rid", "2026-11-08T01:00:00Z", "ok", &down, &up, &ping, (*float64)(nil), (*float64)(nil), (*string)(nil), false, []byte(`{"name":"HCMC-1"}`), (*string)(nil), (*string)(nil), "other", "ookla"}}, true
	}
	return nil, false
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
		case *int:
			*d = r.vals[i].(int)
		case **float64:
			*d = r.vals[i].(*float64)
		case **string:
			*d = r.vals[i].(*string)
		case **int:
			*d = r.vals[i].(*int)
		case *[]byte:
			*d = r.vals[i].([]byte)
		case *[]string:
			*d = r.vals[i].([]string)
		case *time.Time:
			*d = r.vals[i].(time.Time)
		case **time.Time:
			v, _ := r.vals[i].(*time.Time)
			*d = v
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
	testEmitURLAndLimits(t, store, tx, ctx)
	testEmitChannels(t, store, tx, ctx)
	testEmitCatalogEnabled(t, store, tx, ctx)
	testEmitCatalogInserts(t, store, tx, ctx)
	testEmitTestOutcomes(t, store, tx, ctx)
	testEmitRecovery(t, store, tx, ctx)
	testEmitFailures(t, store, tx, ctx)
}

func testEmitURLAndLimits(t *testing.T, store *Store, tx *fakeTx, ctx context.Context) {
	assertEmitCenterURL(t, store, tx, ctx)
	assertInitialQualityLimits(t, store, tx, ctx)
	assertConfiguredQualityLimits(t, store, tx, ctx)
	assertDefaultQualityLimits(t, store, tx, ctx)
	assertQualityLimitFailure(t, store, tx, ctx)
}

func assertEmitCenterURL(t *testing.T, store *Store, tx *fakeTx, ctx context.Context) {
	t.Helper()
	if got, err := store.centerURL(ctx, tx); err != nil || got != "https://center.example" {
		t.Fatalf("centerURL %q %v", got, err)
	}
}

func assertInitialQualityLimits(t *testing.T, store *Store, tx *fakeTx, ctx context.Context) {
	t.Helper()
	limits, err := store.qualityLimits(ctx, tx, "n1", "ookla")
	if err != nil || limits.DownloadPct != 65 || limits.PingMS != 30 || !limits.DownloadSet || !limits.PingSet {
		t.Fatalf("limits %+v %v", limits, err)
	}
}

func assertConfiguredQualityLimits(t *testing.T, store *Store, tx *fakeTx, ctx context.Context) {
	t.Helper()
	tx.limits = [][2]any{{"down_pct", 72.0}}
	limits, err := store.qualityLimits(ctx, tx, "n1", "ookla")
	if err != nil || limits.DownloadPct != 72 || limits.UploadPct != 40 || limits.PingMS != 30 {
		t.Fatalf("configured and fallback limits %+v %v", limits, err)
	}
}

func assertDefaultQualityLimits(t *testing.T, store *Store, tx *fakeTx, ctx context.Context) {
	t.Helper()
	tx.limits = nil
	limits, err := store.qualityLimits(ctx, tx, "n1", "ookla")
	if err != nil || limits.DownloadPct != 65 || limits.UploadPct != 40 || limits.LossPct != 2 {
		t.Fatalf("default limits %+v %v", limits, err)
	}
}

func assertQualityLimitFailure(t *testing.T, store *Store, tx *fakeTx, ctx context.Context) {
	t.Helper()
	tx.limits = [][2]any{{"down_pct", 65.0}, {"ping", 30.0}, {"up_pct", 40.0}, {"jitter", 10.0}, {"loss", 2.0}}
	tx.fail = "limits"
	if _, err := store.qualityLimits(ctx, tx, "n1", "ookla"); err == nil {
		t.Fatal("expected limits failure")
	}
	tx.fail = ""
}

func testEmitChannels(t *testing.T, store *Store, tx *fakeTx, ctx context.Context) {
	t.Helper()
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
}

func testEmitCatalogEnabled(t *testing.T, store *Store, tx *fakeTx, ctx context.Context) {
	t.Helper()
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
}

func testEmitCatalogInserts(t *testing.T, store *Store, tx *fakeTx, ctx context.Context) {
	t.Helper()
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
}

func testEmitTestOutcomes(t *testing.T, store *Store, tx *fakeTx, ctx context.Context) {
	t.Helper()
	down := 200.0
	result := agentapi.Result{ID: "rid", Status: "ok", Down: &down, TS: "2026-11-08T01:00:00Z"}
	before := tx.inserted
	if err := store.emitTestOutcome(ctx, tx, "n1", result, time.Now().UTC()); err != nil || tx.inserted != before+1 {
		t.Fatalf("success %v inserted=%d", err, tx.inserted-before)
	}
	result.Manual = true
	if err := store.emitTestOutcome(ctx, tx, "n1", result, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := store.emitTestOutcome(ctx, tx, "n1", agentapi.Result{Status: "failed"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	tx.settings["test.success"] = false
	if err := store.emitTestOutcome(ctx, tx, "n1", result, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	tx.settings["test.success"] = true
}

func testEmitRecovery(t *testing.T, store *Store, tx *fakeTx, ctx context.Context) {
	t.Helper()
	before := tx.inserted
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
}

func testEmitFailures(t *testing.T, store *Store, tx *fakeTx, ctx context.Context) {
	t.Helper()
	down := 200.0
	tx.settings["test.success"] = true
	tx.settings["test.recovered"] = true
	tx.settings["node.online"] = true
	testEmitInsertFailures(t, store, tx, ctx)
	testEmitOutcomeFailures(t, store, tx, ctx, down)
	testEmitRecoveryLimitFailures(t, store, tx, ctx)
	testEmitRecoveryConfigurationFailures(t, store, tx, ctx, down)
}

func testEmitInsertFailures(t *testing.T, store *Store, tx *fakeTx, ctx context.Context) {
	t.Helper()
	tx.fail = "insert"
	if _, err := store.insertCatalogEvent(ctx, tx, "test.success", "n1", "info", "T", "D", "rid", []string{"telegram"}, map[string]string{}, time.Now().UTC()); err == nil {
		t.Fatal("expected insert failure")
	}
	tx.fail = "outbox"
	before := tx.inserted
	if _, err := store.insertCatalogEvent(ctx, tx, "test.success", "n1", "info", "T", "D", "rid", []string{"telegram"}, map[string]string{}, time.Now().UTC()); err == nil || tx.inserted != before {
		t.Fatal("expected atomic event/outbox failure")
	}
	tx.fail = "channels"
	if _, err := store.insertCatalogEvent(ctx, tx, "test.success", "n1", "info", "T", "D", "", nil, map[string]string{}, time.Now().UTC()); err == nil {
		t.Fatal("expected channels failure")
	}
}

func testEmitOutcomeFailures(t *testing.T, store *Store, tx *fakeTx, ctx context.Context, down float64) {
	t.Helper()
	tx.fail = "node"
	if err := store.emitTestOutcome(ctx, tx, "n1", agentapi.Result{ID: "rid", Status: "ok", Down: &down}, time.Now().UTC()); err == nil {
		t.Fatal("expected node failure")
	}
	tx.fail = "settings-missing"
	if got, err := store.centerURL(ctx, tx); err != nil || got != "" {
		t.Fatalf("missing display settings: %q %v", got, err)
	}
	tx.fail = "settings"
	if _, err := store.centerURL(ctx, tx); err == nil {
		t.Fatal("expected settings failure")
	}
	tx.fail = "enabled"
	if err := store.emitTestOutcome(ctx, tx, "n1", agentapi.Result{ID: "rid", Status: "ok", Down: &down}, time.Now().UTC()); err == nil {
		t.Fatal("expected enabled failure")
	}
	tx.fail = "limits"
	if err := store.emitTestOutcome(ctx, tx, "n1", agentapi.Result{ID: "rid", Status: "ok", Down: &down}, time.Now().UTC()); err == nil {
		t.Fatal("expected limits failure on success")
	}
}

func testEmitRecoveryLimitFailures(t *testing.T, store *Store, tx *fakeTx, ctx context.Context) {
	t.Helper()
	tx.fail = "limits"
	if err := store.emitRecovery(ctx, tx, Rule{Metric: "failed", Channels: []string{"telegram"}}, "n1", time.Now(), time.Now(), ""); err == nil {
		t.Fatal("expected limits failure on recovery")
	}
	tx.fail = "results"
	if err := store.emitRecovery(ctx, tx, Rule{Metric: "failed", Channels: []string{"telegram"}}, "n1", time.Now(), time.Now(), "rid"); err == nil {
		t.Fatal("expected results failure on recovery")
	}
}

func testEmitRecoveryConfigurationFailures(t *testing.T, store *Store, tx *fakeTx, ctx context.Context, down float64) {
	t.Helper()
	tx.fail = "settings"
	if err := store.emitTestOutcome(ctx, tx, "n1", agentapi.Result{ID: "rid", Status: "ok", Down: &down}, time.Now().UTC()); err == nil {
		t.Fatal("expected center URL failure on success")
	}
	if err := store.emitRecovery(ctx, tx, Rule{Metric: "failed", Channels: []string{"telegram"}}, "n1", time.Now(), time.Now(), ""); err == nil {
		t.Fatal("expected center URL failure on recovery")
	}
	tx.fail = "channels"
	if err := store.emitTestOutcome(ctx, tx, "n1", agentapi.Result{ID: "rid", Status: "ok", Down: &down}, time.Now().UTC()); err == nil {
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

func TestCatalogEventAndOutboxUseOneAtomicWrite(t *testing.T) {
	store, tx, ctx := newEmitFixture()
	settings := effectiveEventSettings{Enabled: true, Severity: "info", Channels: []string{"telegram"}}
	if _, err := store.insertCatalogEventWithSettings(ctx, tx, "test.success", "n1", "Success", "Done", "result", map[string]string{}, time.Now(), settings); err != nil {
		t.Fatal(err)
	}
	if tx.inserted != 1 || tx.combinedEventWrites != 1 {
		t.Fatalf("event and outbox writes = %d events, %d combined statements", tx.inserted, tx.combinedEventWrites)
	}
	tx.fail = "outbox"
	if _, err := store.insertCatalogEventWithSettings(ctx, tx, "test.success", "n1", "Success", "Done", "result", map[string]string{}, time.Now(), settings); err == nil {
		t.Fatal("expected atomic event/outbox write to fail")
	}
}

func TestUnobservedMetricAdvancesStateWithOneMonotonicUpsert(t *testing.T) {
	store, tx, ctx := newEmitFixture()
	if err := store.advanceRule(ctx, tx, Rule{ID: "rule-1"}, "node-1", time.Now(), "", false, false); err != nil {
		t.Fatal(err)
	}
	if tx.alertStateUpserts != 1 || tx.alertStateReads != 0 {
		t.Fatalf("unobserved state writes/reads = %d/%d, want one upsert and no lock read", tx.alertStateUpserts, tx.alertStateReads)
	}
	if !strings.Contains(tx.lastStateWrite, "ON CONFLICT") || !strings.Contains(tx.lastStateWrite, "GREATEST(") {
		t.Fatalf("unobserved timestamp update is not monotonic: %s", tx.lastStateWrite)
	}
}

func TestRuleEvaluationBatchSkipsAlreadyEvaluatedTimestamps(t *testing.T) {
	batch := &ruleEvaluationBatch{}
	first := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	if !batch.accept("rule-1", first) || batch.accept("rule-1", first) || batch.accept("rule-1", first.Add(-time.Second)) {
		t.Fatal("equal or older rule timestamp was accepted")
	}
	if !batch.accept("rule-1", first.Add(time.Second)) || !batch.accept("rule-2", first) {
		t.Fatal("newer timestamp or independent rule was rejected")
	}
}

func TestDisabledMedianDropSkipsHistoryScan(t *testing.T) {
	store, tx, ctx := newEmitFixture()
	down, up := 100.0, 50.0
	result := agentapi.Result{Status: "ok", Down: &down, Up: &up}
	if err := store.emitMedianDrop(ctx, tx, "n1", result, time.Now()); err != nil {
		t.Fatal(err)
	}
	if tx.medianScans != 0 {
		t.Fatalf("disabled median event scanned history %d times", tx.medianScans)
	}
}

func TestPrepareObservedBatchCachesStableSettings(t *testing.T) {
	store, tx, ctx := newEmitFixture()
	store.DB = tx
	down, up, serverID := 100.0, 50.0, 7
	isp := "Example ISP"
	result := agentapi.Result{ID: "00000000-0000-4000-8000-000000000001", Status: "ok", Down: &down, Up: &up, ServerID: &serverID, EngineISP: &isp}
	prepared, err := store.PrepareResultBatch(ctx, "n1", []agentapi.Result{result, result})
	if err != nil {
		t.Fatal(err)
	}
	if tx.alertSettingsReads != 1 {
		t.Fatalf("alert event settings read %d times for one batch", tx.alertSettingsReads)
	}
	state, ok := prepared.Value(observedBatchContextKey{}).(*observedBatchState)
	if !ok {
		t.Fatalf("observed batch state was not prepared: %#v", prepared.Value(observedBatchContextKey{}))
	}
	median, hasMedian := state.medianSettings["ookla"]
	if !state.pinnedLoaded || state.pinnedServerID == nil || *state.pinnedServerID != serverID || !hasMedian || median.Enabled {
		t.Fatalf("observed batch settings = %+v", state)
	}
	if tx.serverReads != 1 || tx.medianConfigReads != 1 {
		t.Fatalf("stable event settings read counts: server=%d median=%d", tx.serverReads, tx.medianConfigReads)
	}
}

func TestObservedBatchCachesOrderedISPAndPinnedServer(t *testing.T) {
	store, tx, ctx := newEmitFixture()
	first := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	state := &observedBatchState{
		lastISP:      map[string]observedISP{"ookla": {ts: first, id: "a", value: "Example ISP"}},
		pinnedLoaded: true, pinnedServerID: intPointer(7),
		medianSettings: map[string]effectiveEventSettings{"ookla": {Enabled: false}},
	}
	ctx = context.WithValue(ctx, observedBatchContextKey{}, state)
	isp, serverID := "Example ISP", 7
	result := agentapi.Result{ID: "b", Status: "ok", Down: floatPointer(100), Up: floatPointer(50), EngineISP: &isp, ServerID: &serverID}
	if err := store.emitISPChange(ctx, tx, "n1", result, first.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.emitServerMismatch(ctx, tx, "n1", result, first.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.emitMedianDrop(ctx, tx, "n1", result, first.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if tx.ispReads != 0 || tx.serverReads != 0 || tx.medianConfigReads != 0 || tx.medianScans != 0 {
		t.Fatalf("cached event paths repeated reads: isp=%d server=%d medianConfig=%d median=%d", tx.ispReads, tx.serverReads, tx.medianConfigReads, tx.medianScans)
	}
}

func intPointer(value int) *int { return &value }

func floatPointer(value float64) *float64 { return &value }

func TestResultBefore(t *testing.T) {
	base := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		leftTS, rightTS time.Time
		leftID, rightID string
		want            bool
	}{
		{leftTS: base, rightTS: base.Add(time.Second), leftID: "b", rightID: "a", want: true},
		{leftTS: base, rightTS: base, leftID: "a", rightID: "b", want: true},
		{leftTS: base, rightTS: base, leftID: "b", rightID: "a"},
		{leftTS: base.Add(time.Second), rightTS: base, leftID: "a", rightID: "b"},
	}
	for _, tc := range tests {
		if got := resultBefore(tc.leftTS, tc.leftID, tc.rightTS, tc.rightID); got != tc.want {
			t.Fatalf("resultBefore() = %t, want %t", got, tc.want)
		}
	}
}

func TestRememberISPKeepsNewestResult(t *testing.T) {
	state := &observedBatchState{lastISP: make(map[string]observedISP)}
	newer := time.Date(2026, 11, 10, 11, 0, 0, 0, time.UTC)
	older := newer.Add(-time.Minute)
	rememberISP(state, "ookla", newer, "b", "New ISP")
	rememberISP(state, "ookla", older, "a", "Old ISP")
	if got := state.lastISP["ookla"]; got.value != "New ISP" || got.id != "b" {
		t.Fatalf("older ISP replaced the latest result: %+v", got)
	}
	rememberISP(state, "ookla", newer.Add(time.Minute), "c", "Newest ISP")
	if got := state.lastISP["ookla"]; got.value != "Newest ISP" || got.id != "c" {
		t.Fatalf("newest ISP was not kept: %+v", got)
	}
	rememberISP(nil, "ookla", newer, "d", "No batch")
}

func TestPrepareTestOutcomeBatch(t *testing.T) {
	store, tx, ctx := newEmitFixture()
	store.DB = tx
	down := 200.0
	result := agentapi.Result{ID: "rid", Status: "ok", Down: &down}
	prepared, err := store.PrepareResultBatch(ctx, "n1", []agentapi.Result{result, result})
	if err != nil {
		t.Fatal(err)
	}
	configs, ok := prepared.Value(testOutcomeBatchKey{}).(map[string]testOutcomeConfig)
	if !ok || len(configs) != 1 || !configs["test.success|ookla"].Enabled || configs["test.success|ookla"].ExpectedDown != 300 {
		t.Fatalf("prepared config = %+v", configs)
	}
	// The batch snapshot keeps event settings stable and avoids re-reading each result.
	tx.fail = "enabled"
	if err := store.emitTestOutcome(prepared, tx, "n1", result, time.Now().UTC()); err != nil || tx.inserted != 1 {
		t.Fatalf("cached event config = %v, inserted=%d", err, tx.inserted)
	}
	tx.fail = ""
	tx.settings["test.success"] = false
	tx.fail = "node"
	prepared, err = store.PrepareResultBatch(ctx, "n1", []agentapi.Result{result})
	if err != nil {
		t.Fatal(err)
	}
	if tx.alertSettingsReads != 2 {
		t.Fatalf("alert event settings batch reads = %d after two requests, want 2", tx.alertSettingsReads)
	}
	tx.fail = ""
	if err := store.emitTestOutcome(prepared, tx, "n1", result, time.Now().UTC()); err != nil || tx.inserted != 1 {
		t.Fatalf("disabled batch emitted event: %v, inserted=%d", err, tx.inserted)
	}
}

func TestOwnerTimezonePropagatesDatabaseFailure(t *testing.T) {
	_, tx, ctx := newEmitFixture()
	tx.fail = "timezone"
	if zone, err := ownerTimezone(ctx, tx); err == nil || zone != "" {
		t.Fatalf("timezone failure returned %q, %v", zone, err)
	}
}
