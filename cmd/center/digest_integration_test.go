//go:build integration

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/notify"
	"github.com/jooservices/speedtest/internal/center/settings"
	"github.com/jooservices/speedtest/internal/shared/config"
)

const (
	digestTestNodeID = "b0000000-0000-4000-8000-000000000001"
	digestTestToken  = "digest-integration-placeholder"
)

type digestQueuedEvent struct {
	key      string
	payload  []byte
	channels []string
}

type digestCaptureSender struct {
	messages map[string][]string
}

func (s *digestCaptureSender) Send(_ context.Context, channel, _, _, message string) error {
	if s.messages == nil {
		s.messages = map[string][]string{}
	}
	s.messages[channel] = append(s.messages[channel], message)
	return nil
}

func TestAC1AC2DigestNotificationsEnabledPopulatedRestartAndLateResultIntegration(t *testing.T) {
	pool, ctx := openDigestNotificationPool(t)
	notifications := configureDigestNotifications(t, ctx, pool, true, true)
	capture := &digestCaptureSender{}
	notifications.Sender = capture
	insertDigestSummaryFixtures(t, ctx, pool)
	if _, err := (&notify.EventStore{DB: pool}).PutTemplate(ctx, "digest.daily", "telegram", "{period}\n{summary}\n{link}"); err != nil {
		t.Fatal("save digest template:", err)
	}

	now := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	scheduler := newDigestScheduler(pool, notifications, nil)
	scheduler.Now = func() time.Time { return now }
	if err := scheduler.RunOnce(ctx); err != nil {
		t.Fatal("run digest scheduler:", err)
	}
	queued := loadDigestEvents(t, ctx, pool)
	assertDigestQueuedEvent(t, queued["digest.daily"], []string{"telegram", "email"},
		"Node <alpha> forged — 2 tests: 1 successful, 1 failed; avg down 100.00 Mbps, avg up 40.00 Mbps",
		"https://speedtest.example.test/#/")
	assertDigestQueuedEvent(t, queued["digest.weekly"], []string{"telegram", "email"},
		"Node <alpha> forged — 1 test: 1 successful, 0 failed; avg down 80.00 Mbps, avg up 20.00 Mbps",
		"https://speedtest.example.test/#/")
	assertDigestPayloadHasNoCredential(t, queued["digest.daily"].payload)
	if consumed := consumeDigestOutbox(t, ctx, notifications); consumed != 2 {
		t.Fatalf("consumed digest outbox rows = %d, want 2", consumed)
	}
	if dispatched := dispatchDigestDeliveries(t, ctx, notifications, 4); dispatched != 4 {
		t.Fatalf("dispatched digest deliveries = %d, want 4", dispatched)
	}
	assertCapturedDigestMessage(t, capture.messages["telegram"], "1 node, 2 tests: 1 successful, 1 failed", "https://speedtest.example.test/#/")
	assertCapturedDigestMessage(t, capture.messages["email"], "Node <alpha> forged", "https://speedtest.example.test/#/")

	restarted := newDigestScheduler(pool, notifications, nil)
	restarted.Now = func() time.Time { return now }
	if err := restarted.RunOnce(ctx); err != nil {
		t.Fatal("restart digest scheduler:", err)
	}
	insertDigestResult(t, ctx, pool, 4, "2026-11-17 15:00:00+00", "ok", 900, 500)
	if err := restarted.RunOnce(ctx); err != nil {
		t.Fatal("run digest scheduler after late result:", err)
	}
	if got := countDigestEvents(t, ctx, pool); got != 2 {
		t.Fatalf("digest events after restart and late result = %d, want 2", got)
	}
	assertDigestQueuedEvent(t, loadDigestEvents(t, ctx, pool)["digest.daily"], []string{"telegram", "email"},
		"Node <alpha> forged — 2 tests: 1 successful, 1 failed; avg down 100.00 Mbps, avg up 40.00 Mbps",
		"https://speedtest.example.test/#/")
}

