package control

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/agent/testutil"
)

func TestHeartbeatUptimeAndLastTest(t *testing.T) {
	center := testutil.NewCenter()
	defer center.Close()
	now := time.Unix(100, 0)
	fake := &fakeScheduler{lastRun: time.Unix(80, 0)}
	ctl := New(Client{BaseURL: baseURL(t, center.Server), Credential: "test-node-credential", Agent: "speedtest-agent/1.2.3"}, fake, WithNow(func() time.Time { return now }))
	now = now.Add(65 * time.Second)
	ctl.Once(context.Background())
	hb := center.Heartbeats()[0]
	if hb.Uptime != 65 || hb.LastTest == nil || *hb.LastTest != "1970-01-01T00:01:20Z" {
		t.Fatalf("heartbeat = %+v", hb)
	}
	if HeartbeatInterval != 30*time.Second {
		t.Fatal("production heartbeat cadence changed")
	}
}

func TestControllerRunCadenceAndCancellation(t *testing.T) {
	center := testutil.NewCenter()
	defer center.Close()
	ctl := New(Client{BaseURL: baseURL(t, center.Server), Credential: "test-node-credential"}, &fakeScheduler{}, WithInterval(time.Millisecond), WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ctl.Run(ctx) }()
	deadline := time.After(5 * time.Second)
	for len(center.Heartbeats()) < 3 {
		select {
		case <-deadline:
			cancel()
			t.Fatal("missing repeated heartbeat")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown blocked")
	}
}

func TestAckRetriesWithoutReexecution(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	}))
	defer server.Close()
	fake := &fakeScheduler{}
	ctl := New(Client{BaseURL: baseURL(t, server)}, fake)
	cmd := Command{ID: "c1", Type: "run_now"}
	for range 4 {
		ctl.handleCommand(context.Background(), cmd)
	}
	_, _, runs := fake.counts()
	if runs != 1 || attempts.Load() != 3 {
		t.Fatalf("runs=%d acks=%d", runs, attempts.Load())
	}
}

