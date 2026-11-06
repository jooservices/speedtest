//go:build integration

package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/settings"
	"github.com/jooservices/speedtest/internal/shared/config"
)

func database(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	env, err := godotenv.Read("../../../.env")
	if err != nil {
		t.Fatal("worktree .env required")
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	cfg, err := config.LoadCenter()
	if err != nil || db.CheckTestDatabase(cfg.Database.Name) != nil {
		t.Fatal("configured non-live test database required")
	}
	ctx := t.Context()
	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal("cannot open worktree database")
	}
	t.Cleanup(pool.Close)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	return pool, ctx
}
func execute(t *testing.T, p *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := p.Exec(t.Context(), sql, args...); err != nil {
		t.Fatal(err)
	}
}
func preferences(t *testing.T, p *pgxpool.Pool, url string) *settings.PostgresStore {
	t.Helper()
	cipher, err := settings.NewCipherFromKey([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	view := settings.Defaults("https://dashboard.example")
	view.Notify.Webhook = settings.ChannelConfig{On: true, Configured: true}
	raw := []byte(`{"theme":"dark","centerUrl":"https://dashboard.example","notify":{"webhook":{"on":true}}}`)
	execute(t, p, `INSERT INTO settings(key,value) VALUES('display',$1)`, raw)
	encrypted, err := cipher.Encrypt([]byte(url))
	if err != nil {
		t.Fatal(err)
	}
	execute(t, p, `INSERT INTO settings(key,secret_ciphertext) VALUES('notify.webhook.secret',$1)`, encrypted)
	return settings.NewPostgresStore(p, cipher, view)
}

func TestOutboxExactlyOnceConcurrentRestartAndRollback(t *testing.T) {
	p, ctx := database(t)
	execute(t, p, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES('10000000-0000-4000-8000-000000000001','Fiber',100,50)`)
	execute(t, p, `INSERT INTO alert_events(id,node_id,severity,ts,title) VALUES('a0000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000001','warn',now(),'Speed low')`)
	execute(t, p, `INSERT INTO alert_notification_outbox(event_id,channels) VALUES('a0000000-0000-4000-8000-000000000001',ARRAY['email','telegram','webhook','webhook'])`)
	worker := &Store{DB: p}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			if _, err := worker.Consume(ctx); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	var count int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM notification_deliveries`).Scan(&count); err != nil || count != 3 {
		t.Fatal("exactly once channel rows", count, err)
	}
	if ok, err := (&Store{DB: p}).Consume(ctx); ok || err != nil {
		t.Fatal("restart replay", err)
	}
	execute(t, p, `UPDATE alert_notification_outbox SET consumed_at=NULL,channels=ARRAY['slack','invalid']`)
	if _, err := worker.Consume(ctx); err == nil {
		t.Fatal("expected atomic constraint failure")
	}
	var consumed *time.Time
	if err := p.QueryRow(ctx, `SELECT consumed_at FROM alert_notification_outbox`).Scan(&consumed); err != nil || consumed != nil {
		t.Fatal("failed consume marked complete")
	}
	if err := p.QueryRow(ctx, `SELECT count(*) FROM notification_deliveries WHERE channel='slack'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial channel insert survived rollback")
	}
	items, total, err := worker.Deliveries(ctx, "A0000000-0000-4000-8000-000000000001", 1, 25)
	if err != nil || total != 3 || len(items) != 3 {
		t.Fatal("event UUID filter must be case insensitive", err)
	}
	t.Log("PASS: concurrent/restarted consumers create one delivery per event/channel; failed fanout rolls back")
}

func TestDeliveryRetryLifecycle(t *testing.T) {
	p, ctx := database(t)
	var count atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		if fail.Load() {
			http.Error(w, "secret-token private URL", 500)
			return
		}
		w.WriteHeader(204)
	}))
	defer remote.Close()
	prefs := preferences(t, p, remote.URL+"/secret-token")
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	var logs bytes.Buffer
	worker := &Store{DB: p, Settings: prefs, Sender: &Transport{}, Now: func() time.Time { return now }, Logger: slog.New(slog.NewTextHandler(&logs, nil))}
	d, err := worker.Test(ctx, "webhook")
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != "pending" || d.EventID != nil || d.Attempts != 0 {
		t.Fatalf("test delivery=%+v", d)
	}
	ok, err := worker.Dispatch(ctx)
	if !ok || err != nil {
		t.Fatal("first attempt", err)
	}
	d, _ = worker.Delivery(ctx, d.ID)
	if d.Attempts != 1 || d.Status != "pending" || d.Error == nil {
		t.Fatal("retry metadata")
	}
	ok, err = worker.Dispatch(ctx)
	if ok || err != nil {
		t.Fatal("premature retry")
	}
	now = now.Add(time.Minute)
	fail.Store(false)
	ok, err = worker.Dispatch(ctx)
	if !ok || err != nil {
		t.Fatal("retry", err)
	}
	d, _ = worker.Delivery(ctx, d.ID)
	if d.Status != "sent" || d.Attempts != 2 || d.Error != nil {
		t.Fatal("sent metadata")
	}
	if count.Load() != 2 {
		t.Fatal("send count")
	}
	expired, err := worker.Test(ctx, "webhook")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(24 * time.Hour)
	if _, err = worker.Dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	expired, _ = worker.Delivery(ctx, expired.ID)
	if expired.Status != "failed" || expired.Attempts != 0 {
		t.Fatal("expiry must not send")
	}
	items, total, err := worker.Deliveries(ctx, "", 1, 1)
	if err != nil || total != 2 || len(items) != 1 || items[0].ID != expired.ID {
		t.Fatal("pagination/newest first")
	}
	empty, total, err := worker.Deliveries(ctx, "20000000-0000-4000-8000-000000000001", 1, 25)
	if err != nil || total != 0 || len(empty) != 0 {
		t.Fatal("event filter")
	}
	if _, err := worker.Delivery(ctx, "30000000-0000-4000-8000-000000000001"); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing delivery")
	}
	for _, page := range []int{0, 1000001} {
		if _, _, err := worker.Deliveries(ctx, "", page, 25); err == nil {
			t.Fatal("invalid pagination")
		}
	}
	if strings.Contains(logs.String(), "secret-token") || strings.Contains(logs.String(), remote.URL) {
		t.Fatal("secret in logs")
	}
	if _, err := prefs.NotificationSecret(ctx, "bad"); err == nil {
		t.Fatal("unknown secret")
	}
	if secret, err := prefs.NotificationSecret(ctx, "email"); err != nil || secret != "" {
		t.Fatal("absent credential")
	}
	execute(t, p, `UPDATE settings SET secret_ciphertext='broken' WHERE key='notify.webhook.secret'`)
	if _, err := prefs.NotificationSecret(ctx, "webhook"); err == nil {
		t.Fatal("corrupt ciphertext")
	}
}

func TestSharedMailpitSMTP(t *testing.T) {
	env, err := godotenv.Read("../../../.env")
	if err != nil {
		t.Fatal("worktree environment required")
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	cfg, err := config.LoadCenter()
	if err != nil {
		t.Fatal("configuration")
	}
	// Only the shared development Mailpit listener; never a real SMTP account.
	if cfg.SMTPAddr != "127.0.0.1:11025" {
		t.Fatal("Mailpit test requires the loopback shared development SMTP address")
	}
	conn, err := net.DialTimeout("tcp", cfg.SMTPAddr, time.Second)
	if err != nil {
		t.Skip("shared Mailpit unreachable; no real SMTP account used")
	}
	_ = conn.Close()
	tr := &Transport{SMTPAddr: cfg.SMTPAddr, SMTPFrom: cfg.SMTPFrom}
	if err := tr.Send(t.Context(), "email", "notification-test@example.com", "", Message("Test notification", "Mailpit fixture", time.Now(), "https://dashboard.example")); err != nil {
		t.Fatal(err)
	}
	t.Log("PASS: shared Mailpit accepted plain-text notification")
}

func TestConcurrentDispatchAndLoop(t *testing.T) {
	p, ctx := database(t)
	var calls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(204) }))
	defer remote.Close()
	prefs := preferences(t, p, remote.URL)
	worker := &Store{DB: p, Settings: prefs, Sender: &Transport{}}
	d, err := worker.Test(ctx, "webhook")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			if _, err := worker.Dispatch(ctx); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("concurrent duplicate send")
	}
	d, _ = worker.Delivery(ctx, d.ID)
	if d.Status != "sent" {
		t.Fatal("dispatch")
	}
	_, err = worker.Test(ctx, "webhook")
	if err != nil {
		t.Fatal(err)
	}
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { worker.Loop(loopCtx); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for calls.Load() != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("loop did not stop")
	}
	if calls.Load() != 2 {
		t.Fatal("loop did not drain")
	}
}

func TestNotificationMigrationRollback(t *testing.T) {
	p, ctx := database(t)
	execute(t, p, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES('10000000-0000-4000-8000-000000000003','Rollback fixture',100,50)`)
	execute(t, p, `INSERT INTO alert_events(id,node_id,severity,ts,title) VALUES('20000000-0000-4000-8000-000000000003','10000000-0000-4000-8000-000000000003','warn',now(),'Rollback event')`)
	execute(t, p, `INSERT INTO alert_notification_outbox(event_id,channels) VALUES('20000000-0000-4000-8000-000000000003',ARRAY['webhook'])`)
	worker := &Store{DB: p}
	if _, err := worker.Consume(ctx); err != nil {
		t.Fatal(err)
	}
	// Later migrations (for example the SSH installer) sit above this one; roll back until the delivery table is gone.
	var exists bool
	for step := 0; step < 4; step++ {
		if err := db.Migrate(ctx, p, "down"); err != nil {
			t.Fatal("rollback step", step, err)
		}
		if err := p.QueryRow(ctx, `SELECT to_regclass('notification_deliveries') IS NOT NULL`).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			break
		}
	}
	if exists {
		t.Fatal("delivery table not dropped")
	}
	var consumed bool
	if err := p.QueryRow(ctx, `SELECT consumed_at IS NOT NULL FROM alert_notification_outbox`).Scan(&consumed); err != nil || !consumed {
		t.Fatal("outbox marker not preserved")
	}
	if err := db.Migrate(ctx, p, "up"); err != nil {
		t.Fatal("version 6 up", err)
	}
	if ok, err := worker.Consume(ctx); ok || err != nil {
		t.Fatal("rollback replayed consumed event")
	}
	items, total, err := worker.Deliveries(ctx, "", 1, 25)
	if err != nil || total != 0 || len(items) != 0 {
		t.Fatal("reapplied empty delivery log")
	}
	t.Log("PASS: populated version 6 down/up preserves alert/outbox and does not replay consumed events")
}

func TestStorageFailuresAreRetriableAndSafe(t *testing.T) {
	p, ctx := database(t)
	worker := &Store{DB: p, Settings: preferenceStub{view: settings.Settings{Notify: settings.NotificationSettings{Webhook: settings.ChannelConfig{On: true}}}, secret: "private-token"}}
	// Missing storage must fail without manufacturing a successful enqueue/read.
	execute(t, p, `ALTER TABLE notification_deliveries RENAME TO temporarily_unavailable_deliveries`)
	if _, err := worker.Test(ctx, "webhook"); err == nil {
		t.Fatal("failed enqueue was successful")
	}
	if _, _, err := worker.Deliveries(ctx, "", 1, 25); err == nil {
		t.Fatal("failed list was successful")
	}
	if _, err := worker.Dispatch(ctx); err == nil {
		t.Fatal("failed dispatch query was successful")
	}
	execute(t, p, `ALTER TABLE temporarily_unavailable_deliveries RENAME TO notification_deliveries`)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := worker.Consume(cancelled); err == nil {
		t.Fatal("closed pool consume")
	}
	if _, err := worker.Dispatch(cancelled); err == nil {
		t.Fatal("closed pool dispatch")
	}
}

type senderFunc func(context.Context, string, string, string, string) error

func (f senderFunc) Send(ctx context.Context, ch, to, secret, message string) error {
	return f(ctx, ch, to, secret, message)
}
func TestAttemptDeadlineAndConfigurationRepair(t *testing.T) {
	p, ctx := database(t)
	prefs := preferences(t, p, "http://127.0.0.1:1/private")
	now := time.Now().UTC()
	var gotDeadline bool
	worker := &Store{DB: p, Settings: prefs, Now: func() time.Time { return now }, Sender: senderFunc(func(ctx context.Context, _, _, _, _ string) error {
		d, ok := ctx.Deadline()
		gotDeadline = ok && time.Until(d) > 0 && time.Until(d) <= 10*time.Second
		return errors.New("private transport URL and credential")
	})}
	d, err := worker.Test(ctx, "webhook")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.Dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	if !gotDeadline {
		t.Fatal("attempt deadline not propagated to sender")
	}
	d, _ = worker.Delivery(ctx, d.ID)
	if d.Error == nil || *d.Error != "notification attempt failed" {
		t.Fatal("unsafe delivery error")
	}
	execute(t, p, `UPDATE settings SET value=jsonb_set(value,'{notify,webhook,on}','false') WHERE key='display'`)
	if _, err := worker.Test(ctx, "webhook"); !errors.Is(err, ErrUnconfigured) {
		t.Fatal("disabled channel accepted test")
	}
	now = now.Add(time.Minute)
	if _, err := worker.Dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	d, _ = worker.Delivery(ctx, d.ID)
	if d.Attempts != 2 || d.Status != "pending" {
		t.Fatal("disabled channel retry")
	}
	execute(t, p, `UPDATE settings SET value=jsonb_set(value,'{notify,webhook,on}','true') WHERE key='display'`)
	worker.Sender = senderFunc(func(context.Context, string, string, string, string) error { return nil })
	now = now.Add(2 * time.Minute)
	if _, err := worker.Dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	d, _ = worker.Delivery(ctx, d.ID)
	if d.Status != "sent" || d.Error != nil || d.Attempts != 3 {
		t.Fatal("repaired channel was not retried")
	}
}

func TestWorkersDoNotStarveSmallConnectionPool(t *testing.T) {
	p, ctx := database(t)
	cfg := p.Config().Copy()
	cfg.MaxConns = 2
	limited, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(limited.Close)
	var count atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { count.Add(1); w.WriteHeader(204) }))
	defer remote.Close()
	prefs := preferences(t, limited, remote.URL)
	worker := &Store{DB: limited, Settings: prefs, Sender: &Transport{}}
	for i := 0; i < 8; i++ {
		if _, err := worker.Test(ctx, "webhook"); err != nil {
			t.Fatal(err)
		}
	}
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			if _, err := worker.Dispatch(bounded); err != nil {
				t.Error("small pool dispatch", err)
			}
		})
	}
	wg.Wait()
	if count.Load() != 8 {
		t.Fatalf("small pool sent %d, want 8", count.Load())
	}
	t.Log("PASS: eight concurrent dispatchers send through a two-connection pool without starvation")
}

