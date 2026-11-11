package runloop

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentconfig "github.com/jooservices/speedtest/internal/agent/config"
	"github.com/jooservices/speedtest/internal/agent/control"
	"github.com/jooservices/speedtest/internal/agent/engine"
	"github.com/jooservices/speedtest/internal/agent/enroll"
	"github.com/jooservices/speedtest/internal/agent/report"
	"github.com/jooservices/speedtest/internal/agent/sched"
	"github.com/jooservices/speedtest/internal/agent/testutil"
	"github.com/jooservices/speedtest/internal/center/agentapi"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }
func bootstrap(t *testing.T) agentconfig.Config {
	t.Helper()
	t.Setenv("SPEEDTEST_SPOOL_DIR", filepath.Join(t.TempDir(), "spool"))
	t.Setenv("SPEEDTEST_OOKLA_BINARY", testutil.BuildFakeSpeedtest(t))
	t.Setenv("SPEEDTEST_OOKLA_ARGS", `["--scenario","success"]`)
	return agentconfig.Config{Engine: "ookla", Name: "node", Interval: 30 * time.Minute}
}
func parseURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, e := url.Parse(s)
	if e != nil {
		t.Fatal(e)
	}
	return u
}
func await(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true")
}
func start(t *testing.T, l *Loop) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- l.Serve(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(3 * time.Second):
				t.Error("shutdown did not join workers")
			}
			if err := l.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

type testClock struct {
	mu    sync.Mutex
	now   time.Time
	wait  chan time.Time
	ready chan struct{}
}

func (c *testClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *testClock) After(time.Duration) <-chan time.Time {
	select {
	case c.ready <- struct{}{}:
	default:
	}
	return c.wait
}
func (c *testClock) advance(t time.Time) { c.mu.Lock(); c.now = t; c.mu.Unlock(); c.wait <- t }

// This runs the composed scheduler, real engine process, HTTP protocol and SQLite
// spool together. Only the speed measurement and center are synthetic.
func TestLifecycleOutageRecoveryRestartAndIPChange(t *testing.T) {
	cfg := bootstrap(t)
	center := testutil.NewCenter()
	defer center.Close()
	var offline atomic.Bool
	var ip atomic.Value
	ip.Store("198.51.100.1")
	var receivedMu sync.Mutex
	var received []agentapi.Result
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if offline.Load() {
			w.WriteHeader(503)
			return
		}
		if r.URL.Path == "/api/agent/ip" {
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "code": 200, "data": map[string]string{"ip": ip.Load().(string)}})
			return
		}
		if r.URL.Path == "/api/agent/results" {
			data, _ := io.ReadAll(r.Body)
			var batch struct {
				Results []json.RawMessage `json:"results"`
			}
			if err := json.Unmarshal(data, &batch); err != nil {
				t.Error(err)
			}
			for _, raw := range batch.Results {
				result, err := agentapi.ParseResult(raw)
				if err != nil {
					t.Errorf("wire result invalid: %v", err)
				}
				receivedMu.Lock()
				received = append(received, result)
				receivedMu.Unlock()
			}
			r.Body = io.NopCloser(strings.NewReader(string(data)))
		}
		center.Server.Config.Handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	u := parseURL(t, server.URL)
	enrolled, err := (enroll.Client{BaseURL: u}).Enroll(t.Context(), enroll.Request{Token: "test-enrollment-token", Name: "node", Agent: "speedtest-agent/test"})
	if err != nil {
		t.Fatal(err)
	}
	clock := &testClock{now: time.Unix(1, 0), wait: make(chan time.Time, 1), ready: make(chan struct{}, 1)}
	l, err := New(cfg, u, enrolled.Credential, "test", quietLogger(), sched.WithClock(clock), sched.WithJitter(func(time.Duration) time.Duration { return 0 }))
	if err != nil {
		t.Fatal(err)
	}
	stop := start(t, l)
	select {
	case <-clock.ready:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not arm")
	}
	clock.advance(time.Unix(1800, 0))
	await(t, func() bool { return len(center.Results()) == 1 })
	if center.Results()[0].Manual {
		t.Fatal("scheduled result marked manual")
	}
	offline.Store(true)
	l.command(t.Context(), control.Command{ID: "offline-manual", Type: "run_now"})
	await(t, func() bool { r, _ := l.spool.Peek(100); return len(r) == 1 })
	l.Scheduler.RunNow()
	await(t, func() bool { r, _ := l.spool.Peek(100); return len(r) == 2 })
	pending, err := l.spool.Peek(100)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	// Startup must flush a pre-existing spool without waiting for a new measurement.
	offline.Store(false)
	ip.Store("198.51.100.2")
	next, err := New(cfg, u, enrolled.Credential, "test", quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	stopNext := start(t, next)
	await(t, func() bool { return len(center.Results()) == 3 })
	await(t, func() bool {
		return next.command(t.Context(), control.Command{ID: "offline-manual"}).Status == "succeeded"
	})
	resumed := next.command(t.Context(), control.Command{ID: "offline-manual"})
	if resumed.ResultID == nil || *resumed.ResultID != pending[0].ID || resumed.Pct != 100 {
		t.Fatalf("pending command acknowledgement was not resumed with its result: %+v", resumed)
	}
	results := center.Results()
	if results[1].ID != pending[0].ID || results[2].ID != pending[1].ID {
		t.Fatal("FIFO replay changed UUID/order")
	}
	ack := next.command(t.Context(), control.Command{ID: "manual-1", Type: "run_now"})
	if ack.Status != "running" || ack.Phase != "queued" {
		t.Fatal(ack)
	}
	await(t, func() bool { return next.command(t.Context(), control.Command{ID: "manual-1"}).Status == "succeeded" })
	final := next.command(t.Context(), control.Command{ID: "manual-1"})
	if final.ResultID == nil || final.Pct != 100 || final.Phase != "done" {
		t.Fatal(final)
	}
	await(t, func() bool { return len(center.Results()) == 4 })
	if !center.Results()[3].Manual {
		t.Fatal("manual result lost its origin")
	}
	receivedMu.Lock()
	if *received[0].IP != "198.51.100.1" || *received[len(received)-1].IP != "198.51.100.2" || len(received[len(received)-1].Warnings) == 0 {
		t.Error("observed IP/mismatch not retained")
	}
	receivedMu.Unlock()
	stopNext()
}

