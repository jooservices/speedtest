//go:build integration

package usage_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/agentapi"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/ownerapi"
	"github.com/jooservices/speedtest/internal/center/usage"
	"github.com/jooservices/speedtest/internal/shared/config"
)

const (
	usageNodeA = "00000000-0000-4000-8000-000000000a01"
	usageNodeB = "00000000-0000-4000-8000-000000000a02"
	usageISPA  = "00000000-0000-4000-8000-000000000a03"
)

func usagePool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	values, err := godotenv.Read(filepath.Join("..", "..", "..", ".env"))
	if err != nil {
		t.Fatal("a configured worktree .env is required")
	}
	for key, value := range values {
		t.Setenv(key, value)
	}
	cfg, err := config.LoadCenter()
	if err != nil {
		t.Fatal("invalid integration configuration")
	}
	if err := db.CheckDestructiveAllowed(cfg.AppEnv); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	return pool, ctx
}

func seedUsage(t *testing.T, pool *pgxpool.Pool, ctx context.Context) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO isps(id,asn,organisation,name) VALUES($1,64500,'Fixture Network','Fixture ISP')`, usageISPA); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ id, name string }{{usageNodeA, "Alpha"}, {usageNodeB, "Bravo"}} {
		if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES($1,$2,300,100)`, row.id, row.name); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct {
		node, engine string
		interval     int
	}{{usageNodeA, "ookla", 60}, {usageNodeA, "librespeed", 120}, {usageNodeB, "ookla", 90}} {
		if _, err := pool.Exec(ctx, `INSERT INTO engine_schedules(node_id,engine,interval_minutes,enabled) VALUES($1,$2,$3,true)`, row.node, row.engine, row.interval); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ip_addresses(ip) VALUES('203.0.113.80')`); err != nil {
		t.Fatal(err)
	}
	fixtures := []struct {
		id, node, engine, isp string
		ts                    time.Time
		down, up              any
		status                string
	}{
		{"00000000-0000-4000-8000-000000000b01", usageNodeA, "ookla", usageISPA, time.Date(2026, 11, 2, 4, 0, 0, 0, time.UTC), int64(100), int64(50), "ok"},
		{"00000000-0000-4000-8000-000000000b02", usageNodeA, "librespeed", "", time.Date(2026, 11, 3, 4, 0, 0, 0, time.UTC), int64(1000), int64(500), "ok"},
		{"00000000-0000-4000-8000-000000000b03", usageNodeB, "ookla", "", time.Date(2026, 11, 4, 4, 0, 0, 0, time.UTC), int64(500), int64(250), "ok"},
		{"00000000-0000-4000-8000-000000000b04", usageNodeA, "ookla", usageISPA, time.Date(2026, 10, 5, 4, 0, 0, 0, time.UTC), int64(10000), int64(5000), "ok"},
		{"00000000-0000-4000-8000-000000000b05", usageNodeB, "ookla", "", time.Date(2026, 11, 5, 4, 0, 0, 0, time.UTC), nil, nil, "failed"},
	}
	for _, row := range fixtures {
		var down, up = row.down, row.up
		var errValue any
		var server any = `{"name":"Fixture"}`
		var downMbps, upMbps, ping any = 100.0, 25.0, 8.0
		if row.status == "failed" {
			errValue, server, downMbps, upMbps, ping = "offline", nil, nil, nil, nil
		}
		_, err := pool.Exec(ctx, `INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,server,ip,threads,error,bytes_down,bytes_up,engine,isp_id)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,'203.0.113.80',4,$10,$11,$12,$13,NULLIF($14,'')::uuid)`, row.id, row.node, row.ts, row.status,
			downMbps, upMbps, ping, func() any {
				if row.status == "failed" {
					return nil
				}
				return 9
			}(), server, errValue, down, up, row.engine, row.isp)
		if err != nil {
			t.Fatal(err)
		}
	}
}

type usageIntegrationFixture struct {
	pool  *pgxpool.Pool
	ctx   context.Context
	now   *time.Time
	store *usage.Store
}

func newUsageIntegrationFixture(t *testing.T) usageIntegrationFixture {
	t.Helper()
	pool, ctx := usagePool(t)
	seedUsage(t, pool, ctx)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	fixture := usageIntegrationFixture{pool: pool, ctx: ctx, now: &now}
	fixture.store = &usage.Store{DB: pool, Now: func() time.Time { return *fixture.now }}
	return fixture
}

func TestUsageBudgetsAdviceAndPolicyAgainstPostgres(t *testing.T) {
	fixture := newUsageIntegrationFixture(t)
	query := verifyUsageQueries(t, fixture)
	staleTag, currentTag := verifyBudgetsAndAdvice(t, fixture, query)
	verifyBudgetPolicyAndEvents(t, fixture)
	verifyBudgetMonthResetAndDelete(t, fixture, currentTag, staleTag)
	t.Log("PASS: range/group/node/engine usage, monthly comparison, rollup-backed budgets, ETag replacement, advice, warning/limit actions, audit, JOOST-78 extension events and month reset")
}

func TestAdviceReservesSharedNodeAndGlobalBudgetUsageIntegration(t *testing.T) {
	fixture := newUsageIntegrationFixture(t)
	tag := saveBudget(t, fixture, usage.Budget{NodeID: usageNodeA, MonthlyBytes: 1_000_000, Action: "pause"}, "")
	local, err := fixture.store.Advice(fixture.ctx, usageNodeA, "ookla", 60, 0)
	if err != nil || local.BudgetScope != usageNodeA || local.RemainingBudgetBytes != 460_000 || local.RecommendedInterval == nil || *local.RecommendedInterval != 15 {
		t.Fatalf("node advice did not reserve the enabled LibreSpeed estimate: %+v, %v", local, err)
	}
	_ = saveBudget(t, fixture, usage.Budget{MonthlyBytes: 1_000_000, Action: "pause"}, tag)
	global, err := fixture.store.Advice(fixture.ctx, usageNodeA, "ookla", 60, 0)
	if err != nil || global.BudgetScope != "global" || global.RemainingBudgetBytes != 100_000 || global.RecommendedInterval == nil || *global.RecommendedInterval != 65 {
		t.Fatalf("global advice did not reserve other nodes and engines: %+v, %v", global, err)
	}
}

func TestAdviceRequiresCompleteByteMeasurementsIntegration(t *testing.T) {
	fixture := newUsageIntegrationFixture(t)
	insertPartialUsageResult(t, fixture, usageNodeA, "00000000-0000-4000-8000-000000000b10", 900_000, nil)
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE results SET bytes_up=NULL WHERE node_id=$1 AND engine='librespeed'`, usageNodeA); err != nil {
		t.Fatal(err)
	}
	var completeSiblingSamples int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM results WHERE node_id=$1 AND engine='librespeed' AND bytes_down IS NOT NULL AND bytes_up IS NOT NULL`, usageNodeA).Scan(&completeSiblingSamples); err != nil || completeSiblingSamples != 0 {
		t.Fatalf("incomplete sibling fixtures have %d complete samples, %v", completeSiblingSamples, err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES('00000000-0000-4000-8000-000000000a04','Charlie',100,50)`); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO engine_schedules(node_id,engine,interval_minutes,enabled) VALUES('00000000-0000-4000-8000-000000000a04','ookla',60,true)`); err != nil {
		t.Fatal(err)
	}
	insertPartialUsageResult(t, fixture, "00000000-0000-4000-8000-000000000a04", "00000000-0000-4000-8000-000000000b11", 300, nil)
	missing, err := fixture.store.Advice(fixture.ctx, "00000000-0000-4000-8000-000000000a04", "ookla", 60, 1_000_000)
	if err != nil || missing.MeasurementAvailable || missing.AverageBytesPerTest != nil || missing.EstimatedBytesPerDay != nil || missing.RecommendationStatus != "unavailable" {
		t.Fatalf("partial-only result was treated as a complete measurement: %+v, %v", missing, err)
	}
	_ = saveBudget(t, fixture, usage.Budget{NodeID: usageNodeA, MonthlyBytes: 1_000_000, Action: "pause"}, "")
	shared, err := fixture.store.Advice(fixture.ctx, usageNodeA, "ookla", 60, 0)
	if err != nil || !shared.MeasurementAvailable || *shared.AverageBytesPerTest != 150 || shared.SharedMeasurementsAvailable || shared.RecommendationStatus != "unavailable" {
		t.Fatalf("partial counters biased averages or allowed shared advice: %+v, %v", shared, err)
	}
}

func insertPartialUsageResult(t *testing.T, fixture usageIntegrationFixture, node, id string, down, up any) {
	t.Helper()
	_, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,server,ip,threads,bytes_down,bytes_up,engine)
VALUES($1,$2,$3,'ok',100,25,8,9,'{"name":"Fixture"}'::jsonb,'203.0.113.80',4,$4,$5,'ookla')`, id, node, *fixture.now, down, up)
	if err != nil {
		t.Fatal(err)
	}
}

