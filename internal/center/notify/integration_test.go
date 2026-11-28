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

	"github.com/jooservices/speedtest/internal/shared/testenv"

	"github.com/jackc/pgx/v5"
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
	testenv.Apply(t, env)
	cfg, err := config.LoadCenter()
	if err != nil || db.CheckDestructiveAllowed(cfg.AppEnv) != nil {
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
	items, total, err := worker.Deliveries(ctx, DeliveryFilter{Event: "A0000000-0000-4000-8000-000000000001"}, 1, 25)
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
	items, total, err := worker.Deliveries(ctx, DeliveryFilter{}, 1, 1)
	if err != nil || total != 2 || len(items) != 1 || items[0].ID != expired.ID {
		t.Fatal("pagination/newest first")
	}
	empty, total, err := worker.Deliveries(ctx, DeliveryFilter{Event: "20000000-0000-4000-8000-000000000001"}, 1, 25)
	if err != nil || total != 0 || len(empty) != 0 {
		t.Fatal("event filter")
	}
	if _, err := worker.Delivery(ctx, "30000000-0000-4000-8000-000000000001"); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing delivery")
	}
	for _, page := range []int{0, 1000001} {
		if _, _, err := worker.Deliveries(ctx, DeliveryFilter{}, page, 25); err == nil {
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
	testenv.Apply(t, env)
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
	// Roll back to the version immediately before notification deliveries.
	var exists bool
	if err := db.MigrateDownTo(ctx, p, 5); err != nil {
		t.Fatal("roll back through notification deliveries migration", err)
	}
	if err := p.QueryRow(ctx, `SELECT to_regclass('notification_deliveries') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
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
	items, total, err := worker.Deliveries(ctx, DeliveryFilter{}, 1, 25)
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
	if _, _, err := worker.Deliveries(ctx, DeliveryFilter{}, 1, 25); err == nil {
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
	execute(t, p, `INSERT INTO settings(key,value) VALUES ('notifyQuietStart','"22:00"'),('notifyQuietEnd','"07:00"'),('notifyQuietTimezone','"Asia/Ho_Chi_Minh"'),('notifyQuietCritical','true')
		ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`)
	night := time.Date(2026, 11, 3, 18, 0, 0, 0, time.UTC) // 01:00 in Ho Chi Minh City
	now := night
	worker := &Store{DB: p, Settings: prefs, Sender: &Transport{}, Now: func() time.Time { return now }}
	for range 2 {
		if _, err := worker.Consume(ctx); err != nil {
			t.Fatal(err)
		}
	}
	channels := []string{"webhook"}
	if _, err := (&EventStore{DB: p}).UpdateEvent(ctx, "test.success", EventPatch{Channels: channels}); err != nil {
		t.Fatal("route template test to webhook", err)
	}
	test, err := worker.TestNotificationEvent(ctx, "test.success", "webhook")
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

// statementTrace records the SQL a pool sends, so a test can see whether an idle
// check opened a transaction (BEGIN and ROLLBACK are statements too).
type statementTrace struct {
	mu  sync.Mutex
	sql []string
}

func (s *statementTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sql = append(s.sql, data.SQL)
	return ctx
}

func (s *statementTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// take returns the statements recorded since the previous take and clears them.
func (s *statementTrace) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.sql
	s.sql = nil
	return out
}

// testPool opens a second pool on the test database as database() does. A nil trace
// adds no tracer, and a zero maxConns keeps the driver default.
func testPool(t *testing.T, trace pgx.QueryTracer, maxConns int32) *pgxpool.Pool {
	t.Helper()
	env, err := godotenv.Read("../../../.env")
	if err != nil {
		t.Fatal("worktree .env required")
	}
	testenv.Apply(t, env)
	cfg, err := config.LoadCenter()
	if err != nil || db.CheckDestructiveAllowed(cfg.AppEnv) != nil {
		t.Fatal("configured non-live test database required")
	}
	poolCfg, err := pgxpool.ParseConfig(cfg.Database.DSN())
	if err != nil {
		t.Fatal("invalid test database configuration")
	}
	if trace != nil {
		poolCfg.ConnConfig.Tracer = trace
	}
	if maxConns > 0 {
		poolCfg.MaxConns = maxConns
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), poolCfg)
	if err != nil {
		t.Fatal("cannot open test pool")
	}
	t.Cleanup(pool.Close)
	return pool
}

// rollbacks returns xact_rollback of the test database. It first flushes the pending
// statistics of the pool's connection, because PostgreSQL publishes them with a delay.
func rollbacks(t *testing.T, p *pgxpool.Pool) int64 {
	t.Helper()
	if _, err := p.Exec(t.Context(), `SELECT pg_stat_force_next_flush()`); err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := p.QueryRow(t.Context(), `SELECT xact_rollback FROM pg_stat_database WHERE datname=current_database()`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// assertProbesOnly fails unless statements are exactly want single-statement probes
// and none of them opens or ends a transaction.
func assertProbesOnly(t *testing.T, statements []string, want int) {
	t.Helper()
	if len(statements) != want {
		t.Fatalf("idle check issued %d statements %q, want %d", len(statements), statements, want)
	}
	for _, sql := range statements {
		if !strings.HasPrefix(sql, "SELECT EXISTS(") {
			t.Fatalf("idle check issued %q; want only the probe, no transaction", sql)
		}
	}
}

// AC1: with both queues empty, 100 idle Consume and 100 idle Dispatch calls add no rollback.
func TestAC1IdleQueueChecksAddNoRollback(t *testing.T) {
	_, ctx := database(t)
	pool := testPool(t, nil, 1)
	worker := &Store{DB: pool}
	before := rollbacks(t, pool)
	for i := 0; i < 100; i++ {
		if consumed, err := worker.Consume(ctx); consumed || err != nil {
			t.Fatal("AC1 idle consume", consumed, err)
		}
		if dispatched, err := worker.Dispatch(ctx); dispatched || err != nil {
			t.Fatal("AC1 idle dispatch", dispatched, err)
		}
	}
	if after := rollbacks(t, pool); after != before {
		t.Fatalf("AC1 idle checks added %d rollbacks, want 0", after-before)
	}
}

// AC2: an idle Consume or Dispatch issues exactly one statement, the existence check, and no transaction control.
func TestAC2IdleConsumeAndDispatchIssueOnlyTheExistenceCheck(t *testing.T) {
	_, ctx := database(t)
	var trace statementTrace
	pool := testPool(t, &trace, 0)
	trace.take()
	worker := &Store{DB: pool}

	if consumed, err := worker.Consume(ctx); consumed || err != nil {
		t.Fatal("AC2 idle consume", consumed, err)
	}
	assertProbesOnly(t, trace.take(), 1)
	if dispatched, err := worker.Dispatch(ctx); dispatched || err != nil {
		t.Fatal("AC2 idle dispatch", dispatched, err)
	}
	assertProbesOnly(t, trace.take(), 1)
}

// AC3: one outbox event with two channels and one due delivery: two delivery rows, consumed_at set once,
// the delivery result and retry fields as recorded, and a second Consume returns (false, nil).
func TestAC3FanOutAndDispatchKeepResultAndRetryFields(t *testing.T) {
	p, ctx := database(t)
	var count atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(204)
	}))
	defer remote.Close()
	prefs := preferences(t, p, remote.URL+"/secret-token")
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	worker := &Store{DB: p, Settings: prefs, Sender: &Transport{}, Now: func() time.Time { return now }}
	execute(t, p, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES('10000000-0000-4000-8000-000000000003','Fanout',100,50)`)
	execute(t, p, `INSERT INTO alert_events(id,node_id,severity,ts,title) VALUES('a0000000-0000-4000-8000-000000000003','10000000-0000-4000-8000-000000000003','warn',now(),'Fan out')`)
	execute(t, p, `INSERT INTO alert_notification_outbox(event_id,channels) VALUES('a0000000-0000-4000-8000-000000000003',ARRAY['webhook','email'])`)

	if ok, err := worker.Consume(ctx); !ok || err != nil {
		t.Fatal("AC3 consume", ok, err)
	}
	var rows int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM notification_deliveries WHERE event_id=$1`, "a0000000-0000-4000-8000-000000000003").Scan(&rows); err != nil || rows != 2 {
		t.Fatal("AC3 delivery rows", rows, err)
	}
	var consumedAt time.Time
	if err := p.QueryRow(ctx, `SELECT consumed_at FROM alert_notification_outbox WHERE event_id=$1`, "a0000000-0000-4000-8000-000000000003").Scan(&consumedAt); err != nil {
		t.Fatal(err)
	}
	if ok, err := worker.Consume(ctx); ok || err != nil {
		t.Fatal("AC3 second consume must return (false, nil)", ok, err)
	}
	var again time.Time
	if err := p.QueryRow(ctx, `SELECT consumed_at FROM alert_notification_outbox WHERE event_id=$1`, "a0000000-0000-4000-8000-000000000003").Scan(&again); err != nil || !again.Equal(consumedAt) {
		t.Fatal("AC3 consumed_at changed after the second consume", err)
	}

	for i := 0; i < 2; i++ {
		if ok, err := worker.Dispatch(ctx); !ok || err != nil {
			t.Fatal("AC3 dispatch", i, ok, err)
		}
	}
	if ok, err := worker.Dispatch(ctx); ok || err != nil {
		t.Fatal("AC3 nothing is due after both attempts", ok, err)
	}
	if count.Load() != 1 {
		t.Fatal("AC3 webhook send count", count.Load())
	}
	type row struct {
		status, channel string
		attempts        int
		errText         *string
		next            time.Time
	}
	rowsOut, err := p.Query(ctx, `SELECT channel,status,attempts,error,next_attempt_at FROM notification_deliveries WHERE event_id=$1 ORDER BY channel`, "a0000000-0000-4000-8000-000000000003")
	if err != nil {
		t.Fatal(err)
	}
	var got []row
	for rowsOut.Next() {
		var r row
		if err := rowsOut.Scan(&r.channel, &r.status, &r.attempts, &r.errText, &r.next); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	rowsOut.Close()
	if len(got) != 2 {
		t.Fatal("AC3 rows", got)
	}
	email, webhook := got[0], got[1]
	if webhook.channel != "webhook" || webhook.status != "sent" || webhook.attempts != 1 || webhook.errText != nil {
		t.Fatalf("AC3 webhook result %+v", webhook)
	}
	if email.channel != "email" || email.status != "pending" || email.attempts != 1 || email.errText == nil || *email.errText != "notification attempt failed" {
		t.Fatalf("AC3 email retry result %+v", email)
	}
	if want := now.Add(Backoff(1)); !email.next.Equal(want) {
		t.Fatalf("AC3 email next attempt %v, want %v", email.next, want)
	}
}

// AC4: two concurrent Consume calls on one outbox row: exactly one returns true and one set of delivery rows exists.
func TestAC4ConcurrentConsumeCreatesOneSetOfDeliveries(t *testing.T) {
	p, ctx := database(t)
	execute(t, p, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES('10000000-0000-4000-8000-000000000004','Race',100,50)`)
	execute(t, p, `INSERT INTO alert_events(id,node_id,severity,ts,title) VALUES('a0000000-0000-4000-8000-000000000004','10000000-0000-4000-8000-000000000004','warn',now(),'Race')`)
	execute(t, p, `INSERT INTO alert_notification_outbox(event_id,channels) VALUES('a0000000-0000-4000-8000-000000000004',ARRAY['webhook','email'])`)
	worker := &Store{DB: p}
	var wg sync.WaitGroup
	var claimed atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			ok, err := worker.Consume(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			if ok {
				claimed.Add(1)
			}
		})
	}
	wg.Wait()
	if claimed.Load() != 1 {
		t.Fatal("AC4 consumes that returned true", claimed.Load())
	}
	var rows int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM notification_deliveries WHERE event_id='a0000000-0000-4000-8000-000000000004'`).Scan(&rows); err != nil || rows != 2 {
		t.Fatal("AC4 delivery rows", rows, err)
	}
}

// AC4: eight concurrent Dispatch calls on one due delivery: exactly one returns true and one send happens.
func TestAC4ConcurrentDispatchSendsDeliveryOnce(t *testing.T) {
	p, ctx := database(t)
	var count atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(204)
	}))
	defer remote.Close()
	prefs := preferences(t, p, remote.URL+"/secret-token")
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	worker := &Store{DB: p, Settings: prefs, Sender: &Transport{}, Now: func() time.Time { return now }}
	if _, err := worker.Test(ctx, "webhook"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var claimed atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			ok, err := worker.Dispatch(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			if ok {
				claimed.Add(1)
			}
		})
	}
	wg.Wait()
	if claimed.Load() != 1 || count.Load() != 1 {
		t.Fatal("AC4 claims", claimed.Load(), count.Load())
	}
}

// AC4: the probe error is returned, and the Loop pass is reported as failed.
func TestAC4ProbeFailureReportsFailedPass(t *testing.T) {
	p, ctx := database(t)
	execute(t, p, `ALTER TABLE alert_notification_outbox RENAME TO temporarily_unavailable_outbox`)
	worker := &Store{DB: p}
	if _, err := worker.Consume(ctx); err == nil {
		t.Fatal("AC4 probe failure was reported as an idle queue")
	}
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	var outcomes []bool
	loop := &Store{DB: p, ObserveRun: func(ok bool) { outcomes = append(outcomes, ok); cancel() }}
	loop.Loop(run)
	execute(t, p, `ALTER TABLE temporarily_unavailable_outbox RENAME TO alert_notification_outbox`)
	if len(outcomes) != 1 || outcomes[0] {
		t.Fatal("AC4 failed probe pass was not reported as failed", outcomes)
	}
}

// AC5: a cancelled context makes Consume and Dispatch return the context error, and no
// session of the database is left idle in transaction.
func TestAC5CancelledContextReturnsErrorAndLeavesNoIdleTransaction(t *testing.T) {
	p, ctx := database(t)
	worker := &Store{DB: p}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := worker.Consume(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatal("AC5 consume before cancel", err)
	}
	if _, err := worker.Dispatch(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatal("AC5 dispatch before cancel", err)
	}
	for i := 0; i < 50; i++ {
		during, stop := context.WithCancel(ctx)
		go stop()
		if _, err := worker.Consume(during); err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal("AC5 consume during cancel", err)
		}
		if _, err := worker.Dispatch(during); err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal("AC5 dispatch during cancel", err)
		}
		stop()
	}
	var idle int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND state='idle in transaction'`).Scan(&idle); err != nil || idle != 0 {
		t.Fatal("AC5 sessions idle in transaction", idle, err)
	}
}

