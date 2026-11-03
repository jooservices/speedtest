//go:build integration

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jooservices/speedtest/internal/agent/report"
	"github.com/jooservices/speedtest/internal/agent/testutil"
	"github.com/jooservices/speedtest/internal/center/agentapi"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/metrics"
	"github.com/jooservices/speedtest/internal/center/nodes"
)

// faultProxy cuts established connections as well as refusing new connections.
// It never sends administrative commands to the shared PostgreSQL server.
type faultProxy struct {
	listener    net.Listener
	target      string
	mu          sync.Mutex
	down        bool
	connections map[net.Conn]bool
	wg          sync.WaitGroup
}

func newFaultProxy(t *testing.T, target string) *faultProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &faultProxy{listener: listener, target: target, connections: make(map[net.Conn]bool)}
	p.wg.Go(func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			p.wg.Go(func() { p.forward(conn) })
		}
	})
	t.Cleanup(func() { _ = listener.Close(); p.setDown(true); p.wg.Wait() })
	return p
}

func (p *faultProxy) forward(client net.Conn) {
	defer func() { _ = client.Close() }()
	p.mu.Lock()
	if p.down {
		p.mu.Unlock()
		return
	}
	p.connections[client] = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.connections, client); p.mu.Unlock() }()
	upstream, err := net.DialTimeout("tcp", p.target, time.Second)
	if err != nil {
		return
	}
	defer func() { _ = upstream.Close() }()
	p.mu.Lock()
	if p.down {
		p.mu.Unlock()
		return
	}
	p.connections[upstream] = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.connections, upstream); p.mu.Unlock() }()
	done := make(chan struct{})
	go func() { _, _ = io.Copy(upstream, client); _ = upstream.Close(); close(done) }()
	_, _ = io.Copy(client, upstream)
	_ = client.Close()
	<-done
}

func (p *faultProxy) setDown(down bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = down
	if down {
		for conn := range p.connections {
			_ = conn.Close()
		}
	}
}