func TestBandwidthBudgetWarningStartsAtEightyPercentIntegration(t *testing.T) {
	fixture := newUsageIntegrationFixture(t)
	_ = saveBudget(t, fixture, usage.Budget{MonthlyBytes: 3000, Action: "pause"}, "")
	response, err := fixture.store.Usage(fixture.ctx, usage.Query{NodeID: usageNodeA, From: fixture.now.Add(-24 * time.Hour), To: *fixture.now, Group: "day"})
	if err != nil || len(response.Budgets) != 1 || response.Budgets[0].Ratio != 0.8 || !response.Budgets[0].Alert {
		t.Fatalf("80 percent budget warning state = %+v, %v", response.Budgets, err)
	}
	var warnings int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM usage_budget_events WHERE scope_key='global' AND event_type='budget_warning'`).Scan(&warnings); err != nil || warnings != 1 {
		t.Fatalf("80 percent application notification events = %d, %v", warnings, err)
	}
}

func TestWidenPolicyPausesAlreadyMaximalScheduleIntegration(t *testing.T) {
	fixture := newUsageIntegrationFixture(t)
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE engine_schedules SET interval_minutes=1440 WHERE node_id=$1 AND engine='librespeed'`, usageNodeA); err != nil {
		t.Fatal(err)
	}
	_ = saveBudget(t, fixture, usage.Budget{MonthlyBytes: 1000, Action: "widen", WidenMultiplier: 2}, "")
	assertSchedule(t, fixture.pool, fixture.ctx, usageNodeA, "librespeed", 1440, false)
	response, err := fixture.store.Usage(fixture.ctx, usage.Query{NodeID: usageNodeA, From: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), To: *fixture.now, Group: "day"})
	if err != nil || len(response.Budgets) != 1 || !response.Budgets[0].AtMaximumInterval || !response.Budgets[0].Alert {
		t.Fatalf("maximal interval budget status = %+v, %v", response.Budgets, err)
	}
	var eventCount int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM usage_budget_events WHERE scope_key='global' AND event_type='tests_paused'`).Scan(&eventCount); err != nil || eventCount != 1 {
		t.Fatalf("maximal interval fallback event count = %d, %v", eventCount, err)
	}
}

func verifyUsageQueries(t *testing.T, fixture usageIntegrationFixture) usage.Query {
	t.Helper()
	query := usage.Query{From: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), To: *fixture.now, Group: "day"}
	got, err := fixture.store.Usage(fixture.ctx, query)
	if err != nil || got.Totals.Total != 17400 || got.MonthToDate.Total != 2400 || got.PreviousMonth.Total != 15000 || len(got.ByNodeEngine) != 3 || len(got.Series) != 4 {
		t.Fatalf("usage totals or separated groups = %+v, %v", got, err)
	}
	if got.ByNodeEngine[0].Engine == got.ByNodeEngine[1].Engine && got.ByNodeEngine[0].NodeID == got.ByNodeEngine[1].NodeID {
		t.Fatal("usage grouped distinct engines into one row")
	}
	filtered, err := fixture.store.Usage(fixture.ctx, usage.Query{NodeID: usageNodeA, Engine: "librespeed", From: query.From, To: *fixture.now, Group: "week"})
	if err != nil || filtered.Totals.Total != 1500 || len(filtered.ByNodeEngine) != 1 || filtered.ByNodeEngine[0].Engine != "librespeed" || len(filtered.Series) != 1 {
		t.Fatalf("filtered usage = %+v, %v", filtered, err)
	}
	ispFiltered, err := fixture.store.Usage(fixture.ctx, usage.Query{ISPID: usageISPA, From: query.From, To: *fixture.now, Group: "month"})
	if err != nil || ispFiltered.Totals.Total != 15150 || len(ispFiltered.ByNodeEngine) != 1 || ispFiltered.ByNodeEngine[0].ISPID != usageISPA || ispFiltered.ByNodeEngine[0].ISPName != "Fixture ISP" {
		t.Fatalf("recorded ISP usage filter = %+v, %v", ispFiltered, err)
	}
	return query
}

func verifyBudgetsAndAdvice(t *testing.T, fixture usageIntegrationFixture, query usage.Query) (string, string) {
	t.Helper()
	items, etag, err := fixture.store.Budgets(fixture.ctx)
	if err != nil || len(items) != 0 || etag == "" {
		t.Fatalf("empty budget collection = %v %q %v", items, etag, err)
	}
	global, nextTag, err := fixture.store.ReplaceBudget(fixture.ctx, usage.Budget{MonthlyBytes: 2000, Action: "widen", WidenMultiplier: 2}, etag)
	if err != nil || len(global) != 1 || nextTag == etag {
		t.Fatalf("global budget replace = %+v %q %v", global, nextTag, err)
	}
	if _, _, err := fixture.store.ReplaceBudget(fixture.ctx, usage.Budget{MonthlyBytes: 3000, Action: "pause"}, etag); !errors.Is(err, usage.ErrStale) {
		t.Fatalf("stale ETag error = %v", err)
	}
	nodeBudgets, finalTag, err := fixture.store.ReplaceBudget(fixture.ctx, usage.Budget{NodeID: usageNodeA, MonthlyBytes: 1000, Action: "pause"}, nextTag)
	if err != nil || len(nodeBudgets) != 2 {
		t.Fatalf("node budget replace = %+v %v", nodeBudgets, err)
	}
	statuses, err := fixture.store.Usage(fixture.ctx, query)
	if err != nil || len(statuses.Budgets) != 2 {
		t.Fatalf("budget status = %+v %v", statuses.Budgets, err)
	}
	checkBudgetRatios(t, statuses.Budgets)
	advice, err := fixture.store.Advice(fixture.ctx, usageNodeA, "ookla", 60, 0)
	if err != nil || advice.BudgetBytes != 2000 || advice.BudgetScope != "global" || advice.AverageBytesPerTest == nil || *advice.AverageBytesPerTest != 150 || advice.EstimatedBytesPerDay == nil || *advice.EstimatedBytesPerDay != 3600 || advice.RecommendedInterval != nil || advice.RecommendationStatus != "infeasible" {
		t.Fatalf("advice = %+v %v", advice, err)
	}
	return nextTag, finalTag
}

func checkBudgetRatios(t *testing.T, statuses []usage.BudgetStatus) {
	t.Helper()
	byScope := map[string]usage.BudgetStatus{}
	for _, status := range statuses {
		byScope[status.ScopeKey] = status
	}
	if byScope["global"].Ratio != 1.2 || !byScope["global"].Alert || byScope[usageNodeA].Ratio != 1.65 {
		t.Fatalf("budget threshold status = %+v", byScope)
	}
}

func verifyBudgetPolicyAndEvents(t *testing.T, fixture usageIntegrationFixture) {
	t.Helper()
	tx, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.AfterResultStored(fixture.ctx, tx, usageNodeA, agentapi.Result{}); err != nil {
		_ = tx.Rollback(fixture.ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	var intervalA, intervalB int
	var enabledA, enabledB bool
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT interval_minutes,enabled FROM engine_schedules WHERE node_id=$1 AND engine='ookla'`, usageNodeA).Scan(&intervalA, &enabledA); err != nil || intervalA != 60 || enabledA {
		t.Fatalf("node budget pause = interval %d enabled %t err %v", intervalA, enabledA, err)
	}
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT interval_minutes,enabled FROM engine_schedules WHERE node_id=$1 AND engine='ookla'`, usageNodeB).Scan(&intervalB, &enabledB); err != nil || intervalB != 180 || !enabledB {
		t.Fatalf("global budget widening = interval %d enabled %t err %v", intervalB, enabledB, err)
	}
	verifyBudgetEvents(t, fixture)
}

func verifyBudgetEvents(t *testing.T, fixture usageIntegrationFixture) {
	t.Helper()
	var count int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM usage_budget_events`).Scan(&count); err != nil || count != 7 {
		t.Fatalf("threshold/action event count = %d err %v", count, err)
	}
	var widened, paused int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FILTER (WHERE event_type='interval_widened'),count(*) FILTER (WHERE event_type='tests_paused') FROM usage_budget_events`).Scan(&widened, &paused); err != nil || widened != 1 || paused != 2 {
		t.Fatalf("schedule action event counts = widened:%d paused:%d err %v", widened, paused, err)
	}
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM audit_log WHERE action LIKE 'usage_budget.%'`).Scan(&count); err != nil || count != 7 {
		t.Fatalf("policy audit count = %d err %v", count, err)
	}
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM audit_log WHERE action='usage_budget.limit'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("budget limit audit count = %d err %v", count, err)
	}
	events, err := fixture.store.PendingNotificationEvents(fixture.ctx, 20)
	if err != nil || len(events) != 7 {
		t.Fatalf("pending notification events = %d, %v", len(events), err)
	}
	if err := fixture.store.MarkNotificationEventPublished(fixture.ctx, events[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.MarkNotificationEventPublished(fixture.ctx, "not-a-uuid"); !errors.Is(err, usage.ErrNotFound) {
		t.Fatalf("invalid notification event acknowledgment = %v", err)
	}
	if pending, err := fixture.store.PendingNotificationEvents(fixture.ctx, 20); err != nil || len(pending) != 6 {
		t.Fatalf("pending after acknowledgment = %d, %v", len(pending), err)
	}
}

func verifyBudgetMonthResetAndDelete(t *testing.T, fixture usageIntegrationFixture, etag, staleTag string) {
	t.Helper()
	*fixture.now = time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	if err := fixture.store.ResetExpiredSchedules(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	var interval int
	var enabled bool
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT interval_minutes,enabled FROM engine_schedules WHERE node_id=$1 AND engine='ookla'`, usageNodeA).Scan(&interval, &enabled); err != nil || interval != 60 || !enabled {
		t.Fatalf("next-month schedule restoration = interval %d enabled %t err %v", interval, enabled, err)
	}
	if _, _, err := fixture.store.DeleteBudget(fixture.ctx, "global", staleTag); !errors.Is(err, usage.ErrStale) {
		t.Fatalf("delete with stale collection tag = %v", err)
	}
	_, finalTag, err := fixture.store.DeleteBudget(fixture.ctx, "global", etag)
	if err != nil {
		t.Fatalf("delete global budget = %v", err)
	}
	if _, _, err := fixture.store.DeleteBudget(fixture.ctx, usageNodeA, finalTag); err != nil {
		t.Fatalf("delete node budget = %v", err)
	}
}

func TestUsageRejectsInvalidRequests(t *testing.T) {
	fixture := newUsageIntegrationFixture(t)
	if _, err := fixture.store.Usage(fixture.ctx, usage.Query{Group: "year"}); !errors.Is(err, usage.ErrInvalid) {
		t.Fatalf("invalid usage group error = %v", err)
	}
	if _, err := fixture.store.Usage(fixture.ctx, usage.Query{NodeID: "invalid", Group: "day"}); !errors.Is(err, usage.ErrInvalid) {
		t.Fatalf("invalid node filter error = %v", err)
	}
	if _, _, err := fixture.store.ReplaceBudget(fixture.ctx, usage.Budget{MonthlyBytes: 0, Action: "pause"}, ""); !errors.Is(err, usage.ErrInvalid) {
		t.Fatalf("invalid budget error = %v", err)
	}
	if _, _, err := fixture.store.DeleteBudget(fixture.ctx, "invalid", ""); !errors.Is(err, usage.ErrNotFound) {
		t.Fatalf("invalid budget scope error = %v", err)
	}
	verifyInvalidAdviceRequests(t, fixture)
	for _, limit := range []int{0, 101} {
		if _, err := fixture.store.PendingNotificationEvents(fixture.ctx, limit); !errors.Is(err, usage.ErrInvalid) {
			t.Fatalf("invalid event limit %d error = %v", limit, err)
		}
	}
	if err := fixture.store.MarkNotificationEventPublished(fixture.ctx, "not-a-uuid"); !errors.Is(err, usage.ErrNotFound) {
		t.Fatalf("invalid event ID error = %v", err)
	}
	if err := fixture.store.MarkNotificationEventPublished(fixture.ctx, "00000000-0000-4000-8000-000000000099"); !errors.Is(err, usage.ErrNotFound) {
		t.Fatalf("unknown event ID error = %v", err)
	}
}

func verifyInvalidAdviceRequests(t *testing.T, fixture usageIntegrationFixture) {
	t.Helper()
	for _, request := range []struct {
		node, engine string
		interval     int
		budget       int64
	}{
		{"invalid", "ookla", 60, 0}, {usageNodeA, "unknown", 60, 0}, {usageNodeA, "ookla", 4, 0}, {usageNodeA, "ookla", 60, -1},
	} {
		if _, err := fixture.store.Advice(fixture.ctx, request.node, request.engine, request.interval, request.budget); !errors.Is(err, usage.ErrInvalid) {
			t.Fatalf("invalid advice request %+v error = %v", request, err)
		}
	}
}

func TestUsageLoopStopsWhenCancelled(t *testing.T) {
	fixture := newUsageIntegrationFixture(t)
	ctx, cancel := context.WithCancel(fixture.ctx)
	cancel()
	fixture.store.Loop(ctx, nil)
}

type cancelUsageQueryDB struct {
	usage.DB
	cancel   context.CancelFunc
	cancelAt int
	calls    int
}

func (d *cancelUsageQueryDB) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	d.calls++
	if d.calls == d.cancelAt {
		d.cancel()
	}
	return d.DB.Query(ctx, query, args...)
}

func TestUsageBudgetPresenceCacheSkipsUnconfiguredBudgets(t *testing.T) {
	pool, ctx := usagePool(t)
	store := &usage.Store{DB: pool}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for range 2 {
		if err := store.AfterResultStored(ctx, tx, usageNodeA, agentapi.Result{}); err != nil {
			t.Fatal(err)
		}
	}
	var pid int32
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	var locks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND pid=$1`, pid).Scan(&locks); err != nil {
		t.Fatal(err)
	}
	if locks != 0 {
		t.Fatalf("unconfigured budget path took %d advisory locks", locks)
	}
}

func TestUsageDatabaseCancellationErrors(t *testing.T) {
	fixture := newUsageIntegrationFixture(t)
	assertUsageCancellationErrors(t, fixture)
	assertBudgetStatusCancellationErrors(t, fixture)
}

func assertUsageCancellationErrors(t *testing.T, fixture usageIntegrationFixture) {
	t.Helper()
	ctx, cancel := context.WithCancel(fixture.ctx)
	cancel()
	query := usage.Query{From: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), To: *fixture.now, Group: "day"}
	if _, err := fixture.store.Usage(ctx, query); err == nil {
		t.Fatal("cancelled usage query succeeded")
	}
	if _, _, err := fixture.store.Budgets(ctx); err == nil {
		t.Fatal("cancelled budget query succeeded")
	}
	if _, err := fixture.store.Advice(ctx, usageNodeA, "ookla", 60, 0); err == nil {
		t.Fatal("cancelled advice query succeeded")
	}
	if _, err := fixture.store.PendingNotificationEvents(ctx, 1); err == nil {
		t.Fatal("cancelled event query succeeded")
	}
	if err := fixture.store.MarkNotificationEventPublished(ctx, "00000000-0000-4000-8000-000000000099"); err == nil {
		t.Fatal("cancelled event update succeeded")
	}
	if _, _, err := fixture.store.ReplaceBudget(ctx, usage.Budget{MonthlyBytes: 100000, Action: "pause"}, "etag"); err == nil {
		t.Fatal("cancelled budget replacement succeeded")
	}
	if _, _, err := fixture.store.DeleteBudget(ctx, "global", "etag"); err == nil {
		t.Fatal("cancelled budget deletion succeeded")
	}
	if err := fixture.store.ResetExpiredSchedules(ctx); err == nil {
		t.Fatal("cancelled schedule reset succeeded")
	}
	tx, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(fixture.ctx) }()
	if err := fixture.store.AfterResultStored(ctx, tx, usageNodeA, agentapi.Result{}); err == nil {
		t.Fatal("cancelled post-result policy succeeded")
	}
}

