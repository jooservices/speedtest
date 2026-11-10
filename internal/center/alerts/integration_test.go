//go:build integration

package alerts_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/agentapi"
	"github.com/jooservices/speedtest/internal/center/alerts"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/geo"
	"github.com/jooservices/speedtest/internal/center/ipwatch"
	"github.com/jooservices/speedtest/internal/shared/config"
)

const (
	testNodeID = "10000000-0000-4000-8000-000000000001"
	appKey     = "0123456789abcdef0123456789abcdef" // gitleaks:allow (fixed test-only key)
)

func openIntegrationDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	path, err := filepath.Abs("../../../.env")
	if err != nil {
		t.Fatal("worktree environment path is invalid")
	}
	values, err := godotenv.Read(path)
	if err != nil {
		t.Fatal("worktree .env is required")
	}
	for key, value := range values {
		t.Setenv(key, value)
	}
	cfg, err := config.LoadCenter()
	if err != nil || db.CheckTestDatabase(cfg.Database.Name) != nil {
		t.Fatal("integration tests require a configured worktree test database")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal("cannot connect to the configured worktree database")
	}
	t.Cleanup(pool.Close)
	var name, marker string
	if err := pool.QueryRow(ctx, `SELECT current_database(),shobj_description(oid,'pg_database') FROM pg_database WHERE datname=current_database()`).Scan(&name, &marker); err != nil || name != cfg.Database.Name || marker != "workspaces-project:speedtest" {
		t.Fatal("worktree database identity/ownership marker verification failed")
	}
	t.Log("PASS: worktree test database identity and ownership marker verified")
	return pool, ctx
}

func integrationDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool, ctx := openIntegrationDB(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	return pool, ctx
}

func TestWorktreeDatabaseIdentityIntegration(t *testing.T) {
	openIntegrationDB(t)
}