func TestDatabaseDisconnectRecoveryIntegration(t *testing.T) {
	pool, ctx, key := openServerIntegrationPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	_, token := enrollIngestionNode(t, ctx, nodes.NewService(pool, key), "fault-edge")
	cfg := pool.Config().Copy()
	proxy := newFaultProxy(t, net.JoinHostPort(cfg.ConnConfig.Host, strconv.Itoa(int(cfg.ConnConfig.Port))))
	cfg.ConnConfig.DialFunc = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, proxy.listener.Addr().String())
	}
	proxied, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal("create fault-injected pool")
	}
	defer proxied.Close()
	if err = proxied.Ping(ctx); err != nil {
		t.Fatal("connect through proxy")
	}
	service := nodes.NewService(proxied, key)
	var logs bytes.Buffer
	handler := NewHandler(Options{Pinger: pingFunc(proxied.Ping), NodeCredentials: service, AgentAPI: &agentapi.Postgres{Pool: proxied}, ReadinessTimeout: time.Second, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	checkReady := func(want int) {
		t.Helper()
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
		if w.Code != want {
			t.Fatalf("readyz=%d want %d", w.Code, want)
		}
	}
	checkReady(200)
	proxy.setDown(true)
	checkReady(503)
	for _, path := range []string{"/api/agent/config", "/api/agent/commands"} {
		requestCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		req := httptest.NewRequest("GET", path, nil).WithContext(requestCtx)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		cancel()
		var problem struct {
			Status int `json:"status"`
		}
		if w.Code < 500 || !strings.Contains(w.Header().Get("Content-Type"), "application/problem+json") || json.Unmarshal(w.Body.Bytes(), &problem) != nil || problem.Status != w.Code {
			t.Fatalf("outage response: %d %s", w.Code, w.Body.String())
		}
	}
	proxy.setDown(false)
	deadline := time.Now().Add(10 * time.Second)
	for proxied.Ping(ctx) != nil {
		if time.Now().After(deadline) {
			t.Fatal("pool did not reconnect")
		}
		time.Sleep(20 * time.Millisecond)
	}
	checkReady(200)
	if w := ingestRequest(t, handler, token, "GET", "/api/agent/config", ""); w.Code != 200 {
		t.Fatalf("recovery API status %d", w.Code)
	}
	outcomes(t, ingestRequest(t, handler, token, "POST", "/api/agent/results", `{"results":[`+ingestionResult+`]}`), 201)
	if strings.Contains(logs.String(), "request panic recovered") {
		t.Fatal("database outage triggered a recovered handler panic")
	}
	t.Log("PASS: readyz 200 -> 503 -> 200; config/commands problem+json 5xx during connection cut; same pool reconnects and ingests after recovery; no shared DB restart")
}

func TestMetricsGaugeTransitionsIntegration(t *testing.T) {
	pool, ctx, key := openServerIntegrationPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	now := time.Now().UTC().Truncate(time.Second)
	handler := NewHandler(Options{Metrics: metrics.Collector{DB: pool, Now: func() time.Time { return now }}})
	scrape := func(offline, age, failed string) {
		t.Helper()
		req := httptest.NewRequest("GET", "/metrics", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("metrics status %d", w.Code)
		}
		for _, line := range []string{"speedtest_nodes_offline " + offline, "speedtest_last_result_age_seconds " + age, "speedtest_notification_deliveries_failed " + failed} {
			if !strings.Contains(w.Body.String(), "\n"+line+"\n") {
				t.Fatalf("missing gauge %s: %s", line, w.Body.String())
			}
		}
	}
	scrape("0", "-1", "0")
	service := nodes.NewService(pool, key)
	node, _ := enrollIngestionNode(t, ctx, service, "metrics-edge")
	if _, err := pool.Exec(ctx, `UPDATE nodes SET last_seen_at=$1 WHERE id=$2`, now.Add(-6*time.Minute), node); err != nil {
		t.Fatal(err)
	}
	result, err := agentapi.ParseResult([]byte(ingestionResult))
	if err != nil {
		t.Fatal(err)
	}
	result.TS = now.Add(-90 * time.Second).Format(time.RFC3339)
	if _, err = (&agentapi.Postgres{Pool: pool}).SaveResult(ctx, node, result); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO notification_deliveries(id,channel,status) VALUES ('00000000-0000-4000-8000-000000000991','webhook','failed')`); err != nil {
		t.Fatal(err)
	}
	scrape("1", "90", "1")
	now = now.Add(time.Minute)
	scrape("1", "150", "1")
	if _, err = pool.Exec(ctx, `UPDATE nodes SET last_seen_at=$1 WHERE id=$2`, now, node); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE notification_deliveries SET status='sent'`); err != nil {
		t.Fatal(err)
	}
	result.ID = "00000000-0000-4000-8000-000000000992"
	result.TS = now.Format(time.RFC3339)
	if _, err = (&agentapi.Postgres{Pool: pool}).SaveResult(ctx, node, result); err != nil {
		t.Fatal(err)
	}
	scrape("0", "0", "0")
	t.Log("PASS: HTTP scrape offline 0->1->0, result age -1->90->150->0 seconds, failed deliveries 0->1->0")
}

func TestAgentClockSkewIntegration(t *testing.T) {
	pool, ctx, key := openServerIntegrationPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	node, token := enrollIngestionNode(t, ctx, nodes.NewService(pool, key), "skew-edge")
	handler := NewHandler(Options{NodeCredentials: nodes.NewService(pool, key), AgentAPI: &agentapi.Postgres{Pool: pool}})
	now := time.Now().UTC().Truncate(time.Second)
	for i, skew := range []time.Duration{-10 * time.Minute, 10 * time.Minute} {
		ts := now.Add(skew)
		body := strings.Replace(ingestionResult, "2026-11-03T00:00:00Z", ts.Format(time.RFC3339), 1)
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
		body = strings.Replace(body, "00000000-0000-4000-8000-000000000001", id, 1)
		outcomes(t, ingestRequest(t, handler, token, "POST", "/api/agent/results", `{"results":[`+body+`]}`), 201)
		var stored time.Time
		if err := pool.QueryRow(ctx, `SELECT ts FROM results WHERE id=$1 AND node_id=$2`, id, node).Scan(&stored); err != nil || !stored.Equal(ts) {
			t.Fatal("skew timestamp changed", err)
		}
		heartbeat := ingestRequest(t, handler, token, "POST", "/api/agent/heartbeat", fmt.Sprintf(`{"agent":"skew","uptime":10,"lastTest":%q}`, ts.Format(time.RFC3339)))
		if heartbeat.Code != 200 {
			t.Fatal(heartbeat.Code)
		}
		var seen time.Time
		if err := pool.QueryRow(ctx, `SELECT last_seen_at FROM nodes WHERE id=$1`, node).Scan(&seen); err != nil || time.Since(seen) > 5*time.Second || seen.After(time.Now().Add(time.Second)) {
			t.Fatal("heartbeat used agent clock", err)
		}
		snap, err := (metrics.Collector{DB: pool, Now: func() time.Time { return now }}).Collect(ctx)
		if err != nil || snap.LastResultAgeSeconds != -skew.Seconds() {
			t.Fatalf("skew %s age=%v err=%v", skew, snap.LastResultAgeSeconds, err)
		}
		t.Logf("skew=%s accepted=201 timestamp preserved; last-result-age=%gs; heartbeat uses center clock", skew, snap.LastResultAgeSeconds)
	}
}