func assertBudgetStatusCancellationErrors(t *testing.T, fixture usageIntegrationFixture) {
	t.Helper()
	_, etag, err := fixture.store.Budgets(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.store.ReplaceBudget(fixture.ctx, usage.Budget{MonthlyBytes: 100000, Action: "pause"}, etag); err != nil {
		t.Fatal(err)
	}
	query := usage.Query{From: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), To: *fixture.now, Group: "day"}
	for _, cancelAt := range []int{3, 4} {
		ctx, cancel := context.WithCancel(fixture.ctx)
		db := &cancelUsageQueryDB{DB: fixture.pool, cancel: cancel, cancelAt: cancelAt}
		store := &usage.Store{DB: db, Now: fixture.store.Now}
		if _, err := store.Usage(ctx, query); err == nil {
			cancel()
			t.Fatalf("usage query cancellation at query %d was ignored", cancelAt)
		}
		cancel()
	}
}

func TestUsageMigrationRollbackAndOlderWriterCompatibility(t *testing.T) {
	pool, ctx := usagePool(t)
	seedUsage(t, pool, ctx)
	store := &usage.Store{DB: pool, Now: func() time.Time { return time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC) }}
	verifyActiveOverrideRollback(t, pool, ctx, store)
	var rolled int64
	if err := pool.QueryRow(ctx, `SELECT bytes_down+bytes_up FROM usage_monthly_totals WHERE node_id=$1 AND engine='ookla' AND period_start='2026-11-01'`, usageNodeA).Scan(&rolled); err != nil || rolled != 150 {
		t.Fatalf("rollup backfill after re-up = %d, %v", rolled, err)
	}
	verifyOlderWriterBatchRollup(t, pool, ctx, store)
	t.Log("PASS: migration down preserves owner schedule edits and restores unchanged overrides; re-up backfills rows and batch trigger maintains rollups")
}