func TestAC1AC2DigestNotificationsDisabledEventChannelAndEmptyPeriodIntegration(t *testing.T) {
	pool, ctx := openDigestNotificationPool(t)
	notifications := configureDigestNotifications(t, ctx, pool, false, true)
	capture := &digestCaptureSender{}
	notifications.Sender = capture
	disabled := false
	if _, err := (&notify.EventStore{DB: pool}).UpdateEvent(ctx, "digest.daily", notify.EventPatch{Enabled: &disabled}); err != nil {
		t.Fatal("disable daily digest event:", err)
	}

	scheduler := newDigestScheduler(pool, notifications, nil)
	scheduler.Now = func() time.Time { return time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC) }
	if err := scheduler.RunOnce(ctx); err != nil {
		t.Fatal("run digest scheduler:", err)
	}
	queued := loadDigestEvents(t, ctx, pool)
	if _, ok := queued["digest.daily"]; ok {
		t.Fatal("disabled daily digest created an event")
	}
	assertDigestQueuedEvent(t, queued["digest.weekly"], []string{"telegram"}, "No speed test results were recorded for this period.", "")
	if consumed := consumeDigestOutbox(t, ctx, notifications); consumed != 1 {
		t.Fatalf("consumed digest outbox rows = %d, want 1", consumed)
	}
	if dispatched := dispatchDigestDeliveries(t, ctx, notifications, 1); dispatched != 1 {
		t.Fatalf("dispatched digest deliveries = %d, want 1", dispatched)
	}
	if got := strings.Join(capture.messages["telegram"], "\n"); !strings.Contains(got, "No speed test results were recorded for this period.") {
		t.Fatalf("empty-period message = %q", got)
	}
	if got := countEmailDeliveries(t, ctx, pool); got != 0 {
		t.Fatalf("disabled e-mail deliveries = %d, want 0", got)
	}
}

func TestAC2DigestDisabledPeriodsWithMissingURLStayCompleteAfterReenableIntegration(t *testing.T) {
	pool, ctx := openDigestNotificationPool(t)
	notifications := configureDigestNotifications(t, ctx, pool, false, false)
	insertDigestSummaryFixtures(t, ctx, pool)
	updateDigestSettings(t, ctx, pool, notifications, func(view *settings.Settings) {
		view.CenterURL = ""
	})
	disabled := false
	if _, err := (&notify.EventStore{DB: pool}).UpdateEvent(ctx, "digest.daily", notify.EventPatch{Enabled: &disabled}); err != nil {
		t.Fatal("disable daily digest event:", err)
	}

	now := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	scheduler := newDigestScheduler(pool, notifications, nil)
	scheduler.Now = func() time.Time { return now }
	if err := scheduler.RunOnce(ctx); err != nil {
		t.Fatal("run disabled digest scheduler:", err)
	}
	if got := countDigestEvents(t, ctx, pool); got != 0 {
		t.Fatalf("disabled digest events = %d, want 0", got)
	}
	if got := countCompletedDigestPeriods(t, ctx, pool); got != 2 {
		t.Fatalf("completed disabled digest periods = %d, want 2", got)
	}

	updateDigestSettings(t, ctx, pool, notifications, func(view *settings.Settings) {
		view.CenterURL = "https://speedtest.example.test"
		view.Notify.Telegram.On = true
	})
	enabled := true
	if _, err := (&notify.EventStore{DB: pool}).UpdateEvent(ctx, "digest.daily", notify.EventPatch{Enabled: &enabled}); err != nil {
		t.Fatal("re-enable daily digest event:", err)
	}
	insertDigestResult(t, ctx, pool, 4, "2026-11-17 15:00:00+00", "ok", 900, 500)
	if err := scheduler.RunOnce(ctx); err != nil {
		t.Fatal("run scheduler after re-enabling settings:", err)
	}
	if got := countDigestEvents(t, ctx, pool); got != 0 {
		t.Fatalf("late digest events after re-enabling settings = %d, want 0", got)
	}
	if got := countAllDigestDeliveries(t, ctx, pool); got != 0 {
		t.Fatalf("late digest deliveries after re-enabling settings = %d, want 0", got)
	}
}

func TestAC2DigestPopulatedPeriodsWithUnsafeCenterURLCompleteSilentlyIntegration(t *testing.T) {
	pool, ctx := openDigestNotificationPool(t)
	notifications := configureDigestNotifications(t, ctx, pool, false, true)
	insertDigestSummaryFixtures(t, ctx, pool)
	updateDigestSettings(t, ctx, pool, notifications, func(view *settings.Settings) {
		view.CenterURL = "https://owner:secret@speedtest.example.test"
	})
	scheduler := newDigestScheduler(pool, notifications, nil)
	scheduler.Now = func() time.Time { return time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC) }
	if err := scheduler.RunOnce(ctx); err != nil {
		t.Fatal("run scheduler with unsafe center URL:", err)
	}
	if got := countDigestEvents(t, ctx, pool); got != 0 {
		t.Fatalf("unsafe URL digest events = %d, want 0", got)
	}
	if got := countAllDigestDeliveries(t, ctx, pool); got != 0 {
		t.Fatalf("unsafe URL digest deliveries = %d, want 0", got)
	}
	if got := countCompletedDigestPeriods(t, ctx, pool); got != 2 {
		t.Fatalf("completed unsafe URL digest periods = %d, want 2", got)
	}
	updateDigestSettings(t, ctx, pool, notifications, func(view *settings.Settings) {
		view.CenterURL = "https://speedtest.example.test"
	})
	if err := scheduler.RunOnce(ctx); err != nil {
		t.Fatal("run scheduler after URL repair:", err)
	}
	if got := countDigestEvents(t, ctx, pool); got != 0 {
		t.Fatalf("repaired URL late digest events = %d, want 0", got)
	}
}

func openDigestNotificationPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	values, err := godotenv.Read(filepath.Join("..", "..", ".env"))
	if err != nil {
		t.Fatal("a configured worktree .env is required")
	}
	for key, value := range values {
		t.Setenv(key, value)
	}
	cfg, err := config.LoadCenter()
	if err != nil {
		t.Fatal("invalid worktree test configuration")
	}
	if err := db.CheckTestDatabase(cfg.Database.Name); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal("cannot connect to configured worktree database")
	}
	t.Cleanup(pool.Close)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	return pool, ctx
}

func configureDigestNotifications(t *testing.T, ctx context.Context, pool *pgxpool.Pool, emailOn, telegramOn bool) *notify.Store {
	t.Helper()
	key := []byte("0123456789abcdef0123456789abcdef")
	cipher, err := settings.NewCipherFromKey(key)
	if err != nil {
		t.Fatal("create integration settings cipher")
	}
	defaults := settings.Defaults("https://speedtest.example.test")
	defaults.Timezone = "UTC"
	defaults.NotifyQuietStart, defaults.NotifyQuietEnd = "", ""
	defaults.Notify.Email = settings.ChannelConfig{On: emailOn, To: "owner@example.test"}
	defaults.Notify.Telegram = settings.ChannelConfig{On: telegramOn, To: "10001"}
	raw, err := json.Marshal(defaults)
	if err != nil {
		t.Fatal("encode integration settings")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO settings(key,value,revision,updated_at) VALUES('display',$1::jsonb,1,now())`, raw); err != nil {
		t.Fatal("save integration settings")
	}
	encrypted, err := cipher.Encrypt([]byte(digestTestToken))
	if err != nil {
		t.Fatal("encrypt integration notification credential")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO settings(key,secret_ciphertext,updated_at) VALUES('notify.telegram.secret',$1,now())`, encrypted); err != nil {
		t.Fatal("save integration notification credential")
	}
	preferences := settings.NewPostgresStore(pool, cipher, defaults)
	return &notify.Store{DB: pool, Settings: preferences}
}

func updateDigestSettings(t *testing.T, ctx context.Context, pool *pgxpool.Pool, notifications *notify.Store, update func(*settings.Settings)) {
	t.Helper()
	view, _, err := notifications.Settings.NotificationConfig(ctx, nil, "telegram")
	if err != nil {
		t.Fatal("read digest settings:", err)
	}
	update(&view)
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal("encode digest settings:", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE settings SET value=$1::jsonb,revision=revision+1,updated_at=now() WHERE key='display'`, raw); err != nil {
		t.Fatal("update digest settings:", err)
	}
}

func insertDigestSummaryFixtures(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps)
		VALUES($1,'Node <alpha>' || chr(10) || 'forged',100,50)`, digestTestNodeID); err != nil {
		t.Fatal("insert digest node")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ip_addresses(ip) VALUES('203.0.113.124'::inet) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal("insert digest IP fixture")
	}
	insertDigestResult(t, ctx, pool, 1, "2026-11-17 10:00:00+00", "ok", 100, 40)
	insertDigestResult(t, ctx, pool, 2, "2026-11-17 11:00:00+00", "failed", 0, 0)
	insertDigestResult(t, ctx, pool, 3, "2026-11-11 10:00:00+00", "ok", 80, 20)
}

