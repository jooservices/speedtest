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

	"github.com/jooservices/speedtest/internal/shared/testenv"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/agentapi"
	"github.com/jooservices/speedtest/internal/center/alerts"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/geo"
	"github.com/jooservices/speedtest/internal/center/ipwatch"
	"github.com/jooservices/speedtest/internal/center/notify"
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
	testenv.Apply(t, values)
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
		} else {
			item.FailureClass = "timeout"
		}
		saveAlertResult(t, ctx, agent, item)
	}
	assertAlertActive(t, ctx, pool, failRule)
	var failurePayload map[string]string
	if err := pool.QueryRow(ctx, `SELECT payload FROM alert_events WHERE rule_id=$1 AND resolved_at IS NULL`, failRule).Scan(&failurePayload); err != nil || failurePayload["failure_reason"] != "Timeout" {
		t.Fatalf("classified failure payload = %#v %v", failurePayload, err)
	}
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

func TestFailedMeasurementsDoNotRecoverQualityAlertsIntegration(t *testing.T) {
	pool, ctx := integrationDB(t)
	addNode(t, ctx, pool, testNodeID, "quality-failure-edge", time.Now().UTC().Add(-time.Hour), 30)
	store := &alerts.Store{DB: pool, AppKey: []byte(appKey)}
	agent := &agentapi.Postgres{Pool: pool, Hook: store}
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for index := 0; index < 2; index++ {
		item := qualityMeasurements(fmt.Sprintf("74100000-0000-4000-8000-%012x", index+1), base.Add(time.Duration(index)*time.Minute), 40, 10, 40, 15, 3)
		saveAlertResult(t, ctx, agent, item)
	}
	if got := activeQualityRules(t, ctx, pool); got != 5 {
		t.Fatalf("active quality rules = %d, want 5", got)
	}
	failed := testResult("74100000-0000-4000-8000-000000000003", base.Add(2*time.Minute), "failed", nil, 0)
	failed.FailureClass = "timeout"
	saveAlertResult(t, ctx, agent, failed)
	staleFailed := testResult("74100000-0000-4000-8000-000000000006", base.Add(90*time.Second), "failed", nil, 0)
	staleFailed.FailureClass = "timeout"
	saveAlertResult(t, ctx, agent, staleFailed)
	var evaluated time.Time
	if err := pool.QueryRow(ctx, `SELECT s.last_evaluated_at FROM alert_state s JOIN alert_rules r ON r.id=s.rule_id
		WHERE s.node_id=$1 AND r.metric='ping' AND r.node_ids IS NULL AND r.engine IS NULL`, testNodeID).Scan(&evaluated); err != nil || !evaluated.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("unobserved metric timestamp = %s, want monotonic %s: %v", evaluated, base.Add(2*time.Minute), err)
	}
	rateLimited := testResult("74100000-0000-4000-8000-000000000004", base.Add(3*time.Minute), "failed", nil, 0)
	rateLimited.FailureClass = "rate_limited"
	saveAlertResult(t, ctx, agent, rateLimited)
	if got := activeQualityRules(t, ctx, pool); got != 5 {
		t.Fatalf("failed measurements resolved quality rules: %d remain active", got)
	}
	recovery := qualityMeasurements("74100000-0000-4000-8000-000000000005", base.Add(4*time.Minute), 90, 45, 5, 2, 0)
	saveAlertResult(t, ctx, agent, recovery)
	if got := activeQualityRules(t, ctx, pool); got != 0 {
		t.Fatalf("valid recovery measurement left %d quality rules active", got)
	}
	if got := scalar(t, ctx, pool, `SELECT count(*) FROM alert_events WHERE event_key='quality.recovered'`); got != 5 {
		t.Fatalf("quality recovery messages = %d, want 5", got)
	}
}