// AC6: with a running Loop and an empty queue, an event inserted later gets its delivery rows and is dispatched within 2 seconds.
func TestAC6RunningLoopDispatchesLateEventWithinTwoSeconds(t *testing.T) {
	p, ctx := database(t)
	var count atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(204)
	}))
	defer remote.Close()
	prefs := preferences(t, p, remote.URL+"/secret-token")
	worker := &Store{DB: p, Settings: prefs, Sender: &Transport{}}
	run, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.Loop(run)
	}()
	time.Sleep(1200 * time.Millisecond)
	execute(t, p, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES('10000000-0000-4000-8000-000000000006','Loop',100,50)`)
	execute(t, p, `INSERT INTO alert_events(id,node_id,severity,ts,title) VALUES('a0000000-0000-4000-8000-000000000006','10000000-0000-4000-8000-000000000006','crit',now(),'Loop')`)
	start := time.Now()
	execute(t, p, `INSERT INTO alert_notification_outbox(event_id,channels) VALUES('a0000000-0000-4000-8000-000000000006',ARRAY['webhook'])`)
	var status string
	for time.Since(start) < 2*time.Second {
		var n int
		if err := p.QueryRow(ctx, `SELECT count(*), coalesce(min(status),'') FROM notification_deliveries WHERE event_id='a0000000-0000-4000-8000-000000000006'`).Scan(&n, &status); err != nil {
			t.Fatal(err)
		}
		if status == "sent" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if status != "sent" || count.Load() != 1 {
		t.Fatal("AC6 late event not dispatched within 2s", status, count.Load())
	}
}

// AC6: a row that arrives after an empty probe is handled on the next Loop pass, exactly once.
func TestAC6LateOutboxRowHandledExactlyOnce(t *testing.T) {
	p, ctx := database(t)
	var count atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(204)
	}))
	defer remote.Close()
	prefs := preferences(t, p, remote.URL+"/secret-token")
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	worker := &Store{DB: p, Settings: prefs, Sender: &Transport{}, Now: clock}
	if consumed, err := worker.Consume(ctx); consumed || err != nil {
		t.Fatal("AC6 empty probe", err)
	}
	execute(t, p, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES('10000000-0000-4000-8000-000000000002','Late',100,50)`)
	execute(t, p, `INSERT INTO alert_events(id,node_id,severity,ts,title) VALUES('a0000000-0000-4000-8000-000000000002','10000000-0000-4000-8000-000000000002','warn',now(),'Late row')`)
	execute(t, p, `INSERT INTO alert_notification_outbox(event_id,channels) VALUES('a0000000-0000-4000-8000-000000000002',ARRAY['webhook'])`)
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	var outcomes []bool
	loop := &Store{DB: p, Settings: prefs, Sender: &Transport{}, Now: clock, ObserveRun: func(ok bool) { outcomes = append(outcomes, ok); cancel() }}
	loop.Loop(run)
	if len(outcomes) != 1 || !outcomes[0] {
		t.Fatal("AC6 late row pass", outcomes)
	}
	var deliveries int
	var status string
	var consumed bool
	if err := p.QueryRow(ctx, `SELECT count(*), coalesce(min(status),'') FROM notification_deliveries WHERE event_id='a0000000-0000-4000-8000-000000000002'`).Scan(&deliveries, &status); err != nil {
		t.Fatal(err)
	}
	if err := p.QueryRow(ctx, `SELECT consumed_at IS NOT NULL FROM alert_notification_outbox WHERE event_id='a0000000-0000-4000-8000-000000000002'`).Scan(&consumed); err != nil {
		t.Fatal(err)
	}
	if deliveries != 1 || status != "sent" || count.Load() != 1 || !consumed {
		t.Fatal("AC6 late row not handled exactly once", deliveries, status, count.Load(), consumed)
	}
}