func insertDigestResult(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int, at, status string, down, up float64) {
	t.Helper()
	resultID := fmt.Sprintf("00000000-0000-4000-8000-%012x", id)
	_, err := pool.Exec(ctx, `INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,ip,threads,error,server,engine)
		VALUES($1::uuid,$2::uuid,$3::timestamptz,$4,CASE WHEN $4='failed' THEN NULL::double precision ELSE $5::double precision END,
		CASE WHEN $4='failed' THEN NULL::double precision ELSE $6::double precision END,10,1,'203.0.113.124'::inet,4,
		CASE WHEN $4='failed' THEN 'timeout' END,
		CASE WHEN $4='failed' THEN NULL ELSE '{"id":1,"name":"fixture","city":"test"}'::jsonb END,'ookla')`, resultID, digestTestNodeID, at, status, down, up)
	if err != nil {
		t.Fatalf("insert digest result: %v", err)
	}
}

func loadDigestEvents(t *testing.T, ctx context.Context, pool *pgxpool.Pool) map[string]digestQueuedEvent {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT e.event_key,e.payload,COALESCE(o.channels,ARRAY[]::text[])
		FROM alert_events e LEFT JOIN alert_notification_outbox o ON o.event_id=e.id
		WHERE e.event_key IN ('digest.daily','digest.weekly') ORDER BY e.event_key`)
	if err != nil {
		t.Fatal("load digest outbox events")
	}
	defer rows.Close()
	result := map[string]digestQueuedEvent{}
	for rows.Next() {
		var event digestQueuedEvent
		if err := rows.Scan(&event.key, &event.payload, &event.channels); err != nil {
			t.Fatal("scan digest outbox event")
		}
		result[event.key] = event
	}
	if err := rows.Err(); err != nil {
		t.Fatal("read digest outbox events")
	}
	return result
}

func assertDigestQueuedEvent(t *testing.T, event digestQueuedEvent, channels []string, summary, link string) {
	t.Helper()
	if strings.Join(event.channels, ",") != strings.Join(channels, ",") {
		t.Errorf("%s outbox channels = %v, want %v", event.key, event.channels, channels)
	}
	var payload map[string]string
	if err := json.Unmarshal(event.payload, &payload); err != nil {
		t.Fatalf("%s payload is not valid JSON", event.key)
	}
	if summary != "" && !strings.Contains(payload["summary"], summary) {
		t.Errorf("%s summary %q does not contain %q", event.key, payload["summary"], summary)
	}
	if link != "" && payload["link"] != link {
		t.Errorf("%s dashboard link = %q, want %q", event.key, payload["link"], link)
	}
	if strings.Contains(payload["summary"], "\nforged") {
		t.Errorf("%s summary contains an injected message line", event.key)
	}
}

func assertDigestPayloadHasNoCredential(t *testing.T, payload []byte) {
	t.Helper()
	digest := sha256.Sum256([]byte(digestTestToken))
	if strings.Contains(string(payload), digestTestToken) || strings.Contains(string(payload), fmt.Sprintf("%x", digest)) {
		t.Fatal("digest event payload contains a notification credential")
	}
}

func consumeDigestOutbox(t *testing.T, ctx context.Context, store *notify.Store) int {
	t.Helper()
	consumed := 0
	for range 3 {
		didConsume, err := store.Consume(ctx)
		if err != nil {
			t.Fatal("consume digest outbox")
		}
		if !didConsume {
			return consumed
		}
		consumed++
	}
	return consumed
}

func dispatchDigestDeliveries(t *testing.T, ctx context.Context, store *notify.Store, limit int) int {
	t.Helper()
	dispatched := 0
	for range limit {
		didDispatch, err := store.Dispatch(ctx)
		if err != nil {
			t.Fatal("dispatch digest notification")
		}
		if !didDispatch {
			return dispatched
		}
		dispatched++
	}
	return dispatched
}

func assertCapturedDigestMessage(t *testing.T, messages []string, summary, link string) {
	t.Helper()
	for _, message := range messages {
		if strings.Contains(message, summary) && strings.Contains(message, link) {
			return
		}
	}
	t.Fatalf("no rendered message contains %q and %q: %v", summary, link, messages)
}

func countDigestEvents(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM alert_events WHERE event_key IN ('digest.daily','digest.weekly')`).Scan(&count); err != nil {
		t.Fatal("count digest events")
	}
	return count
}

func countEmailDeliveries(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notification_deliveries WHERE channel='email'`).Scan(&count); err != nil {
		t.Fatal("count e-mail deliveries")
	}
	return count
}

func countCompletedDigestPeriods(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM digest_periods WHERE completed_at IS NOT NULL`).Scan(&count); err != nil {
		t.Fatal("count completed digest periods:", err)
	}
	return count
}

func countAllDigestDeliveries(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notification_deliveries`).Scan(&count); err != nil {
		t.Fatal("count digest deliveries:", err)
	}
	return count
}