func TestBandwidthBudgetEventUsesMonthlyResultBytesIntegration(t *testing.T) {
	pool, ctx := integrationDB(t)
	addNode(t, ctx, pool, testNodeID, "bandwidth-budget", time.Now().UTC().Add(-time.Hour), 30)
	store := &alerts.Store{DB: pool, AppKey: []byte(appKey)}
	if _, err := (&notify.EventStore{DB: pool}).UpdateEvent(ctx, "bandwidth.budget_80", notify.EventPatch{
		Thresholds: map[string]float64{"monthly_gb": 100},
	}); err != nil {
		t.Fatal("configure monthly budget:", err)
	}
	agent := &agentapi.Postgres{Pool: pool, Hook: store}
	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	first := qualityMeasurements("74300000-0000-4000-8000-000000000001", base, 90, 45, 8, 2, 0)
	first.BytesDown, first.BytesUp = 70_000_000_000, 0
	saveAlertResult(t, ctx, agent, first)
	second := qualityMeasurements("74300000-0000-4000-8000-000000000002", base.Add(time.Hour), 90, 45, 8, 2, 0)
	second.BytesDown, second.BytesUp = 11_000_000_000, 0
	saveAlertResult(t, ctx, agent, second)
	var payload map[string]string
	if err := pool.QueryRow(ctx, `SELECT payload FROM alert_events WHERE event_key='bandwidth.budget_80'`).Scan(&payload); err != nil {
		t.Fatal("read crossed budget event:", err)
	}
	if payload["value"] != "81" || payload["expected"] != "100" || payload["pct"] != "81" {
		t.Fatalf("bandwidth event payload = %#v", payload)
	}
	if got := scalar(t, ctx, pool, `SELECT count(*) FROM alert_notification_outbox o JOIN alert_events e ON e.id=o.event_id WHERE e.event_key='bandwidth.budget_80'`); got != 1 {
		t.Fatalf("bandwidth delivery count = %d, want 1", got)
	}
}

func TestTestSuccessCooldownSuppressesDeliveryButKeepsHistoryIntegration(t *testing.T) {
	pool, ctx := integrationDB(t)
	addNode(t, ctx, pool, testNodeID, "test-success-cooldown", time.Now().UTC().Add(-time.Hour), 30)
	cooldown := 300
	if _, err := (&notify.EventStore{DB: pool}).UpdateEvent(ctx, "test.success", notify.EventPatch{CooldownSeconds: &cooldown}); err != nil {
		t.Fatal("configure success cooldown:", err)
	}
	store := &alerts.Store{DB: pool, AppKey: []byte(appKey)}
	agent := &agentapi.Postgres{Pool: pool, Hook: store}
	base := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second)
	for index := 0; index < 2; index++ {
		result := qualityMeasurements(fmt.Sprintf("74400000-0000-4000-8000-%012x", index+1), base.Add(time.Duration(index)*time.Minute), 90, 45, 8, 2, 0)
		saveAlertResult(t, ctx, agent, result)
	}
	if events := scalar(t, ctx, pool, `SELECT count(*) FROM alert_events WHERE event_key='test.success'`); events != 2 {
		t.Fatalf("success history rows = %d, want 2", events)
	}
	if deliveries := scalar(t, ctx, pool, `SELECT count(*) FROM alert_notification_outbox o JOIN alert_events e ON e.id=o.event_id WHERE e.event_key='test.success'`); deliveries != 1 {
		t.Fatalf("success deliveries = %d, want cooldown to suppress the second send", deliveries)
	}
}

func qualityMeasurements(id string, ts time.Time, down, up, ping, jitter, loss float64) agentapi.Result {
	ip := "203.0.113.100"
	item := testResult(id, ts, "ok", &ip, down)
	item.Up, item.Ping, item.Jitter, item.Loss = &up, &ping, &jitter, &loss
	return item
}

func activeQualityRules(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	return scalar(t, ctx, pool, `SELECT count(*) FROM alert_state s JOIN alert_rules r ON r.id=s.rule_id
		WHERE s.node_id=$1 AND s.active AND r.metric IN ('down_pct','up_pct','ping','jitter','loss')`, testNodeID)
}