func verifyActiveOverrideRollback(t *testing.T, pool *pgxpool.Pool, ctx context.Context, store *usage.Store) {
	t.Helper()
	_, tag, err := store.Budgets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ReplaceBudget(ctx, usage.Budget{MonthlyBytes: 1000, Action: "pause"}, tag); err != nil {
		t.Fatal("apply active policy before rollback", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE engine_schedules SET interval_minutes=75,enabled=true WHERE node_id=$1 AND engine='ookla'`, usageNodeA); err != nil {
		t.Fatal(err)
	}
	if err := db.MigrateDownTo(ctx, pool, 32); err != nil {
		t.Fatal(err)
	}
	var version int64
	if err := pool.QueryRow(ctx, `SELECT version_id FROM goose_db_version WHERE is_applied ORDER BY version_id DESC LIMIT 1`).Scan(&version); err != nil || version != 32 {
		t.Fatalf("usage rollback left migration version %d applied, error %v; want develop version 00032", version, err)
	}
	var scheduleDefault string
	if err := pool.QueryRow(ctx, `SELECT column_default FROM information_schema.columns WHERE table_schema='public' AND table_name='schedules' AND column_name='interval_minutes'`).Scan(&scheduleDefault); err != nil || scheduleDefault != "60" {
		t.Fatalf("usage rollback schedule default = %q, error %v; want 60 minutes", scheduleDefault, err)
	}
	assertSchedule(t, pool, ctx, usageNodeA, "ookla", 75, true)
	assertSchedule(t, pool, ctx, usageNodeB, "ookla", 90, true)
	var tableMissing bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('usage_monthly_totals') IS NULL`).Scan(&tableMissing); err != nil || !tableMissing {
		t.Fatalf("usage down migration did not remove rollup: %t %v", tableMissing, err)
	}
	if err := db.Migrate(ctx, pool, "up"); err != nil {
		t.Fatal(err)
	}
}

func verifyOlderWriterBatchRollup(t *testing.T, pool *pgxpool.Pool, ctx context.Context, store *usage.Store) {
	t.Helper()
	_, tag, err := store.Budgets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ReplaceBudget(ctx, usage.Budget{MonthlyBytes: 100000, Action: "pause"}, tag); err != nil {
		t.Fatal("configure rollup for older writer", err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO results(id,node_id,ts,status,error,ip,threads,bytes_down,bytes_up)
SELECT ('00000000-0000-4000-8000-' || lpad(g::text,12,'0'))::uuid,$1,'2026-11-06T00:00:00Z'::timestamptz + g*interval '1 minute',
       'failed','old writer fixture','203.0.113.80',1,g*100,g*10 FROM generate_series(1,3) g`, usageNodeA)
	if err != nil {
		t.Fatal("older writer batch insert", err)
	}
	var down, up int64
	if err := pool.QueryRow(ctx, `SELECT bytes_down,bytes_up FROM usage_monthly_totals WHERE node_id=$1 AND engine='ookla' AND period_start='2026-11-01'`, usageNodeA).Scan(&down, &up); err != nil || down != 700 || up != 110 {
		t.Fatalf("batch insert rollup = %d/%d, %v", down, up, err)
	}
}

func TestBudgetPolicyReconcilesEditsDeletesAndNewNodesAgainstPostgres(t *testing.T) {
	fixture := newUsageIntegrationFixture(t)
	globalTag := saveBudget(t, fixture, usage.Budget{MonthlyBytes: 2000, Action: "widen", WidenMultiplier: 2}, "")
	nodeTag := saveBudget(t, fixture, usage.Budget{NodeID: usageNodeA, MonthlyBytes: 1000, Action: "pause"}, globalTag)
	assertSchedule(t, fixture.pool, fixture.ctx, usageNodeA, "ookla", 60, false)
	editOwnerSchedule(t, fixture, usageNodeA, 90, true)
	assertSchedule(t, fixture.pool, fixture.ctx, usageNodeA, "ookla", 90, false)
	nodeTag = saveBudget(t, fixture, usage.Budget{NodeID: usageNodeA, MonthlyBytes: 1000, Action: "widen", WidenMultiplier: 3}, nodeTag)
	assertSchedule(t, fixture.pool, fixture.ctx, usageNodeA, "ookla", 270, true)
	addPolicyNode(t, fixture)
	assertSchedule(t, fixture.pool, fixture.ctx, "00000000-0000-4000-8000-000000000a03", "ookla", 120, true)
	globalTag = removeBudget(t, fixture, usageNodeA, nodeTag)
	assertSchedule(t, fixture.pool, fixture.ctx, usageNodeA, "ookla", 180, true)
	_ = removeBudget(t, fixture, "global", globalTag)
	assertSchedule(t, fixture.pool, fixture.ctx, usageNodeA, "ookla", 90, true)
	assertSchedule(t, fixture.pool, fixture.ctx, "00000000-0000-4000-8000-000000000a03", "ookla", 60, true)
	t.Log("PASS: node/global overlap, owner schedule rebase, action edit, budget deletion and newly added schedule policy reconciliation")
}

func saveBudget(t *testing.T, fixture usageIntegrationFixture, budget usage.Budget, tag string) string {
	t.Helper()
	if tag == "" {
		_, current, err := fixture.store.Budgets(fixture.ctx)
		if err != nil {
			t.Fatal(err)
		}
		tag = current
	}
	_, next, err := fixture.store.ReplaceBudget(fixture.ctx, budget, tag)
	if err != nil {
		t.Fatal(err)
	}
	return next
}

func removeBudget(t *testing.T, fixture usageIntegrationFixture, scope, tag string) string {
	t.Helper()
	_, next, err := fixture.store.DeleteBudget(fixture.ctx, scope, tag)
	if err != nil {
		t.Fatal(err)
	}
	return next
}

func assertSchedule(t *testing.T, pool *pgxpool.Pool, ctx context.Context, node, engine string, wantInterval int, wantEnabled bool) {
	t.Helper()
	var interval int
	var enabled bool
	err := pool.QueryRow(ctx, `SELECT interval_minutes,enabled FROM engine_schedules WHERE node_id=$1 AND engine=$2`, node, engine).Scan(&interval, &enabled)
	if err != nil || interval != wantInterval || enabled != wantEnabled {
		t.Fatalf("schedule %s/%s = %d/%t, want %d/%t: %v", node, engine, interval, enabled, wantInterval, wantEnabled, err)
	}
}

func editOwnerSchedule(t *testing.T, fixture usageIntegrationFixture, node string, interval int, enabled bool) {
	t.Helper()
	store := &ownerapi.Store{DB: fixture.pool, Now: fixture.store.Now}
	_, etag, err := store.Schedule(fixture.ctx, node)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ReplaceSchedule(fixture.ctx, node, ownerapi.ScheduleInput{Interval: interval, Offset: 0, Enabled: enabled}, etag); err != nil {
		t.Fatal(err)
	}
}

func addPolicyNode(t *testing.T, fixture usageIntegrationFixture) {
	t.Helper()
	node := "00000000-0000-4000-8000-000000000a03"
	tx, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(fixture.ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES($1,'Charlie',300,100)`, node); err != nil {
		_ = tx.Rollback(fixture.ctx)
		t.Fatal(err)
	}
	if _, err := tx.Exec(fixture.ctx, `INSERT INTO engine_schedules(node_id,engine,interval_minutes,enabled) VALUES($1,'ookla',60,true)`, node); err != nil {
		_ = tx.Rollback(fixture.ctx)
		t.Fatal(err)
	}
	if err := usage.ReconcileScheduleEdit(fixture.ctx, tx, node, *fixture.now); err != nil {
		_ = tx.Rollback(fixture.ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentGlobalBudgetCrossingIsSerializedAgainstPostgres(t *testing.T) {
	fixture := newUsageIntegrationFixture(t)
	_ = saveBudget(t, fixture, usage.Budget{MonthlyBytes: 3000, Action: "pause"}, "")
	ready, release := make(chan error, 2), make(chan struct{})
	done := make(chan error, 2)
	startConcurrentResult(t, fixture, usageNodeA, "00000000-0000-4000-8000-000000000b06", ready, release, done)
	startConcurrentResult(t, fixture, usageNodeB, "00000000-0000-4000-8000-000000000b07", ready, release, done)
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var count int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM usage_budget_events WHERE scope_key='global' AND event_type='budget_limit' AND used_bytes>=3200`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("combined global threshold event count = %d, %v", count, err)
	}
	assertSchedule(t, fixture.pool, fixture.ctx, usageNodeA, "ookla", 60, false)
	t.Log("PASS: independent cross-node result transactions serialize global rollup evaluation and enforce the combined limit")
}

func startConcurrentResult(t *testing.T, fixture usageIntegrationFixture, node, id string, ready chan<- error, release <-chan struct{}, done chan<- error) {
	t.Helper()
	go func() {
		tx, err := fixture.pool.Begin(fixture.ctx)
		if err != nil {
			ready <- err
			return
		}
		_, err = tx.Exec(fixture.ctx, `INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,server,ip,threads,bytes_down,bytes_up,engine)
			VALUES($1,$2,$3,'ok',100,25,8,9,'{"name":"Test"}'::jsonb,'203.0.113.80',1,400,0,'ookla')`, id, node, *fixture.now)
		if err != nil {
			_ = tx.Rollback(fixture.ctx)
			ready <- err
			return
		}
		ready <- nil
		<-release
		store := &usage.Store{DB: fixture.pool, Now: fixture.store.Now}
		if err := store.AfterResultStored(fixture.ctx, tx, node, agentapi.Result{}); err != nil {
			_ = tx.Rollback(fixture.ctx)
			done <- err
			return
		}
		done <- tx.Commit(fixture.ctx)
	}()
}

func TestBudgetUpdateDoesNotDeadlockInFlightResultAgainstPostgres(t *testing.T) {
	fixture := newUsageIntegrationFixture(t)
	tag, _ := saveInitialBudget(t, fixture)
	tx, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(fixture.ctx, `INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,server,ip,threads,bytes_down,bytes_up,engine)
		VALUES('00000000-0000-4000-8000-000000000b08',$1,$2,'ok',100,25,8,9,'{"name":"Test"}'::jsonb,'203.0.113.80',1,100,0,'ookla')`, usageNodeA, *fixture.now)
	if err != nil {
		_ = tx.Rollback(fixture.ctx)
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go replaceBudgetWhileIngesting(fixture, tag, done)
	waitForBudgetMutationWaiter(t, fixture)
	if err := fixture.store.AfterResultStored(fixture.ctx, tx, usageNodeA, agentapi.Result{}); err != nil {
		_ = tx.Rollback(fixture.ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("budget update remained blocked after ingestion committed")
	}
	t.Log("PASS: budget refresh waits for the in-flight result without blocking result-hook commit")
}

func TestBudgetCreationAccountsForInFlightResultsAgainstPostgres(t *testing.T) {
	fixture := newUsageIntegrationFixture(t)
	tx, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(fixture.ctx, `INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,server,ip,threads,bytes_down,bytes_up,engine)
		VALUES('00000000-0000-4000-8000-000000000b09',$1,$2,'ok',100,25,8,9,'{"name":"Test"}'::jsonb,'203.0.113.80',1,1000,0,'ookla')`, usageNodeA, *fixture.now)
	if err != nil {
		_ = tx.Rollback(fixture.ctx)
		t.Fatal(err)
	}
	_, tag, err := fixture.store.Budgets(fixture.ctx)
	if err != nil {
		_ = tx.Rollback(fixture.ctx)
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, replaceErr := fixture.store.ReplaceBudget(fixture.ctx, usage.Budget{NodeID: usageNodeA, MonthlyBytes: 1000, Action: "pause"}, tag)
		done <- replaceErr
	}()
	waitForResultTableWaiter(t, fixture)
	if err := tx.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("budget creation remained blocked after the in-flight result committed")
	}
	var down, up int64
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT bytes_down,bytes_up FROM usage_monthly_totals WHERE node_id=$1 AND engine='ookla' AND period_start='2026-11-01'`, usageNodeA).Scan(&down, &up); err != nil || down != 1100 || up != 50 {
		t.Fatalf("refreshed usage = %d/%d, %v; want 1100/50", down, up, err)
	}
	assertSchedule(t, fixture.pool, fixture.ctx, usageNodeA, "ookla", 60, false)
	var events int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM usage_budget_events WHERE scope_key=$1 AND event_type='budget_limit' AND used_bytes>=1000`, usageNodeA).Scan(&events); err != nil || events != 1 {
		t.Fatalf("budget limit events = %d, %v; want 1", events, err)
	}
	t.Log("PASS: first budget refresh includes an in-flight result before applying the limit")
}

func saveInitialBudget(t *testing.T, fixture usageIntegrationFixture) (string, string) {
	t.Helper()
	_, tag, err := fixture.store.Budgets(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	next := saveBudget(t, fixture, usage.Budget{MonthlyBytes: 100000, Action: "widen", WidenMultiplier: 2}, tag)
	return next, tag
}

func replaceBudgetWhileIngesting(fixture usageIntegrationFixture, tag string, done chan<- error) {
	_, _, err := fixture.store.ReplaceBudget(fixture.ctx, usage.Budget{MonthlyBytes: 200000, Action: "widen", WidenMultiplier: 2}, tag)
	done <- err
}

func waitForBudgetMutationWaiter(t *testing.T, fixture usageIntegrationFixture) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM pg_locks WHERE NOT granted AND (locktype='advisory' OR (locktype='relation' AND relation='results'::regclass AND mode='ShareLock'))`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("budget update did not reach the waiting lifecycle lock")
}

func waitForResultTableWaiter(t *testing.T, fixture usageIntegrationFixture) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM pg_locks WHERE locktype='relation' AND relation='results'::regclass AND mode='ShareLock' AND NOT granted`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("budget creation did not wait for the in-flight result")
}