func addNode(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id, name string, created time.Time, interval int) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps,created_at)
		VALUES ($1,$2,100,50,$3)`, id, name, created); err != nil {
		t.Fatal("create integration node")
	}
	if _, err := pool.Exec(ctx, `WITH legacy AS (INSERT INTO schedules(node_id,interval_minutes,offset_minutes,enabled) VALUES ($1,$2,0,true) RETURNING *) INSERT INTO engine_schedules SELECT node_id,'ookla',interval_minutes,offset_minutes,enabled,revision FROM legacy`, id, interval); err != nil {
		t.Fatal("create integration schedule")
	}
}

func addRule(t *testing.T, ctx context.Context, store *alerts.Store, key string, in alerts.RuleInput) string {
	t.Helper()
	data, replay, err := store.CreateRule(ctx, "owner:alerts-test", key, in)
	if err != nil || replay {
		t.Fatalf("create alert rule: replay=%t err=%v", replay, err)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &created); err != nil || created.ID == "" {
		t.Fatal("alert create response did not contain an ID")
	}
	return created.ID
}

func scalar(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, query, args...).Scan(&count); err != nil {
		t.Fatal("count alert rows")
	}
	return count
}

func saveAlertResult(t *testing.T, ctx context.Context, agent *agentapi.Postgres, result agentapi.Result) {
	t.Helper()
	if duplicate, err := agent.SaveResult(ctx, testNodeID, result); err != nil || duplicate {
		t.Fatalf("save alert fixture %s status=%s class=%s: duplicate=%t err=%v", result.ID, result.Status, result.FailureClass, duplicate, err)
	}
}

func assertAlertActive(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ruleID string) {
	t.Helper()
	if scalar(t, ctx, pool, `SELECT count(*) FROM alert_events WHERE rule_id=$1 AND resolved_at IS NULL`, ruleID) != 1 {
		t.Fatalf("rate-limited result changed active alert state for rule %s", ruleID)
	}
}

func TestRateLimitedResultsPreserveMeasurementAlertStateIntegration(t *testing.T) {
	pool, ctx := integrationDB(t)
	addNode(t, ctx, pool, testNodeID, "alert-rate-limit", time.Now().UTC().Add(-time.Hour), 30)
	store := &alerts.Store{DB: pool, AppKey: []byte(appKey)}
	failRule := addRule(t, ctx, store, "rate-fail", alerts.RuleInput{Name: "Failures", Metric: "failed", Op: ">=", Value: 1, ForTests: 2, AllNodes: true, Severity: "warn", Channels: []string{"email"}, Enabled: true})
	pingRule := addRule(t, ctx, store, "rate-ping", alerts.RuleInput{Name: "High ping", Metric: "ping", Op: ">", Value: 50, ForTests: 2, AllNodes: true, Severity: "warn", Channels: []string{"email"}, Enabled: true})
	agent := &agentapi.Postgres{Pool: pool, Hook: store}
	base := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	ip := "203.0.113.81"
	for index, class := range []string{"ping", "rate_limited", "ping", "rate_limited"} {
		item := testResult(fmt.Sprintf("73000000-0000-4000-8000-%012x", index+1), base.Add(time.Duration(index)*time.Minute), map[string]string{"ping": "ok", "rate_limited": "failed"}[class], nil, 0)
		item.Engine = "ookla"
		if class == "ping" {
			item = testResult(fmt.Sprintf("73000000-0000-4000-8000-%012x", index+1), base.Add(time.Duration(index)*time.Minute), "ok", &ip, 100)
			*item.Ping = 100
		} else {
			item.FailureClass = "rate_limited"
		}
		saveAlertResult(t, ctx, agent, item)
	}
	assertAlertActive(t, ctx, pool, pingRule)
	for index, class := range []string{"failed", "rate_limited", "failed", "rate_limited"} {
		status := class
		if class == "rate_limited" {
			status = "failed"
		}
		item := testResult(fmt.Sprintf("73000000-0000-4000-8000-%012x", index+5), base.Add(time.Duration(index+4)*time.Minute), status, nil, 0)
		item.Engine = "ookla"
		if class == "rate_limited" {
			item.FailureClass = "rate_limited"
		}
		saveAlertResult(t, ctx, agent, item)
	}
	assertAlertActive(t, ctx, pool, failRule)
	if scalar(t, ctx, pool, `SELECT consecutive_count FROM alert_state WHERE rule_id=$1 AND node_id=$2`, failRule, testNodeID) != 2 {
		t.Fatal("rate-limited failure changed the consecutive failure count")
	}
	t.Log("PASS: intervening and trailing rate-limited results preserve active measurement alerts and streaks")
	recovery := testResult("73000000-0000-4000-8000-000000000009", base.Add(10*time.Minute), "ok", &ip, 100)
	*recovery.Ping = 10
	saveAlertResult(t, ctx, agent, recovery)
	for _, ruleID := range []string{pingRule, failRule} {
		if scalar(t, ctx, pool, `SELECT count(*) FROM alert_events WHERE rule_id=$1 AND resolved_at IS NOT NULL`, ruleID) != 1 {
			t.Fatalf("normal recovery result did not resolve rule %s", ruleID)
		}
	}
	t.Log("PASS: normal recovery results still resolve active measurement alerts")
}

func testResult(id string, ts time.Time, status string, ip *string, down float64) agentapi.Result {
	result := agentapi.Result{ID: id, TS: ts.UTC().Format(time.RFC3339Nano), Status: status, IP: ip,
		Threads: 1, BytesDown: 10, BytesUp: 5, Duration: 1, Raw: []byte(`{}`), Warnings: []string{}}
	if status == "failed" {
		message := "fixture failure"
		result.Error = &message
		return result
	}
	up, ping, serverID := 20.0, 8.0, 1
	result.Down, result.Up, result.Ping = &down, &up, &ping
	result.ServerID = &serverID
	result.Server = &agentapi.Server{ID: &serverID, Name: "Fixture", Host: "fixture.example"}
	return result
}

func TestIncrementalStreakRestartResolutionAndRuleLifecycleIntegration(t *testing.T) {
	pool, ctx := integrationDB(t)
	addNode(t, ctx, pool, testNodeID, "alert-edge", time.Now().UTC().Add(-time.Hour), 30)
	store := &alerts.Store{DB: pool, AppKey: []byte(appKey), Now: func() time.Time { return time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC) }}
	in := alerts.RuleInput{Name: "Two failures", Metric: "failed", Op: ">=", Value: 1, ForTests: 2,
		AllNodes: true, Severity: "crit", Channels: []string{"email", "webhook"}, Enabled: true}
	id := addRule(t, ctx, store, "rule-create-1", in)

	created, replay, err := store.CreateRule(ctx, "owner:alerts-test", "rule-create-1", in)
	if err != nil || !replay {
		t.Fatalf("same create key replay = %t, %v", replay, err)
	}
	var replayed struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(created, &replayed) != nil || replayed.ID != id {
		t.Fatal("create replay returned a different rule")
	}
	changed := in
	changed.Name = "Different request"
	if _, _, err = store.CreateRule(ctx, "owner:alerts-test", "rule-create-1", changed); !errors.Is(err, alerts.ErrConflict) {
		t.Fatalf("same idempotency key with a different body = %v", err)
	}
	if _, _, err = store.CreateRule(ctx, "owner:alerts-test", "missing-node", alerts.RuleInput{Name: "missing", Metric: "ping", Op: ">", Value: 1,
		ForTests: 1, NodeIDs: []string{"20000000-0000-4000-8000-000000000099"}, Severity: "warn", Channels: []string{"email"}, Enabled: true}); !errors.Is(err, alerts.ErrInvalid) {
		t.Fatalf("unknown selected node error = %v", err)
	}

	agent := &agentapi.Postgres{Pool: pool, Hook: store}
	first := testResult("30000000-0000-4000-8000-000000000001", time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC), "failed", nil, 0)
	second := testResult("30000000-0000-4000-8000-000000000002", time.Date(2026, 11, 3, 10, 30, 0, 0, time.UTC), "failed", nil, 0)
	third := testResult("30000000-0000-4000-8000-000000000003", time.Date(2026, 11, 3, 11, 0, 0, 0, time.UTC), "failed", nil, 0)
	for _, item := range []agentapi.Result{first, second} {
		if duplicate, err := agent.SaveResult(ctx, testNodeID, item); err != nil || duplicate {
			t.Fatalf("save failure result: duplicate=%t err=%v", duplicate, err)
		}
	}
	opened := scalar(t, ctx, pool, `SELECT count(*) FROM alert_events WHERE rule_id=$1`, id)
	var stateCount int
	var stateActive bool
	if err := pool.QueryRow(ctx, `SELECT consecutive_count,active FROM alert_state WHERE rule_id=$1 AND node_id=$2`, id, testNodeID).Scan(&stateCount, &stateActive); err != nil {
		t.Fatal("read failed-run streak state:", err)
	}
	if opened != 1 {
		t.Fatalf("two failed results should open one event: events=%d streak=%d active=%t", opened, stateCount, stateActive)
	}
	if duplicate, err := agent.SaveResult(ctx, testNodeID, second); err != nil || !duplicate {
		t.Fatalf("duplicate result replay = %t, %v", duplicate, err)
	}
	// A new store instance represents a center restart; the persisted active event
	// and streak must continue without reopening or duplicating the notification.
	restarted := &alerts.Store{DB: pool, AppKey: []byte(appKey), Now: store.Now}
	agent.Hook = restarted
	if duplicate, err := agent.SaveResult(ctx, testNodeID, third); err != nil || duplicate {
		t.Fatalf("post-restart result = duplicate %t, %v", duplicate, err)
	}
	if scalar(t, ctx, pool, `SELECT count(*) FROM alert_events WHERE rule_id=$1`, id) != 1 || scalar(t, ctx, pool, `SELECT count(*) FROM alert_notification_outbox WHERE event_id IN (SELECT id FROM alert_events WHERE rule_id=$1)`, id) != 1 {
		t.Fatal("restart duplicated an active alert event or its notification handoff")
	}
	knownIP := "203.0.113.21"
	good := testResult("30000000-0000-4000-8000-000000000004", time.Date(2026, 11, 3, 11, 30, 0, 0, time.UTC), "ok", &knownIP, 90)
	if _, err := agent.SaveResult(ctx, testNodeID, good); err != nil {
		t.Fatal("save streak-breaking result:", err)
	}
	events, total, err := restarted.ListEvents(ctx, alerts.EventFilter{State: "all"}, 1, 25)
	if err != nil || total != 1 || events[0].ResolvedAt == nil {
		t.Fatalf("streak-breaking result did not resolve the event: total=%d events=%+v err=%v", total, events, err)
	}
	event, etag, err := restarted.GetEvent(ctx, events[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	acked, newETag, err := restarted.Acknowledge(ctx, event.ID, true, etag)
	if err != nil || !acked.Acked || newETag == etag {
		t.Fatalf("acknowledge event: acked=%t ETags=%q/%q err=%v", acked.Acked, etag, newETag, err)
	}
	if _, _, err = restarted.Acknowledge(ctx, event.ID, false, etag); !errors.Is(err, alerts.ErrStale) {
		t.Fatalf("stale acknowledgement error = %v", err)
	}
	if _, _, err = restarted.Acknowledge(ctx, event.ID, false, newETag); err != nil {
		t.Fatal("reopen acknowledgement:", err)
	}
	if _, total, err = restarted.ListEvents(ctx, alerts.EventFilter{State: "open"}, 1, 25); err != nil || total != 1 {
		t.Fatalf("reopened event was not in open activity: total=%d err=%v", total, err)
	}
	jobID, err := restarted.AcknowledgeAll(ctx)
	if err != nil || jobID == "" {
		t.Fatalf("acknowledge all: %q %v", jobID, err)
	}
	if scalar(t, ctx, pool, `SELECT count(*) FROM alert_events WHERE acked=false`) != 0 {
		t.Fatal("every event that was open must be acknowledged")
	}
	if scalar(t, ctx, pool, `SELECT count(*) FROM maintenance_jobs WHERE id=$1 AND status='succeeded' AND kind='alerts-acknowledge-all'`, jobID) != 1 {
		t.Fatal("the bulk acknowledgement must be recorded as a succeeded job")
	}
	if again, err := restarted.AcknowledgeAll(ctx); err != nil || again == jobID {
		t.Fatalf("a second bulk acknowledgement is a new, harmless job: %q %v", again, err)
	}
	if after, _, err := restarted.GetEvent(ctx, event.ID); err != nil || !after.Acked {
		t.Fatalf("the reopened event must be acknowledged again: %+v %v", after, err)
	}

	rule, ruleETag, err := restarted.GetRule(ctx, id)
	if err != nil || rule.Revision != 1 {
		t.Fatalf("get rule revision: %+v etag=%q err=%v", rule, ruleETag, err)
	}
	replacement := in
	replacement.Name = "Updated failures"
	if _, _, err = restarted.ReplaceRule(ctx, id, replacement, `"stale"`); !errors.Is(err, alerts.ErrStale) {
		t.Fatalf("stale rule replacement error = %v", err)
	}
	updated, updatedETag, err := restarted.ReplaceRule(ctx, id, replacement, ruleETag)
	if err != nil || updated.Revision != 2 || updatedETag == ruleETag {
		t.Fatalf("replace rule: revision=%d etag=%q/%q err=%v", updated.Revision, ruleETag, updatedETag, err)
	}
	if err = restarted.DeleteRule(ctx, id, ruleETag); !errors.Is(err, alerts.ErrStale) {
		t.Fatalf("stale rule deletion error = %v", err)
	}
	if err = restarted.DeleteRule(ctx, id, updatedETag); err != nil {
		t.Fatal("delete rule:", err)
	}
	if scalar(t, ctx, pool, `SELECT count(*) FROM alert_events WHERE id=$1 AND rule_id IS NULL`, event.ID) != 1 {
		t.Fatal("deleting a rule did not preserve its event history")
	}
	if rows, count, err := restarted.ListRules(ctx, 1, 25); err != nil || count != 0 || len(rows) != 0 {
		t.Fatalf("deleted rule remains visible: count=%d rows=%d err=%v", count, len(rows), err)
	}
}

type fixedGeo struct{}

func (fixedGeo) Lookup(context.Context, string) (geo.Location, error) {
	return geo.Location{City: "Fixture city", Country: "ZZ"}, nil
}

func TestIPSignalDeduplicationAndRestartDrainIntegration(t *testing.T) {
	pool, ctx := integrationDB(t)
	addNode(t, ctx, pool, testNodeID, "ip-edge", time.Now().UTC().Add(-time.Hour), 30)
	store := &alerts.Store{DB: pool, AppKey: []byte(appKey)}
	ruleID := addRule(t, ctx, store, "ip-rule", alerts.RuleInput{Name: "IP changed", Metric: "ip_changed", Op: "=", Value: 1,
		ForTests: 1, AllNodes: true, Severity: "info", Channels: []string{"email"}, Enabled: true})
	watcher := &ipwatch.Watcher{Offline: fixedGeo{}}
	agent := &agentapi.Postgres{Pool: pool, Hook: agentapi.ResultHookChain{watcher, store}}
	timestamp := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	for i, address := range []string{"203.0.113.1", "203.0.113.1", "203.0.113.2"} {
		ip := address
		item := testResult(fmt.Sprintf("31000000-0000-4000-8000-%012x", i+1), timestamp.Add(time.Duration(i)*30*time.Minute), "ok", &ip, 90)
		if duplicate, err := agent.SaveResult(ctx, testNodeID, item); err != nil || duplicate {
			t.Fatalf("save IP result: duplicate=%t err=%v", duplicate, err)
		}
	}
	if scalar(t, ctx, pool, `SELECT count(*) FROM alert_events WHERE rule_id=$1`, ruleID) != 1 || scalar(t, ctx, pool, `SELECT count(*) FROM ip_change_signals WHERE consumed_at IS NULL`) != 0 {
		t.Fatal("newest IP change was not turned into exactly one event and consumed signal")
	}
	latePath := &agentapi.Postgres{Pool: pool, Hook: watcher}
	lateIP := "203.0.113.3"
	late := testResult("31000000-0000-4000-8000-000000000004", timestamp.Add(90*time.Minute), "ok", &lateIP, 80)
	if _, err := latePath.SaveResult(ctx, testNodeID, late); err != nil {
		t.Fatal("save pending IP signal:", err)
	}
	if scalar(t, ctx, pool, `SELECT count(*) FROM ip_change_signals WHERE consumed_at IS NULL`) != 1 {
		t.Fatal("test did not leave one durable signal for restart recovery")
	}
	restarted := &alerts.Store{DB: pool, AppKey: []byte(appKey)}
	if drained, err := restarted.DrainSignals(ctx); err != nil || drained != 1 {
		t.Fatalf("restart signal drain = %d, %v", drained, err)
	}
	if scalar(t, ctx, pool, `SELECT count(*) FROM alert_events WHERE rule_id=$1`, ruleID) != 2 || scalar(t, ctx, pool, `SELECT count(*) FROM alert_notification_outbox WHERE event_id IN (SELECT id FROM alert_events WHERE rule_id=$1)`, ruleID) != 2 {
		t.Fatal("restart drain did not create exactly one event handoff for the new IP signal")
	}
	if _, err := restarted.DrainSignals(ctx); err != nil {
		t.Fatal("empty repeated signal drain:", err)
	}
	if scalar(t, ctx, pool, `SELECT count(*) FROM alert_events WHERE rule_id=$1`, ruleID) != 2 {
		t.Fatal("repeated drain duplicated an IP event")
	}
}

func TestOfflineRuleUsesThreeScheduleIntervalsIntegration(t *testing.T) {
	pool, ctx := integrationDB(t)
	old := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	addNode(t, ctx, pool, testNodeID, "offline-edge", old, 5)
	now := old.Add(16 * time.Minute)
	store := &alerts.Store{DB: pool, AppKey: []byte(appKey), Now: func() time.Time { return now }}
	ruleID := addRule(t, ctx, store, "offline-rule", alerts.RuleInput{Name: "Node offline", Metric: "offline", Op: "=", Value: 1,
		ForTests: 1, AllNodes: true, Severity: "crit", Channels: []string{"email"}, Enabled: true})
	if err := store.EvaluateOffline(ctx); err != nil {
		t.Fatal("offline check:", err)
	}
	if err := store.EvaluateOffline(ctx); err != nil {
		t.Fatal("repeated offline check:", err)
	}
	if scalar(t, ctx, pool, `SELECT count(*) FROM alert_events WHERE rule_id=$1`, ruleID) != 1 || scalar(t, ctx, pool, `SELECT count(*) FROM alert_notification_outbox WHERE event_id IN (SELECT id FROM alert_events WHERE rule_id=$1)`, ruleID) != 1 {
		t.Fatal("three missed intervals did not open one offline alert and handoff")
	}
	if _, err := pool.Exec(ctx, `UPDATE nodes SET last_seen_at=$2 WHERE id=$1`, testNodeID, now); err != nil {
		t.Fatal("restore node heartbeat")
	}
	now = now.Add(time.Minute)
	if err := store.EvaluateOffline(ctx); err != nil {
		t.Fatal("online recovery check:", err)
	}
	var resolved time.Time
	if err := pool.QueryRow(ctx, `SELECT resolved_at FROM alert_events WHERE rule_id=$1`, ruleID).Scan(&resolved); err != nil || resolved.IsZero() {
		t.Fatalf("offline event did not resolve after heartbeat recovery: %v", err)
	}
}

type previewFixture struct {
	Nodes []struct {
		ID           string  `json:"id"`
		Name         string  `json:"name"`
		ExpectedDown float64 `json:"expectedDown"`
		ExpectedUp   float64 `json:"expectedUp"`
		Interval     int     `json:"interval"`
		Enabled      bool    `json:"enabled"`
	} `json:"nodes"`
	Rules []struct {
		ID       string          `json:"id"`
		Name     string          `json:"name"`
		Metric   string          `json:"metric"`
		Op       string          `json:"op"`
		Value    float64         `json:"value"`
		ForTests int             `json:"forTests"`
		Nodes    json.RawMessage `json:"nodes"`
		Severity string          `json:"severity"`
		Channels []string        `json:"channels"`
		Enabled  bool            `json:"enabled"`
	} `json:"rules"`
	Results []struct {
		NodeID       string   `json:"nodeId"`
		TS           int64    `json:"ts"`
		Status       string   `json:"status"`
		FailureClass string   `json:"failureClass"`
		Down         *float64 `json:"down"`
		Up           *float64 `json:"up"`
		Ping         *float64 `json:"ping"`
		Jitter       *float64 `json:"jitter"`
		Loss         *float64 `json:"loss"`
		IP           *string  `json:"ip"`
		Error        *string  `json:"error"`
	} `json:"results"`
	Events []struct {
		RuleID     string `json:"ruleId"`
		NodeID     string `json:"nodeId"`
		Severity   string `json:"severity"`
		TS         int64  `json:"ts"`
		ResolvedAt *int64 `json:"resolvedAt"`
		Title      string `json:"title"`
		Detail     string `json:"detail"`
		Acked      bool   `json:"acked"`
	} `json:"events"`
	Geos map[string]struct {
		City string `json:"city"`
	} `json:"geos"`
}

const previewScript = `
const fs = require('node:fs');
const vm = require('node:vm');
const fixedNow = Date.UTC(2026, 10, 3, 12, 0, 0, 0);
class FixedDate extends Date { static now() { return fixedNow; } }
const context = {window: {}, location: {search: '?demo=1'}, URLSearchParams, Date: FixedDate, console,
  localStorage: {getItem() { return null; }, setItem() {}, removeItem() {}}};