func TestEventThresholdScopesEditsAndEnginePayloadIntegration(t *testing.T) {
	pool, ctx := integrationDB(t)
	addNode(t, ctx, pool, testNodeID, "quality-engine-edge", time.Now().UTC().Add(-time.Hour), 30)
	if _, err := pool.Exec(ctx, `UPDATE nodes SET installed_engines=ARRAY['ookla','librespeed'] WHERE id=$1`, testNodeID); err != nil {
		t.Fatal(err)
	}
	store := &alerts.Store{DB: pool, AppKey: []byte(appKey)}
	settings := &notify.EventStore{DB: pool}
	_, err := settings.UpdateEvent(ctx, "quality.download_low", notify.EventPatch{Thresholds: map[string]float64{"down_pct": 70.5, "for_tests": 3}})
	if err != nil {
		t.Fatal("configure global quality event:", err)
	}
	_, err = settings.UpdateEvent(ctx, "quality.ping_high", notify.EventPatch{Thresholds: map[string]float64{"ping": 10, "for_tests": 4}})
	if err != nil {
		t.Fatal("configure global ping event:", err)
	}
	var downloadRuleID, pingRuleID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM alert_rules WHERE metric='down_pct' AND node_ids IS NULL AND engine IS NULL`).Scan(&downloadRuleID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE alert_rules SET value=200,updated_at=clock_timestamp() WHERE metric='ping' AND node_ids IS NULL AND engine IS NULL`); err != nil {
		t.Fatal(err)
	}
	pingRuleID = addRule(t, ctx, store, "quality-node-ping", alerts.RuleInput{Name: "Node ping", Metric: "ping", Op: ">", Value: 50, ForTests: 1,
		NodeIDs: []string{testNodeID}, Severity: "warn", Channels: []string{"email"}, Enabled: true})
	engineRuleID := addRule(t, ctx, store, "quality-engine-download", alerts.RuleInput{Name: "LibreSpeed download", Metric: "down_pct", Op: "<", Value: 90, ForTests: 1,
		AllNodes: true, Engine: "librespeed", Severity: "warn", Channels: []string{"email"}, Enabled: true})
	addRule(t, ctx, store, "quality-engine-ping", alerts.RuleInput{Name: "LibreSpeed ping", Metric: "ping", Op: ">", Value: 40, ForTests: 1,
		AllNodes: true, Engine: "librespeed", Severity: "warn", Channels: []string{"email"}, Enabled: true})
	addRule(t, ctx, store, "quality-engine-jitter", alerts.RuleInput{Name: "LibreSpeed jitter", Metric: "jitter", Op: ">", Value: 20, ForTests: 1,
		AllNodes: true, Engine: "librespeed", Severity: "warn", Channels: []string{"email"}, Enabled: true})
	addRule(t, ctx, store, "quality-engine-loss", alerts.RuleInput{Name: "LibreSpeed loss", Metric: "loss", Op: ">", Value: 5, ForTests: 1,
		AllNodes: true, Engine: "librespeed", Severity: "warn", Channels: []string{"email"}, Enabled: true})
	agent := &agentapi.Postgres{Pool: pool, Hook: store}
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for index := 0; index < 3; index++ {
		saveAlertResult(t, ctx, agent, qualityMeasurements(fmt.Sprintf("74200000-0000-4000-8000-%012x", index+1), base.Add(time.Duration(index)*time.Minute), 70, 20, 5, 2, 0))
	}
	if scalar(t, ctx, pool, `SELECT count(*) FROM alert_events WHERE rule_id=$1 AND resolved_at IS NULL`, downloadRuleID) != 1 {
		t.Fatal("event threshold override did not require three consecutive measurements")
	}
	var payload map[string]string
	if err := pool.QueryRow(ctx, `SELECT payload FROM alert_events WHERE rule_id=$1 AND resolved_at IS NULL`, downloadRuleID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["threshold"] != "70.5% of expected" || payload["expected"] != "100 Mbps" || payload["value"] != "70 Mbps" {
		t.Fatalf("fractional quality trigger payload = %#v", payload)
	}
	ping := qualityMeasurements("74200000-0000-4000-8000-000000000004", base.Add(3*time.Minute), 90, 45, 60, 2, 0)
	saveAlertResult(t, ctx, agent, ping)
	if err := pool.QueryRow(ctx, `SELECT payload FROM alert_events WHERE rule_id=$1 AND resolved_at IS NULL`, pingRuleID).Scan(&payload); err != nil || payload["threshold"] != "50 ms" {
		t.Fatalf("node-specific threshold was overridden: %#v %v", payload, err)
	}
	libre := qualityMeasurements("74200000-0000-4000-8000-000000000005", base.Add(4*time.Minute), 85, 25, 35, 15, 4)
	libre.Engine = "librespeed"
	saveAlertResult(t, ctx, agent, libre)
	if err := pool.QueryRow(ctx, `SELECT payload FROM alert_events WHERE rule_id=$1`, engineRuleID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["engine"] != "librespeed" || payload["download_limit"] != "90% of expected" || payload["ping_limit"] != "40" || payload["jitter_limit"] != "20" || payload["loss_limit"] != "5" || payload["expected"] != "100 Mbps" {
		t.Fatalf("LibreSpeed event used wrong measurements or limits: %#v", payload)
	}
	zone, _ := time.LoadLocation("Asia/Bangkok")
	wantSince := base.Add(4 * time.Minute).In(zone).Format(time.RFC3339)
	if _, err := time.Parse(time.RFC3339, payload["since"]); err != nil || payload["since"] != wantSince {
		t.Fatalf("quality onset is not an owner-zone timestamp: %q %v", payload["since"], err)
	}
	libreRecovery := qualityMeasurements("74200000-0000-4000-8000-000000000008", base.Add(5*time.Minute), 95, 45, 35, 15, 4)
	libreRecovery.Engine = "librespeed"
	saveAlertResult(t, ctx, agent, libreRecovery)
	if err := pool.QueryRow(ctx, `SELECT payload FROM alert_events WHERE event_key='quality.recovered' AND trigger_result_id=$1`, libreRecovery.ID).Scan(&payload); err != nil {
		t.Fatal("read LibreSpeed recovery payload:", err)
	}
	if payload["engine"] != "librespeed" || payload["download_limit"] != "90% of expected" || payload["ping_limit"] != "40" || payload["jitter_limit"] != "20" || payload["loss_limit"] != "5" {
		t.Fatalf("LibreSpeed recovery used wrong engine limits: %#v", payload)
	}
	if _, err := pool.Exec(ctx, `UPDATE alert_rules SET value=72,for_tests=1,updated_at=clock_timestamp()+interval '1 second' WHERE id=$1`, downloadRuleID); err != nil {
		t.Fatal(err)
	}
	recovery := qualityMeasurements("74200000-0000-4000-8000-000000000006", base.Add(5*time.Minute), 90, 45, 5, 2, 0)
	saveAlertResult(t, ctx, agent, recovery)
	changed := qualityMeasurements("74200000-0000-4000-8000-000000000007", base.Add(6*time.Minute), 71, 20, 5, 2, 0)
	saveAlertResult(t, ctx, agent, changed)
	if err := pool.QueryRow(ctx, `SELECT payload FROM alert_events WHERE rule_id=$1 ORDER BY ts DESC LIMIT 1`, downloadRuleID).Scan(&payload); err != nil || payload["threshold"] != "72% of expected" {
		t.Fatalf("subsequent rule edit was hidden by migrated event settings: %#v %v", payload, err)
	}
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
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var ruleEvent *alerts.Event
	for i := range events {
		if events[i].RuleID != nil && *events[i].RuleID == id {
			ruleEvent = &events[i]
			break
		}
	}
	if ruleEvent == nil || ruleEvent.ResolvedAt == nil || total < 1 {
		t.Fatalf("streak-breaking result did not resolve the event: total=%d events=%+v", total, events)
	}
	event, etag, err := restarted.GetEvent(ctx, ruleEvent.ID)
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
	openEvents, total, err := restarted.ListEvents(ctx, alerts.EventFilter{State: "open"}, 1, 25)
	reopened := false
	for _, candidate := range openEvents {
		if candidate.ID == event.ID && !candidate.Acked && candidate.ResolvedAt != nil {
			reopened = true
		}
	}
	if err != nil || total < 1 || !reopened {
		t.Fatalf("reopened event was not in open activity: total=%d reopened=%t err=%v", total, reopened, err)
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
	rows, count, err := restarted.ListRules(ctx, 1, 25)
	deletedRuleVisible := false
	for _, listed := range rows {
		if listed.ID == id {
			deletedRuleVisible = true
		}
	}
	if err != nil || count != 8 || deletedRuleVisible {
		t.Fatalf("deleted rule remains visible: count=%d deleted=%t err=%v", count, deletedRuleVisible, err)
	}
}

func TestObservedNetworkAndMedianEventsIntegration(t *testing.T) {
	pool, ctx := integrationDB(t)
	addNode(t, ctx, pool, testNodeID, "observed-edge", time.Now().UTC().Add(-time.Hour), 30)
	if _, err := pool.Exec(ctx, `UPDATE nodes SET server_id=2 WHERE id=$1`, testNodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO notification_event_settings(event_key,enabled,severity,channels,thresholds) VALUES
		('network.server_differs',true,'warn',ARRAY['email'],'{}'),
		('quality.drop_vs_median',true,'warn',ARRAY['email'],'{"drop_pct":30}')`); err != nil {
		t.Fatal(err)
	}
	store := &alerts.Store{DB: pool, AppKey: []byte(appKey)}
	start := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	ip := "203.0.113.30"
	for i := 0; i < 3; i++ {
		result := testResult(fmt.Sprintf("30000000-0000-4000-8000-%012d", i+1), start.Add(time.Duration(i)*time.Hour), "ok", &ip, 100)
		isp := "Alpha ISP"
		result.EngineISP = &isp
		if _, err := (&agentapi.Postgres{Pool: pool, Hook: store}).SaveResult(ctx, testNodeID, result); err != nil {
			t.Fatal("save baseline:", err)
		}
	}
	result := testResult("30000000-0000-4000-8000-000000000004", start.Add(3*time.Hour), "ok", &ip, 30)
	isp := "Beta ISP"
	result.EngineISP = &isp
	if _, err := (&agentapi.Postgres{Pool: pool, Hook: store}).SaveResult(ctx, testNodeID, result); err != nil {
		t.Fatal("save changed result:", err)
	}
	if scalar(t, ctx, pool, `SELECT count(*) FROM alert_events WHERE node_id=$1 AND event_key='network.server_differs'`, testNodeID) != 4 {
		t.Fatal("pinned server mismatch events were not emitted")
	}
	if scalar(t, ctx, pool, `SELECT count(*) FROM alert_events WHERE node_id=$1 AND event_key='network.isp_changed' AND payload->>'old_isp'='Alpha ISP' AND payload->>'isp'='Beta ISP'`, testNodeID) != 1 {
		t.Fatal("ISP change payload was not emitted")
	}
	if scalar(t, ctx, pool, `SELECT count(*) FROM alert_events WHERE node_id=$1 AND event_key='quality.drop_vs_median' AND payload->>'threshold'='70.0'`, testNodeID) != 1 {
		t.Fatal("seven-day median drop event was not emitted")
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
	var detail string
	if err := pool.QueryRow(ctx, `SELECT detail FROM alert_events WHERE rule_id=$1`, ruleID).Scan(&detail); err != nil || detail != "Changed from 203.0.113.1 to 203.0.113.2 (Fixture city)" {
		t.Fatalf("the IP change event must name the old and the new address: %q %v", detail, err)
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
	if err := pool.QueryRow(ctx, `SELECT detail FROM alert_events WHERE rule_id=$1 ORDER BY ts DESC LIMIT 1`, ruleID).Scan(&detail); err != nil || detail != "Changed from 203.0.113.2 to 203.0.113.3 (Fixture city)" {
		t.Fatalf("the drained IP change event must name the old and the new address: %q %v", detail, err)
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
	const ispID = "20000000-0000-4000-8000-0000000000a1"
	const resultID = "20000000-0000-4000-8000-0000000000a2"
	if _, err := pool.Exec(ctx, `INSERT INTO isps(id,asn,organisation,name) VALUES($1,64500,'Event ISP','Event ISP')`, ispID); err != nil {
		t.Fatal("seed event-time ISP:", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ip_addresses(ip,isp_id) VALUES('203.0.113.44',$1)`, ispID); err != nil {
		t.Fatal("seed event-time address:", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,ip,threads,server,engine,isp_id)
		VALUES($1,$2,$3,'ok',80,40,8,1,'203.0.113.44',4,'{"id":1,"name":"fixture"}'::jsonb,'ookla',$4)`, resultID, testNodeID, old.Add(5*time.Minute), ispID); err != nil {
		t.Fatal("seed event-time result:", err)
	}
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
	assertAlertRuleEventISP(t, store, ctx, ruleID, ispID)
	if _, err := pool.Exec(ctx, `DELETE FROM results WHERE id=$1`, resultID); err != nil {
		t.Fatal("delete retained trigger context result:", err)
	}
	assertAlertRuleEventISP(t, store, ctx, ruleID, ispID)
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

func assertAlertRuleEventISP(t *testing.T, store *alerts.Store, ctx context.Context, ruleID, ispID string) {
	t.Helper()
	events, total, err := store.ListEvents(ctx, alerts.EventFilter{State: "all", ISPID: ispID}, 1, 25)
	if err != nil {
		t.Fatal("list events by ISP:", err)
	}
	for _, event := range events {
		if event.RuleID != nil && *event.RuleID == ruleID && event.ISPID != nil && *event.ISPID == ispID {
			return
		}
	}
	t.Fatalf("result-less offline event ISP attribution: total=%d events=%+v", total, events)
}

func TestOfflineCooldownRepeatsAndRebreachesRemainTrackedIntegration(t *testing.T) {
	pool, ctx := integrationDB(t)
	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	addNode(t, ctx, pool, testNodeID, "offline-repeat-edge", base, 5)
	if _, err := pool.Exec(ctx, `INSERT INTO notification_event_settings(event_key,enabled,severity,channels,thresholds,cooldown_seconds,repeat_seconds)
		VALUES('node.offline',true,'crit',ARRAY['email'],'{}',300,60)`); err != nil {
		t.Fatal(err)
	}
	var ruleID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM alert_rules WHERE metric='offline' AND node_ids IS NULL AND engine IS NULL`).Scan(&ruleID); err != nil {
		t.Fatal(err)
	}
	now := base.Add(16 * time.Minute)
	store := &alerts.Store{DB: pool, AppKey: []byte(appKey), Now: func() time.Time { return now }}
	agentOfflineCheck(t, store, ctx)
	now = now.Add(61 * time.Second)
	agentOfflineCheck(t, store, ctx)
	assertOfflineRepeatRows(t, ctx, pool, ruleID, 2, 1, 1)
	if _, err := pool.Exec(ctx, `UPDATE nodes SET last_seen_at=$2 WHERE id=$1`, testNodeID, now); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	agentOfflineCheck(t, store, ctx)
	if _, err := pool.Exec(ctx, `UPDATE nodes SET last_seen_at=$2 WHERE id=$1`, testNodeID, base.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	now = base.Add(21 * time.Minute)
	agentOfflineCheck(t, store, ctx)
	assertOfflineRepeatRows(t, ctx, pool, ruleID, 3, 1, 2)
	now = now.Add(61 * time.Second)
	agentOfflineCheck(t, store, ctx)
	assertOfflineRepeatRows(t, ctx, pool, ruleID, 4, 1, 2)
	if _, err := pool.Exec(ctx, `UPDATE nodes SET last_seen_at=$2 WHERE id=$1`, testNodeID, now); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	agentOfflineCheck(t, store, ctx)
	assertOfflineRepeatRows(t, ctx, pool, ruleID, 4, 0, 2)
	t.Log("PASS: offline cooldown suppresses delivery without losing the open event; repeat events resolve; recovery and rebreach stay tracked")
}

func agentOfflineCheck(t *testing.T, store *alerts.Store, ctx context.Context) {
	t.Helper()
	if err := store.EvaluateOffline(ctx); err != nil {
		t.Fatal("offline evaluation:", err)
	}
}

func assertOfflineRepeatRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ruleID string, wantEvents, wantOpen, wantOutbox int) {
	t.Helper()
	events := scalar(t, ctx, pool, `SELECT count(*) FROM alert_events WHERE rule_id=$1`, ruleID)
	open := scalar(t, ctx, pool, `SELECT count(*) FROM alert_events WHERE rule_id=$1 AND resolved_at IS NULL`, ruleID)
	outbox := scalar(t, ctx, pool, `SELECT count(*) FROM alert_notification_outbox WHERE event_id IN (SELECT id FROM alert_events WHERE rule_id=$1)`, ruleID)
	if events != wantEvents || open != wantOpen || outbox != wantOutbox {
		t.Fatalf("offline event counts = %d/%d/%d, want %d/%d/%d", events, open, outbox, wantEvents, wantOpen, wantOutbox)
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
for (const file of ['i18n.js', 'seed.js', 'store.js'])
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

func missingEventSignatures(want, got []eventSignature) []eventSignature {
	counts := make(map[eventSignature]int, len(got))
	for _, event := range got {
		counts[event]++
	}
	var missing []eventSignature
	for _, event := range want {
		if counts[event] == 0 {
			missing = append(missing, event)
			continue
		}
		counts[event]--
	}
	return missing
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
		if event.RuleID == nil {
			continue // catalog-only notifications (test.success/recovery) are outside the preview alert fixture
		}
		if _, fixtureRule := fixtureRuleID(ruleIDs, *event.RuleID); !fixtureRule {
			continue // migration-seeded defaults are outside the preview rule set
		}
		var ts time.Time
		ts, err := time.Parse(time.RFC3339Nano, event.TS)
		if err != nil {
			t.Fatal(err)
		}
		signature := eventSignature{RuleID: deref(event.RuleID), NodeID: deref(event.NodeID), Severity: event.Severity, TS: ts.UnixMilli(), Title: event.Title, Detail: event.Detail, Acked: event.Acked}
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
		t.Fatalf("30-day replay event count differs: center=%d preview=%d missing=%+v extra=%+v",
			len(actualSet), len(expectedSet), missingEventSignatures(expectedSet, actualSet), missingEventSignatures(actualSet, expectedSet))
	}
	for i := range actualSet {
		if actualSet[i] != expectedSet[i] {
			t.Fatalf("30-day replay event %d differs:\ncenter:  %+v\npreview: %+v", i, actualSet[i], expectedSet[i])
		}
	}
	t.Logf("PASS: replayed %d fixture results; center and preview produced %d identical alert events", len(fixture.Results), len(actualSet))
}

func fixtureRuleID(ruleIDs map[string]string, id string) (string, bool) {
	for previewID, storedID := range ruleIDs {
		if storedID == id {
			return previewID, true
		}
	}
	return "", false
}