type fakeEngine struct {
	run func(context.Context, engine.Options) (engine.Result, error)
}

func (f fakeEngine) Run(c context.Context, o engine.Options) (engine.Result, error) {
	return f.run(c, o)
}
func TestFailedOfflineAndCancellationAreDurable(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "failure", true: "shutdown"}[cancelled], func(t *testing.T) {
			cfg := bootstrap(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
			defer server.Close()
			l, err := New(cfg, parseURL(t, server.URL), "credential", "test", quietLogger())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = l.Close() }()
			entered := make(chan struct{})
			l.cfg.engine = fakeEngine{func(ctx context.Context, _ engine.Options) (engine.Result, error) {
				close(entered)
				if cancelled {
					<-ctx.Done()
				}
				return engine.Result{Error: "speedtest: canceled"}, errors.New("safe failure")
			}}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- l.RunCommand(ctx, "cmd") }()
			<-entered
			cancel()
			if <-done == nil {
				t.Fatal("engine error lost")
			}
			rows, err := l.spool.Peek(10)
			if err != nil || len(rows) != 1 || rows[0].Status != "failed" || rows[0].IP != "" || rows[0].Error == nil {
				t.Fatal(rows, err)
			}
			encoded, _ := json.Marshal(rows[0])
			var fields map[string]any
			_ = json.Unmarshal(encoded, &fields)
			if fields["ip"] != nil {
				t.Fatal("invented IP")
			}
			if _, err := agentapi.ParseResult(encoded); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRateLimitBackoffSurvivesRestartAndDefersManualRuns(t *testing.T) {
	config := bootstrap(t)
	t.Setenv("SPEEDTEST_RATE_LIMIT_BACKOFF_INITIAL", "2s")
	t.Setenv("SPEEDTEST_RATE_LIMIT_BACKOFF_MAX", "2s")
	t.Setenv("SPEEDTEST_RATE_LIMIT_BACKOFF_FACTOR", "2")
	t.Setenv("SPEEDTEST_RATE_LIMIT_BACKOFF_JITTER", "0")
	center := parseURL(t, "http://127.0.0.1:1")
	var calls atomic.Int32
	limited := fakeEngine{run: func(context.Context, engine.Options) (engine.Result, error) {
		calls.Add(1)
		return engine.Result{Error: "Too many requests"}, errors.New("provider rejected the request")
	}}
	first, err := New(config, center, "credential", "test", quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	first.cfg.engine = limited
	if err := first.RunCommand(t.Context(), "manual-first"); err == nil {
		t.Fatal("rate-limited engine error was lost")
	}
	deadline, ok := first.rateLimits.Deadline("ookla")
	if !ok || !first.rateLimits.Blocked("ookla") {
		t.Fatal("first failure did not open a backoff window")
	}
	pending, err := first.spool.Peek(10)
	if err != nil || len(pending) != 1 || pending[0].FailureClass != "rate_limited" {
		t.Fatalf("first rate-limited result = %+v err=%v", pending, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := New(config, center, "credential", "test", quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	if !second.rateLimits.Blocked("ookla") {
		t.Fatal("restart forgot the provider deadline")
	}
	states, err := second.spool.RateLimitStates()
	if err != nil || states["ookla"].Attempt != 1 || !states["ookla"].Recorded {
		t.Fatalf("restart lost the recorded attempt: %+v err=%v", states, err)
	}
	second.cfg.engine = fakeEngine{run: func(context.Context, engine.Options) (engine.Result, error) {
		calls.Add(1)
		down, up, ping := 200.0, 100.0, 9.0
		return engine.Result{Down: &down, Up: &up, Ping: &ping, ExternalIP: "203.0.113.10", Server: &engine.Server{ID: 1, Name: "test", Host: "test.example"}, Duration: 1}, nil
	}}
	for _, id := range []string{"manual-deferred", "manual-deferred-again"} {
		if err := second.RunCommand(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		ack := second.command(t.Context(), control.Command{ID: id})
		if ack.Status != "failed" || ack.Error == nil || !strings.Contains(*ack.Error, "provider rate-limit backoff is active") || calls.Load() != 1 {
			t.Fatalf("manual request %s did not return a deferred outcome: ack=%+v calls=%d", id, ack, calls.Load())
		}
	}
	time.Sleep(time.Until(deadline) + 10*time.Millisecond)
	if err := second.RunCommand(t.Context(), "manual-recovered"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || second.rateLimits.Blocked("ookla") {
		t.Fatalf("provider did not recover after its deadline: calls=%d blocked=%t", calls.Load(), second.rateLimits.Blocked("ookla"))
	}
	states, err = second.spool.RateLimitStates()
	if err != nil || len(states) != 0 {
		t.Fatalf("success did not clear durable backoff: %+v err=%v", states, err)
	}
}

func TestLiveConfigurationAtomicAndBinding(t *testing.T) {
	cfg := bootstrap(t)
	cfg.NetworkInterface = "lo"
	l, err := New(cfg, parseURL(t, "http://127.0.0.1:1"), "credential", "test", quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	sch := sched.Schedule{Interval: 15 * time.Minute, Enabled: true}
	var remote control.AgentConfig
	if err := json.Unmarshal([]byte(`{"engine":"librespeed","threads":8,"server":42,"binding":{"iface":"","sourceIp":"127.0.0.1"}}`), &remote); err != nil {
		t.Fatal(err)
	}
	if err := l.applyConfig(remote, sch); err != nil {
		t.Fatal(err)
	}
	if l.cfg.name != "librespeed" || l.cfg.threads != 8 || l.cfg.server != 42 || l.cfg.binding.SourceIP != "127.0.0.1" || l.Scheduler.Schedule() != sch {
		t.Fatal("settings not applied together")
	}
	good := l.cfg
	for _, bad := range []string{`{"engine":"unsupported"}`, `{"threads":65}`, `{"threads":-1}`, `{"server":null}`, `{"server":0}`, `{"binding":{"iface":"absent0"}}`} {
		var invalid control.AgentConfig
		_ = json.Unmarshal([]byte(bad), &invalid)
		if l.applyConfig(invalid, sched.Schedule{Interval: time.Minute}) == nil {
			t.Fatal("accepted invalid config", bad)
		}
		if l.cfg.server != good.server || l.cfg.name != good.name || l.Scheduler.Schedule() != sch {
			t.Fatal("partial configuration applied")
		}
	}
	remote.Server = json.RawMessage(`"auto"`)
	remote.Binding = nil
	if err := l.applyConfig(remote, sch); err != nil || l.cfg.server != 0 {
		t.Fatal(err)
	}
}
func TestInitializationAndFatalErrors(t *testing.T) {
	cfg := bootstrap(t)
	u := parseURL(t, "http://127.0.0.1:1")
	for _, bad := range []agentconfig.Config{{Engine: "bad"}, {Engine: "ookla", Name: "n", Interval: time.Minute, NetworkInterface: "absent0"}} {
		if l, err := New(bad, u, "credential", "test", nil); err == nil {
			_ = l.Close()
			t.Fatal("bad config accepted")
		}
	}
	if _, err := New(cfg, nil, "credential", "test", nil); err == nil {
		t.Fatal("nil center")
	}
	if _, err := New(cfg, u, "", "test", nil); err == nil {
		t.Fatal("empty credential")
	}
	t.Setenv("SPEEDTEST_SPOOL_MAX_RESULTS", "bad")
	if _, err := New(cfg, u, "credential", "test", nil); err == nil {
		t.Fatal("bad spool limit")
	}
	t.Setenv("SPEEDTEST_SPOOL_MAX_RESULTS", "")
	dir := t.TempDir()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPEEDTEST_SPOOL_DIR", dir)
	if _, err := New(cfg, u, "credential", "test", nil); err == nil {
		t.Fatal("unsafe spool accepted")
	}
	t.Setenv("SPEEDTEST_SPOOL_DIR", filepath.Join(t.TempDir(), "spool"))
	l, err := New(cfg, u, "credential", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := l.Serve(ctx); err == nil {
		t.Fatal("closed spool did not fail lifecycle")
	}
}

func TestJournalRestartAndCommandOutcomes(t *testing.T) {
	cfg := bootstrap(t)
	u := parseURL(t, "http://127.0.0.1:1")
	l, err := New(cfg, u, "credential", "test", quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	state := commandState{Ack: control.CommandAck{Status: "running", Phase: "reporting", Pct: 80}, Result: "00000000-0000-4000-8000-000000000001"}
	data, _ := json.Marshal(state)
	if err = l.spool.SaveCommand("resuming", data); err != nil {
		t.Fatal(err)
	}
	queued, _ := json.Marshal(commandState{Ack: control.CommandAck{Status: "running", Phase: "queued"}})
	if err = l.spool.SaveCommand("retry", queued); err != nil {
		t.Fatal(err)
	}
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	l, err = New(cfg, u, "credential", "test", quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if l.command(t.Context(), control.Command{ID: "resuming"}).Phase != "reporting" {
		t.Fatal("lost pending command")
	}
	res := report.Result{ID: state.Result, Status: report.StatusFailed, Error: func() *string { s := "speedtest: timeout"; return &s }()}
	if err = l.delivered(res, true); err != nil {
		t.Fatal(err)
	}
	ack := l.command(t.Context(), control.Command{ID: "resuming"})
	if ack.Status != "failed" || ack.ResultID == nil || ack.Error == nil {
		t.Fatal(ack)
	}
	state.Result = "00000000-0000-4000-8000-000000000002"
	l.commands["rejected"] = state
	if err = l.delivered(report.Result{ID: state.Result}, false); err != nil {
		t.Fatal(err)
	}
	if l.commands["rejected"].Ack.ResultID != nil || l.commands["rejected"].Ack.Error == nil {
		t.Fatal("rejected result claimed reported")
	}
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	l, err = New(cfg, u, "credential", "test", quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if l.commands["resuming"].Ack.Status != "failed" {
		t.Fatal("terminal ack lost on restart")
	}
	if _, ok := l.commands["retry"]; ok {
		t.Fatal("uncommitted run not retried")
	}
}

func TestCommandHistoryCapacityAndStorageFailures(t *testing.T) {
	cfg := bootstrap(t)
	l, err := New(cfg, parseURL(t, "http://127.0.0.1:1"), "credential", "test", quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	for i := range 10000 {
		l.commands[fmt.Sprint(i)] = commandState{Ack: control.CommandAck{Status: "running"}}
	}
	if l.command(t.Context(), control.Command{ID: "deferred"}).Phase != "waiting" {
		t.Fatal("unacknowledged history evicted")
	}
	l.commands["0"] = commandState{Ack: control.CommandAck{Status: "succeeded"}}
	if l.command(t.Context(), control.Command{ID: "new"}).Phase != "queued" {
		t.Fatal("terminal history not evicted")
	}
	clear(l.commands)
	for range 100 {
		l.Scheduler.RunNow()
	}
	_ = l.command(t.Context(), control.Command{ID: "queue-full"})
	if _, ok := l.commands["queue-full"]; ok {
		t.Fatal("unqueued command consumed")
	}
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	_ = l.command(t.Context(), control.Command{ID: "storage-failed"})
	select {
	case fatalErr := <-l.fatal:
		if fatalErr == nil {
			t.Fatal("journal failure signaled without an error")
		}
	default:
		t.Fatal("journal failure did not stop lifecycle")
	}
	l.commands["reporting"] = commandState{Result: "id"}
	if l.delivered(report.Result{ID: "id"}, true) == nil {
		t.Fatal("terminal journal error ignored")
	}
	for i := range 10000 {
		l.commands[fmt.Sprint(i)] = commandState{Ack: control.CommandAck{Status: "succeeded"}}
	}
	_ = l.command(t.Context(), control.Command{ID: "delete-failed"})
}

func TestMeasureStorageAndRepresentationFailures(t *testing.T) {
	for _, scenario := range []string{"coordinator", "representation", "storage", "network"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := bootstrap(t)
			l, err := New(cfg, parseURL(t, "http://127.0.0.1:1"), "credential", "test", quietLogger())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = l.Close() }()
			l.cfg.engine = fakeEngine{func(context.Context, engine.Options) (engine.Result, error) {
				return engine.Result{Error: "speedtest: no_route"}, errors.New("engine failure")
			}}
			switch scenario {
			case "coordinator":
				l.center = "invalid"
			case "representation":
				l.cfg.threads = 65
			case "storage":
				_ = l.Close()
			case "network":
				l.cfg.binding.Interface = "absent0"
			}
			err = l.Run(t.Context())
			if err == nil {
				t.Fatal("failure lost")
			}
			if scenario != "network" {
				select {
				case fatalErr := <-l.fatal:
					if fatalErr == nil {
						t.Fatal("fatal error signal was nil")
					}
				default:
					t.Fatal("fatal error not signaled")
				}
			}
		})
	}
}

func TestBootstrapLibreSpeedAndInvalidJournal(t *testing.T) {
	cfg := bootstrap(t)
	cfg.Engine = "librespeed"
	u := parseURL(t, "http://127.0.0.1:1")
	l, err := New(cfg, u, "credential", "test", quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if l.cfg.threads != 4 {
		t.Fatal("env concurrency default lost")
	}
	if err = l.spool.SaveCommand("invalid", json.RawMessage(`not-json`)); err != nil {
		t.Fatal(err)
	}
	_ = l.Close()
	if _, err = New(cfg, u, "credential", "test", quietLogger()); err == nil {
		t.Fatal("corrupt journal accepted")
	}
}

func TestJournalInitializationQueryAndQueueCleanupFailure(t *testing.T) {
	cfg := bootstrap(t)
	dir := os.Getenv("SPEEDTEST_SPOOL_DIR")
	u := parseURL(t, "http://127.0.0.1:1")
	l, err := New(cfg, u, "credential", "test", quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", filepath.Join(dir, "spool.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err = raw.Exec(`CREATE TRIGGER command_delete_failure BEFORE DELETE ON command_state BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	for range 100 {
		l.Scheduler.RunNow()
	}
	_ = l.command(t.Context(), control.Command{ID: "queue-full"})
	select {
	case fatalErr := <-l.fatal:
		if fatalErr == nil {
			t.Fatal("queue cleanup signaled without an error")
		}
	default:
		t.Fatal("queue cleanup failure lost")
	}
	_ = l.Close()
	if _, err = raw.Exec(`DROP TABLE command_state; CREATE TABLE command_state(other TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err = New(cfg, u, "credential", "test", quietLogger()); err == nil {
		t.Fatal("invalid journal schema accepted")
	}
}

func TestIdleReporterStopsOnCancellation(t *testing.T) {
	cfg := bootstrap(t)
	l, err := New(cfg, parseURL(t, "http://127.0.0.1:1"), "credential", "test", quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { l.deliver(ctx); close(done) }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("idle reporter did not stop")
	}
}

func TestControlProtocolProgressPauseResumeAndLiveETag(t *testing.T) {
	cfg := bootstrap(t)
	id := "00000000-0000-4000-8000-000000000051"
	center := testutil.NewCenter(testutil.WithCommands(testutil.Command{ID: id, Type: "run_now"}, testutil.Command{ID: "pause", Type: "pause"}, testutil.Command{ID: "resume", Type: "resume"}, testutil.Command{ID: "update", Type: "update_config"}))
	defer center.Close()
	var acks []agentapi.Ack
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ack") {
			data, _ := io.ReadAll(r.Body)
			ack, err := agentapi.ParseAck(data)
			if err != nil {
				t.Error("ack contract", err)
			}
			acks = append(acks, ack)
			r.Body = io.NopCloser(strings.NewReader(string(data)))
		}
		center.Server.Config.Handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	l, err := New(cfg, parseURL(t, server.URL), "test-node-credential", "test", quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	l.Controller.Once(t.Context())
	if len(acks) != 4 || acks[0].Status != "running" || acks[0].Phase == nil || *acks[0].Phase != "queued" {
		t.Fatal(acks)
	}
	if !l.Scheduler.Schedule().Enabled {
		t.Fatal("resume did not enable schedule")
	}
	for _, a := range acks[1:] {
		if a.Status != "succeeded" || a.Pct == nil || *a.Pct != 100 {
			t.Fatal(a)
		}
	}
	if err = l.RunCommand(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	l.Controller.Once(t.Context())
	if acks[len(acks)-1].Phase == nil || *acks[len(acks)-1].Phase != "reporting" {
		t.Fatal("spooled command acknowledged early")
	}
	if err = l.reporter.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	l.Controller.Once(t.Context())
	ack := acks[len(acks)-1]
	if ack.Status != "succeeded" || ack.ResultID == nil || ack.Pct == nil || *ack.Pct != 100 {
		t.Fatal(ack)
	}
	if len(center.Results()) != 1 {
		t.Fatal("redelivery executed another test")
	}
	found := false
	for _, etag := range center.ConfigRequests() {
		if etag != "" {
			found = true
		}
	}
	if !found {
		t.Fatal("config ETag not used")
	}
}

func TestLibreSpeedAutomaticServerWithSourceBinding(t *testing.T) {
	cfg := bootstrap(t)
	cfg.Engine = "librespeed"
	cfg.SourceIP = "127.0.0.1"
	cfg.ParallelConnections = 8
	data, err := os.ReadFile("../engine/testdata/librespeed.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture map[string]any
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	fixture["timestamp"] = time.Now().UTC().Format(time.RFC3339)
	data, _ = json.Marshal(fixture)
	path := filepath.Join(t.TempDir(), "librespeed")
	argsFile := filepath.Join(t.TempDir(), "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argsFile + "'\ncat <<'RESULT'\n" + string(data) + "\nRESULT\n"
	if err = os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPEEDTEST_LIBRESPEED_BINARY", path)
	center := testutil.NewCenter()
	defer center.Close()
	l, err := New(cfg, parseURL(t, center.URL), "test-node-credential", "test", quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if err = l.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows, err := l.spool.Peek(1)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	wire, _ := json.Marshal(rows[0])
	if _, err = agentapi.ParseResult(wire); err != nil {
		t.Fatal(err)
	}
	if rows[0].ServerID != nil || rows[0].Server == nil || rows[0].Threads != 8 {
		t.Fatal("LibreSpeed identity or threads lost")
	}
	args, err := os.ReadFile(argsFile)
	if err != nil || !strings.Contains(string(args), "--source\n127.0.0.1") || !strings.Contains(string(args), "--concurrent\n8") {
		t.Fatal(string(args), err)
	}
	if err = l.reporter.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestMissingPublicIPRecordsFailedMeasurement(t *testing.T) {
	cfg := bootstrap(t)
	l, err := New(cfg, parseURL(t, "http://127.0.0.1:1"), "credential", "test", quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	l.cfg.engine = fakeEngine{func(context.Context, engine.Options) (engine.Result, error) { return engine.Result{}, nil }}
	if err = l.Run(t.Context()); err == nil {
		t.Fatal("missing IP was silently accepted")
	}
	rows, err := l.spool.Peek(1)
	if err != nil || len(rows) != 1 || rows[0].IP != "" || rows[0].Error == nil || *rows[0].Error != "speedtest: public_ip_unavailable" {
		t.Fatal(rows, err)
	}
}

func TestLateFatalErrorWinsNormalShutdown(t *testing.T) {
	cfg := bootstrap(t)
	l, err := New(cfg, parseURL(t, "http://127.0.0.1:1"), "credential", "test", quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	l.fail(errors.New("spool failed"))
	if err = l.Serve(ctx); err == nil {
		t.Fatal("late fatal failure lost during cancellation")
	}
}