vm.createContext(context);
for (const file of ['seed.js', 'store.js'])
  vm.runInContext(fs.readFileSync('../../../ui/assets/js/' + file, 'utf8'), context);
const ST = context.window.ST;
const ips = [...new Set(ST.state.results.map(result => result.ip).filter(Boolean))];
const geos = Object.fromEntries(ips.map(ip => [ip, ST.geo(ip)]));
process.stdout.write(JSON.stringify({nodes: ST.nodes(), rules: ST.rules(), results: ST.state.results,
  events: ST.events(), geos}));
`

func loadPreviewFixture(t *testing.T) previewFixture {
	t.Helper()
	command := exec.Command("node", "-e", previewScript)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run approved UI preview fixture: %v: %s", err, output)
	}
	var fixture previewFixture
	if err := json.Unmarshal(output, &fixture); err != nil {
		t.Fatalf("decode UI preview fixture: %v", err)
	}
	return fixture
}

type eventSignature struct {
	RuleID, NodeID, Severity string
	TS                       int64
	ResolvedAt               int64
	HasResolution            bool
	Title, Detail            string
	Acked                    bool
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func TestThirtyDayReplayMatchesApprovedUIPreviewIntegration(t *testing.T) {
	pool, ctx := integrationDB(t)
	fixture := loadPreviewFixture(t)
	if len(fixture.Nodes) != 5 || len(fixture.Results) < 7000 || len(fixture.Events) == 0 {
		t.Fatalf("preview fixture was incomplete: %d nodes, %d results, %d events", len(fixture.Nodes), len(fixture.Results), len(fixture.Events))
	}
	nodeIDs := make(map[string]string, len(fixture.Nodes))
	for i, node := range fixture.Nodes {
		id := fmt.Sprintf("40000000-0000-4000-8000-%012x", i+1)
		nodeIDs[node.ID] = id
		addNode(t, ctx, pool, id, "replay-"+node.Name, time.UnixMilli(fixture.Results[0].TS).UTC().Add(-24*time.Hour), node.Interval)
		if _, err := pool.Exec(ctx, `UPDATE nodes SET expected_down_mbps=$2,expected_up_mbps=$3,enabled=$4 WHERE id=$1`, id, node.ExpectedDown, node.ExpectedUp, node.Enabled); err != nil {
			t.Fatal("set preview node expectations")
		}
		if _, err := pool.Exec(ctx, `UPDATE schedules SET enabled=$2 WHERE node_id=$1`, id, node.Enabled); err != nil {
			t.Fatal("set preview schedule state")
		}
	}
	store := &alerts.Store{DB: pool, AppKey: []byte(appKey)}
	ruleIDs := make(map[string]string, len(fixture.Rules))
	for i, rule := range fixture.Rules {
		input := alerts.RuleInput{Name: rule.Name, Metric: rule.Metric, Op: rule.Op, Value: rule.Value, ForTests: rule.ForTests,
			Severity: rule.Severity, Channels: rule.Channels, Enabled: rule.Enabled}
		if string(rule.Nodes) == `"all"` {
			input.AllNodes = true
		} else {
			var selected []string
			if err := json.Unmarshal(rule.Nodes, &selected); err != nil {
				t.Fatal("decode preview rule scope")
			}
			for _, id := range selected {
				input.NodeIDs = append(input.NodeIDs, nodeIDs[id])
			}
		}
		ruleIDs[rule.ID] = addRule(t, ctx, store, fmt.Sprintf("preview-rule-%d", i), input)
	}
	for ip, location := range fixture.Geos {
		geoJSON, err := json.Marshal(geo.Location{City: location.City, Country: "VN"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO ip_addresses(ip,geo,source,looked_up_at) VALUES ($1,$2::jsonb,'preview-fixture',now()) ON CONFLICT(ip) DO UPDATE SET geo=EXCLUDED.geo,source=EXCLUDED.source,looked_up_at=EXCLUDED.looked_up_at`, ip, geoJSON); err != nil {
			t.Fatal("seed preview geolocation")
		}
	}
	watcher := &ipwatch.Watcher{}
	agent := &agentapi.Postgres{Pool: pool, Hook: agentapi.ResultHookChain{watcher, store}}
	for i, sample := range fixture.Results {
		id := fmt.Sprintf("50000000-0000-4000-8000-%012x", i+1)
		result := agentapi.Result{ID: id, TS: time.UnixMilli(sample.TS).UTC().Format(time.RFC3339Nano), Status: sample.Status,
			FailureClass: sample.FailureClass, Down: sample.Down, Up: sample.Up, Ping: sample.Ping, Jitter: sample.Jitter, Loss: sample.Loss, IP: sample.IP,
			Threads: 1, BytesDown: 1, BytesUp: 1, Duration: 1, Raw: []byte(`{}`), Warnings: []string{}}
		switch {
		case result.Status != "failed":
			serverID := 1
			result.ServerID = &serverID
			result.Server = &agentapi.Server{ID: &serverID, Name: "Preview fixture", Host: "preview.example"}
		case sample.Error != nil:
			result.Error = sample.Error
		default:
			message := "Preview fixture failure"
			result.Error = &message
		}
		if duplicate, err := agent.SaveResult(ctx, nodeIDs[sample.NodeID], result); err != nil || duplicate {
			t.Fatalf("replay result %d: duplicate=%t err=%v", i, duplicate, err)
		}
	}
	var actual []alerts.Event
	for page := 1; ; page++ {
		batch, total, err := store.ListEvents(ctx, alerts.EventFilter{State: "all"}, page, 100)
		if err != nil {
			t.Fatal("list replayed alert events:", err)
		}
		actual = append(actual, batch...)
		if len(actual) >= total {
			break
		}
	}
	actualSet := make([]eventSignature, 0, len(actual))
	for _, event := range actual {
		var ts time.Time
		ts, err := time.Parse(time.RFC3339Nano, event.TS)
		if err != nil {
			t.Fatal(err)
		}
		signature := eventSignature{RuleID: deref(event.RuleID), NodeID: event.NodeID, Severity: event.Severity, TS: ts.UnixMilli(), Title: event.Title, Detail: event.Detail, Acked: event.Acked}
		if event.ResolvedAt != nil {
			resolved, parseErr := time.Parse(time.RFC3339Nano, *event.ResolvedAt)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			signature.ResolvedAt, signature.HasResolution = resolved.UnixMilli(), true
		}
		actualSet = append(actualSet, signature)
	}
	expectedSet := make([]eventSignature, 0, len(fixture.Events))
	for _, event := range fixture.Events {
		signature := eventSignature{RuleID: ruleIDs[event.RuleID], NodeID: nodeIDs[event.NodeID], Severity: event.Severity,
			TS: event.TS, Title: event.Title, Detail: event.Detail, Acked: event.Acked}
		if event.ResolvedAt != nil {
			signature.ResolvedAt, signature.HasResolution = *event.ResolvedAt, true
		}
		expectedSet = append(expectedSet, signature)
	}
	order := func(values []eventSignature) {
		sort.Slice(values, func(i, j int) bool {
			a, b := values[i], values[j]
			if a.RuleID != b.RuleID {
				return a.RuleID < b.RuleID
			}
			if a.NodeID != b.NodeID {
				return a.NodeID < b.NodeID
			}
			return a.TS < b.TS
		})
	}
	order(actualSet)
	order(expectedSet)
	if len(actualSet) != len(expectedSet) {
		t.Fatalf("30-day replay event count differs: center=%d preview=%d", len(actualSet), len(expectedSet))
	}
	for i := range actualSet {
		if actualSet[i] != expectedSet[i] {
			t.Fatalf("30-day replay event %d differs:\ncenter:  %+v\npreview: %+v", i, actualSet[i], expectedSet[i])
		}
	}
	t.Logf("PASS: replayed %d fixture results; center and preview produced %d identical alert events", len(fixture.Results), len(actualSet))
}