func TestInvalidConfigDoesNotReplaceScheduleOrETag(t *testing.T) {
	var body atomic.Value
	body.Store(configJSON())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"valid"`)
		_, _ = io.WriteString(w, `{"success":true,"data":`+body.Load().(string)+`}`)
	}))
	defer server.Close()
	fake := &fakeScheduler{}
	ctl := New(Client{BaseURL: baseURL(t, server)}, fake)
	ctl.syncConfig(context.Background())
	if fake.scheduleCount() != 1 {
		t.Fatal("valid config not applied")
	}
	body.Store(`{"schedule":{"interval":0}}`)
	ctl.syncConfig(context.Background())
	if fake.scheduleCount() != 1 || ctl.etag != `"valid"` {
		t.Fatal("invalid config replaced prior schedule")
	}
	if ack := ctl.updateConfig(context.Background()); ack.Status != "failed" || ack.Error == nil {
		t.Fatalf("ack=%+v", ack)
	}
	server.Close()
	if ack := ctl.updateConfig(context.Background()); ack.Status != "failed" || ack.Error == nil {
		t.Fatalf("ack=%+v", ack)
	}
	ctl.syncConfig(context.Background())
	if fake.scheduleCount() != 1 {
		t.Fatal("offline config replaced schedule")
	}
}

func TestCommandStoreBoundAndDuplicate(t *testing.T) {
	s := newCommandStore()
	s.max = 2
	s.put("a", CommandAck{Status: "succeeded"})
	s.put("a", CommandAck{Status: "failed"})
	s.put("b", CommandAck{Status: "succeeded"})
	s.markAcked("a")
	if !s.makeRoom() {
		t.Fatal("cannot evict acknowledged command")
	}
	s.put("c", CommandAck{Status: "succeeded"})
	if _, ok := s.get("a"); ok {
		t.Fatal("old entry retained")
	}
	if len(s.order) != 2 || len(s.store) != 2 {
		t.Fatal("unbounded store")
	}
	s.markAcked("absent")
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func (brokenReader) Close() error             { return nil }

func TestClientFailureBoundaries(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"status", "bad-json", "bad-data", "transport", "read"} {
		t.Run(mode, func(t *testing.T) {
			client := Client{BaseURL: &url.URL{Scheme: "http", Host: "center.invalid"}, Timeout: time.Second, HTTP: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				if mode == "transport" {
					return nil, errors.New("offline")
				}
				resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("garbage"))}
				if mode == "status" {
					resp.StatusCode = 503
				}
				if mode == "bad-data" {
					resp.Body = io.NopCloser(strings.NewReader(`{"success":true,"data":42}`))
				}
				if mode == "read" {
					resp.Body = brokenReader{}
				}
				return resp, nil
			})}}
			if _, _, _, err := client.Config(ctx, ""); err == nil {
				t.Fatal("config accepted failure")
			}
			if _, err := client.PollCommands(ctx); err == nil {
				t.Fatal("commands accepted failure")
			}
			// Heartbeat and ack do not consume response data on success.
			if mode != "bad-json" && mode != "bad-data" {
				if err := client.Ack(ctx, "c1", CommandAck{}); err == nil {
					t.Fatal("ack accepted failure")
				}
				if err := client.Heartbeat(ctx, Heartbeat{}); err == nil {
					t.Fatal("heartbeat accepted failure")
				}
			}
		})
	}
	for _, client := range []Client{{}, {BaseURL: &url.URL{Scheme: "file"}}} {
		if _, _, _, err := client.Config(ctx, ""); err == nil {
			t.Fatal("missing URL validation")
		}
		if _, err := client.PollCommands(ctx); err == nil {
			t.Fatal("missing URL validation")
		}
		if err := client.Ack(ctx, "c1", CommandAck{}); err == nil {
			t.Fatal("missing URL validation")
		}
	}
	c := Client{BaseURL: &url.URL{Scheme: "http", Host: "center.invalid"}}
	if _, err := c.newRequest(ctx, "POST", "", make(chan int)); err == nil {
		t.Fatal("marshal accepted channel")
	}
	if _, err := c.newRequest(ctx, "bad method", "", nil); err == nil {
		t.Fatal("invalid method accepted")
	}
	for _, body := range []string{`{}`, `{"title":"Unavailable"}`, `{"detail":"Unavailable"}`, `bad`} {
		if err := decodeProblem(503, []byte(body)); err == nil {
			t.Fatal("missing problem error")
		}
	}
}

func TestConfigIntervalBounds(t *testing.T) {
	for _, n := range []int{-1, 0, 4, 1441, int(^uint(0) >> 1)} {
		if _, err := (AgentConfig{Schedule: ScheduleInput{Interval: n}}).schedule(); err == nil {
			t.Fatalf("accepted interval %d", n)
		}
	}
}

func TestClientSecurityAndBounds(t *testing.T) {
	ctx := context.Background()
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer target.Close()
	for _, mode := range []string{"redirect", "oversized", "secret", "too-many", "bad-envelope"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "redirect":
					http.Redirect(w, r, target.URL, http.StatusFound)
				case "oversized":
					_, _ = io.WriteString(w, strings.Repeat("x", (1<<20)+1))
				case "secret":
					w.WriteHeader(401)
					_, _ = io.WriteString(w, `{"detail":"node-secret"}`)
				case "too-many":
					_, _ = io.WriteString(w, `{"success":true,"data":[`+strings.Repeat(`{},`, 100)+`{}]}`)
				case "bad-envelope":
					_, _ = io.WriteString(w, `{"success":false,"data":[]}`)
				}
			}))
			defer server.Close()
			client := Client{BaseURL: baseURL(t, server), Credential: "node-secret"}
			_, err := client.PollCommands(ctx)
			if err == nil || strings.Contains(err.Error(), "node-secret") {
				t.Fatalf("unsafe/missing error: %v", err)
			}
		})
	}
	if redirected.Load() != 0 {
		t.Fatal("credential request followed redirect")
	}
	for _, raw := range []string{"http:///no-host", "http://user:secret@center.invalid", "http://center.invalid?token=secret", "http://center.invalid#secret"} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := (Client{BaseURL: u}).Heartbeat(ctx, Heartbeat{}); err == nil {
			t.Fatal("unsafe center URL accepted")
		}
	}
	for _, id := range []string{"", "..", "a/b", "a?b", "a#b", "%2f"} {
		if err := (Client{}).Ack(ctx, id, CommandAck{}); err == nil {
			t.Fatal("invalid ID accepted")
		}
	}
}

func TestHeartbeatContinuesWhileConfigBlocked(t *testing.T) {
	blocked := make(chan struct{})
	release := make(chan struct{})
	var beats atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/agent/config" {
			close(blocked)
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		if r.URL.Path == "/api/agent/heartbeat" {
			beats.Add(1)
		}
		_, _ = io.WriteString(w, `{"success":true,"data":[]}`)
	}))
	defer server.Close()
	ctl := New(Client{BaseURL: baseURL(t, server)}, &fakeScheduler{}, WithInterval(10*time.Millisecond), WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ctl.Run(ctx) }()
	<-blocked
	deadline := time.Now().Add(time.Second)
	for beats.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if beats.Load() < 3 {
		t.Fatal("config blocked heartbeat cadence")
	}
}

func TestConfigRejectsMalformedQuietHours(t *testing.T) {
	if _, err := (AgentConfig{Schedule: ScheduleInput{Interval: 30}, Quiet: QuietHours{QuietStart: "01:00"}}).schedule(); err == nil {
		t.Fatal("invalid quiet window accepted")
	}
}

func TestUnacknowledgedHistoryNeverEvicted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	fake := &fakeScheduler{}
	ctl := New(Client{BaseURL: baseURL(t, server)}, fake)
	ctl.commands.max = 1
	ctl.handleCommand(context.Background(), Command{ID: "first", Type: "run_now"})
	ctl.handleCommand(context.Background(), Command{ID: "second", Type: "run_now"})
	ctl.handleCommand(context.Background(), Command{ID: "first", Type: "run_now"})
	_, _, runs := fake.counts()
	if runs != 1 || len(ctl.commands.store) != 1 {
		t.Fatal("unacknowledged command evicted or new command executed while full")
	}
}