func TestCenterRestartSpoolIntegration(t *testing.T) {
	// Reuse the agent harness for the transport origin, then restart that origin
	// with fresh production handlers over the same verified durable database.
	pool, ctx, key := openServerIntegrationPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	node, token := enrollIngestionNode(t, ctx, nodes.NewService(pool, key), "restart-edge")
	center := testutil.NewCenter()
	origin := center.URL
	addr := center.Server.Listener.Addr().String()
	center.Close()
	firstListener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	first := httptest.NewUnstartedServer(NewHandler(Options{NodeCredentials: nodes.NewService(pool, key), AgentAPI: &agentapi.Postgres{Pool: pool}}))
	_ = first.Listener.Close()
	first.Listener = firstListener
	first.Start()
	request, err := http.NewRequestWithContext(ctx, "GET", origin+"/api/agent/config", nil)
	if err != nil {
		first.Close()
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := first.Client().Do(request)
	if err != nil {
		first.Close()
		t.Fatal("initial center unavailable")
	}
	_ = response.Body.Close()
	first.Close()
	if response.StatusCode != 200 {
		t.Fatal("initial center config", response.StatusCode)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	spool, err := report.Open(report.SpoolConfig{Dir: filepath.Join(t.TempDir(), "spool")}, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = spool.Close() }()
	base, err := url.Parse(origin)
	if err != nil {
		t.Fatal(err)
	}
	client := &report.Client{BaseURL: base, Credential: token, HTTP: &http.Client{Timeout: time.Second}}
	reporter := report.NewReporter(spool, client, report.WithBatchSize(1), report.WithLogger(logger), report.WithBackoff(report.Backoff{Initial: 10 * time.Millisecond, Max: 20 * time.Millisecond, Factor: 2}))
	ids := make([]string, 3)
	for i := range ids {
		var result report.Result
		if err = json.Unmarshal([]byte(ingestionResult), &result); err != nil {
			t.Fatal(err)
		}
		result.ID = fmt.Sprintf("00000000-0000-4000-8000-%012d", i+100)
		result.TS = time.Now().UTC().Add(time.Duration(i-3) * time.Minute).Format(time.RFC3339)
		ids[i], err = reporter.Enqueue(result)
		if err != nil {
			t.Fatal(err)
		}
	}
	offline, cancel := context.WithTimeout(ctx, 80*time.Millisecond)
	err = reporter.Flush(offline)
	cancel()
	if err == nil {
		t.Fatal("offline delivery unexpectedly succeeded")
	}
	pending, err := spool.Peek(10)
	if err != nil || len(pending) != 3 {
		t.Fatal("offline spool lost results", err)
	}
	// A real TCP listener is closed and rebound, rather than returning synthetic 503s.
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	hook := &orderHook{}
	restarted := httptest.NewUnstartedServer(NewHandler(Options{NodeCredentials: nodes.NewService(pool, key), AgentAPI: &agentapi.Postgres{Pool: pool, Hook: hook}}))
	_ = restarted.Listener.Close()
	restarted.Listener = listener
	restarted.Start()
	defer restarted.Close()
	if err = reporter.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err = reporter.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	hook.mu.Lock()
	order := append([]string(nil), hook.ids...)
	hook.mu.Unlock()
	if fmt.Sprint(order) != fmt.Sprint(ids) {
		t.Fatalf("delivery order/duplicates: %v want %v", order, ids)
	}
	pending, err = spool.Peek(10)
	if err != nil || len(pending) != 0 {
		t.Fatal("spool not drained", err)
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM results WHERE node_id=$1`, node).Scan(&count); err != nil || count != 3 {
		t.Fatal("durable result count", count, err)
	}
	t.Log("PASS: stopped TCP origin retains three spooled results; fresh center handlers deliver once in FIFO order; second flush sends nothing; PostgreSQL count=3")
}

type orderHook struct {
	mu  sync.Mutex
	ids []string
}

func (h *orderHook) AfterResultStored(_ context.Context, _ pgx.Tx, _ string, result agentapi.Result) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ids = append(h.ids, result.ID)
	return nil
}