// Quiet hours hold alert deliveries (not test messages), let critical alerts through when allowed, and release the
// held ones once the window is over.
func TestQuietHoursHoldAlertDeliveries(t *testing.T) {
	p, ctx := database(t)
	var sent atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sent.Add(1); w.WriteHeader(204) }))
	defer remote.Close()
	prefs := preferences(t, p, remote.URL+"/hook")
	execute(t, p, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES('10000000-0000-4000-8000-000000000001','Fiber',100,50)`)
	for i, sev := range []string{"warn", "crit"} {
		id := fmt.Sprintf("a0000000-0000-4000-8000-00000000000%d", i+1)
		execute(t, p, `INSERT INTO alert_events(id,node_id,severity,ts,title) VALUES($1,'10000000-0000-4000-8000-000000000001',$2,now(),'Alert')`, id, sev)
		execute(t, p, `INSERT INTO alert_notification_outbox(event_id,channels) VALUES($1,ARRAY['webhook'])`, id)
	}
	execute(t, p, `INSERT INTO settings(key,value) VALUES ('notifyQuietStart','"22:00"'),('notifyQuietEnd','"07:00"'),('notifyQuietTimezone','"Asia/Ho_Chi_Minh"'),('notifyQuietCritical','true')`)
	night := time.Date(2026, 11, 3, 18, 0, 0, 0, time.UTC) // 01:00 in Ho Chi Minh City
	now := night
	worker := &Store{DB: p, Settings: prefs, Sender: &Transport{}, Now: func() time.Time { return now }}
	for range 2 {
		if _, err := worker.Consume(ctx); err != nil {
			t.Fatal(err)
		}
	}
	test, err := worker.Test(ctx, "webhook")
	if err != nil {
		t.Fatal(err)
	}
	for range 3 { // critical alert and the test message go out, the warning waits
		if _, err := worker.Dispatch(ctx); err != nil {
			t.Fatal(err)
		}
	}
	status := func(event string) string {
		var s string
		if err := p.QueryRow(ctx, `SELECT status FROM notification_deliveries WHERE event_id=$1`, event).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if status("a0000000-0000-4000-8000-000000000002") != "sent" || sent.Load() != 2 {
		t.Fatalf("critical alert and test message are delivered at night: sent=%d", sent.Load())
	}
	if d, _ := worker.Delivery(ctx, test.ID); d.Status != "sent" {
		t.Fatalf("a test message is never held: %+v", d)
	}
	if status("a0000000-0000-4000-8000-000000000001") != "pending" {
		t.Fatal("the warning waits for the quiet hours to end")
	}
	// Still night a minute later: still held, nothing sent. After the window it goes out.
	now = night.Add(time.Minute)
	_, _ = worker.Dispatch(ctx)
	if status("a0000000-0000-4000-8000-000000000001") != "pending" || sent.Load() != 2 {
		t.Fatal("still held during the window")
	}
	now = time.Date(2026, 11, 3, 0, 30, 0, 0, time.UTC) // 07:30 local, window over (and within 24 h of creation)
	execute(t, p, `UPDATE notification_deliveries SET created_at=$1, next_attempt_at=$1 WHERE status='pending'`, now.Add(-time.Hour))
	if _, err := worker.Dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	if status("a0000000-0000-4000-8000-000000000001") != "sent" || sent.Load() != 3 {
		t.Fatalf("released after the window: sent=%d", sent.Load())
	}
	// With the critical exemption off, a critical alert waits as well.
	execute(t, p, `UPDATE settings SET value='false' WHERE key='notifyQuietCritical'`)
	held, err := heldByQuietHours(ctx, p, "a0000000-0000-4000-8000-000000000002", night)
	if err != nil || !held {
		t.Fatalf("critical alerts wait when the exemption is off: held=%v err=%v", held, err)
	}
	if held, _ := heldByQuietHours(ctx, p, "a0000000-0000-4000-8000-000000000002", now); held {
		t.Fatal("nothing is held outside the window")
	}
}

func TestNotificationLoopObserverIntegration(t *testing.T) {
	p, ctx := database(t)
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	var outcomes []bool
	store := &Store{DB: p, ObserveRun: func(ok bool) { outcomes = append(outcomes, ok); cancel() }}
	store.Loop(run)
	if len(outcomes) != 1 || !outcomes[0] {
		t.Fatal(outcomes)
	}
	outcomes = nil
	store.Loop(run)
	if len(outcomes) != 1 || outcomes[0] {
		t.Fatal("cancelled pass advanced success", outcomes)
	}
}