// AC3: work that exists is still claimed as before. A due delivery is sent once, and a
// delivery whose next attempt is later is not claimed early.
func TestAC3QueueWorkStillFlowsAfterProbe(t *testing.T) {
	p, ctx := database(t)
	var count atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(204)
	}))
	defer remote.Close()
	prefs := preferences(t, p, remote.URL+"/secret-token")
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	worker := &Store{DB: p, Settings: prefs, Sender: &Transport{}, Now: func() time.Time { return now }}
	due, err := worker.Test(ctx, "webhook")
	if err != nil {
		t.Fatal(err)
	}
	const laterID = "40000000-0000-4000-8000-000000000001"
	execute(t, p, `INSERT INTO notification_deliveries(id,channel,created_at,next_attempt_at) VALUES($1,'webhook',$2,$3)`, laterID, now, now.Add(time.Hour))
	if ok, err := worker.Dispatch(ctx); !ok || err != nil {
		t.Fatal("AC3 due delivery not claimed", err)
	}
	if ok, err := worker.Dispatch(ctx); ok || err != nil {
		t.Fatal("AC3 later delivery claimed early", err)
	}
	if count.Load() != 1 {
		t.Fatal("AC3 send count", count.Load())
	}
	sent, err := worker.Delivery(ctx, due.ID)
	if err != nil || sent.Status != "sent" {
		t.Fatal("AC3 due delivery not sent", sent.Status, err)
	}
	later, err := worker.Delivery(ctx, laterID)
	if err != nil || later.Status != "pending" || later.Attempts != 0 {
		t.Fatal("AC3 later delivery changed", err)
	}
}
