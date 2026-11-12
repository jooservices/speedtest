package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/agent/engine"
	"github.com/jooservices/speedtest/internal/agent/sched"
	"github.com/jooservices/speedtest/internal/agent/testutil"
)

func baseURL(t *testing.T, s *httptest.Server) *url.URL {
	t.Helper()
	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	return u
}

func configJSON() string {
	return `{"revision":"2","name":"viettel-fast-50","expectedDown":300,"expectedUp":150,` +
		`"schedule":{"interval":45,"offset":7,"enabled":true},` +
		`"quiet":{"quietStart":"02:00","quietEnd":"03:00","timezone":"UTC"}}`
}

func TestClientHeartbeat(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody Heartbeat
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true,"code":200,"message":"ok","data":{"receivedAt":"2026-11-03T00:00:00Z"},"meta":{},"errors":null}`))
	}))
	defer s.Close()

	lastTest := "2026-11-03T00:00:00Z"
	err := Client{BaseURL: baseURL(t, s), Credential: "secret", Agent: "speedtest-agent/1.2.3"}.
		Heartbeat(context.Background(), Heartbeat{Agent: "speedtest-agent/1.2.3", Uptime: 42, LastTest: &lastTest})
	if err != nil {
		t.Fatalf("Heartbeat() error = %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/agent/heartbeat" {
		t.Fatalf("request = %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer secret" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if gotBody.Agent != "speedtest-agent/1.2.3" || gotBody.Uptime != 42 || gotBody.LastTest == nil || *gotBody.LastTest != lastTest {
		t.Fatalf("heartbeat body = %+v", gotBody)
	}
}

func TestClientConfigConditional(t *testing.T) {
	var ifNoneMatch string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ifNoneMatch = r.Header.Get("If-None-Match")
		if ifNoneMatch == `"v2"` {
			w.Header().Set("ETag", `"v2"`)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"v2"`)
		_, _ = w.Write([]byte(`{"success":true,"code":200,"message":"ok","data":` + configJSON() + `,"meta":{},"errors":null}`))
	}))
	defer s.Close()

	client := Client{BaseURL: baseURL(t, s), Credential: "secret", Agent: "speedtest-agent/1.2.3"}

	cfg, etag, notModified, err := client.Config(context.Background(), "")
	if err != nil {
		t.Fatalf("Config() error = %v", err)
	}
	if notModified {
		t.Fatal("Config() notModified = true on first fetch")
	}
	if etag != `"v2"` {
		t.Fatalf("etag = %q, want \"v2\"", etag)
	}
	if cfg.Schedule.Interval != 45 || cfg.Schedule.Offset != 7 || !cfg.Schedule.Enabled {
		t.Fatalf("config schedule = %+v", cfg.Schedule)
	}
	if cfg.Quiet.QuietStart != "02:00" || cfg.Quiet.QuietEnd != "03:00" {
		t.Fatalf("config quiet = %+v", cfg.Quiet)
	}

	// Second fetch with the stored etag must return 304.
	_, gotETag, notModified, err := client.Config(context.Background(), `"v2"`)
	if err != nil {
		t.Fatalf("Config() error = %v", err)
	}
	if !notModified {
		t.Fatal("Config() notModified = false on unchanged etag")
	}
	if gotETag != `"v2"` {
		t.Fatalf("etag = %q, want \"v2\"", gotETag)
	}
	if ifNoneMatch != `"v2"` {
		t.Fatalf("If-None-Match = %q, want \"v2\"", ifNoneMatch)
	}
}

func TestClientPollCommands(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"code":200,"message":"ok","data":[` +
			`{"id":"c1","nodeId":"n1","type":"run_now","version":null,"status":"pending","createdAt":"2026-11-03T00:00:00Z","ackedAt":null,"phase":"","pct":0,"resultId":null,"error":null}` +
			`],"meta":{},"errors":null}`))
	}))
	defer s.Close()

	cmds, err := Client{BaseURL: baseURL(t, s), Credential: "secret"}.PollCommands(context.Background())
	if err != nil {
		t.Fatalf("PollCommands() error = %v", err)
	}
	if len(cmds) != 1 || cmds[0].ID != "c1" || cmds[0].Type != "run_now" {
		t.Fatalf("commands = %+v", cmds)
	}
}

func TestClientAck(t *testing.T) {
	var gotPath string
	var gotBody CommandAck
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"code":200,"message":"ok","data":{"id":"c1"},"meta":{},"errors":null}`))
	}))
	defer s.Close()

	err := Client{BaseURL: baseURL(t, s), Credential: "secret"}.Ack(context.Background(), "c1", CommandAck{Status: "succeeded"})
	if err != nil {
		t.Fatalf("Ack() error = %v", err)
	}
	if gotPath != "/api/agent/commands/c1/ack" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotBody.Status != "succeeded" {
		t.Fatalf("ack body = %+v", gotBody)
	}
}

func TestClientProblem(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"about:blank","title":"Unauthorized","status":401,"detail":"bad credential"}`))
	}))
	defer s.Close()

	err := Client{BaseURL: baseURL(t, s), Credential: "secret"}.Heartbeat(context.Background(), Heartbeat{})
	if err == nil || !strings.Contains(err.Error(), "bad credential") {
		t.Fatalf("Heartbeat() error = %v, want problem detail", err)
	}
}

func TestClientNilBaseURL(t *testing.T) {
	err := Client{}.Heartbeat(context.Background(), Heartbeat{})
	if err == nil || !strings.Contains(err.Error(), "center URL is required") {
		t.Fatalf("Heartbeat() error = %v", err)
	}
}

// fakeScheduler records scheduler interactions for the controller tests.
type fakeScheduler struct {
	mu        sync.Mutex
	schedules []sched.Schedule
	paused    int
	resumed   int
	runNow    int
	lastRun   time.Time
}

func (f *fakeScheduler) SetSchedule(s sched.Schedule) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.schedules = append(f.schedules, s)
}

func (f *fakeScheduler) Pause() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused++
}

func (f *fakeScheduler) Resume() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumed++
}

func (f *fakeScheduler) RunNow() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runNow++
}

func (f *fakeScheduler) LastRun() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastRun
}

func (f *fakeScheduler) scheduleCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.schedules)
}

func (f *fakeScheduler) lastSchedule() sched.Schedule {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.schedules) == 0 {
		return sched.Schedule{}
	}
	return f.schedules[len(f.schedules)-1]
}

func (f *fakeScheduler) counts() (int, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paused, f.resumed, f.runNow
}

func newController(t *testing.T, c *testutil.Center, sched *fakeScheduler) *Controller {
	t.Helper()
	u, err := url.Parse(c.URL)
	if err != nil {
		t.Fatalf("parse center URL: %v", err)
	}
	client := Client{BaseURL: u, Credential: "test-node-credential", Agent: "speedtest-agent/1.2.3"}
	return New(client, sched, WithNow(func() time.Time { return time.Unix(100, 0) }))
}

func TestControllerHeartbeatConfigAndCommands(t *testing.T) {
	center := testutil.NewCenter(
		testutil.WithConfig("2", testutil.AgentConfigFull{
			Revision:     "2",
			Name:         "node",
			ExpectedDown: 300,
			ExpectedUp:   150,
			Schedule:     testutil.ScheduleInput{Interval: 45, Offset: 7, Enabled: true},
			Quiet:        testutil.QuietHours{QuietStart: "02:00", QuietEnd: "03:00", Timezone: "UTC"},
		}),
		testutil.WithCommands(
			testutil.Command{ID: "c1", Type: "run_now"},
			testutil.Command{ID: "c2", Type: "pause"},
			testutil.Command{ID: "c3", Type: "resume"},
			testutil.Command{ID: "c4", Type: "update_config"},
		),
	)
	t.Cleanup(center.Close)

	fake := &fakeScheduler{}
	ctl := newController(t, center, fake)
	ctl.Once(context.Background())

	// Heartbeat was sent with the agent identity.
	hbs := center.Heartbeats()
	if len(hbs) != 1 || hbs[0].Agent != "speedtest-agent/1.2.3" {
		t.Fatalf("heartbeats = %+v", hbs)
	}

	// Configuration was applied once.
	if fake.scheduleCount() != 2 {
		t.Fatalf("schedule applied %d times, want 2 (periodic sync + update_config)", fake.scheduleCount())
	}
	sch := fake.lastSchedule()
	if sch.Interval != 45*time.Minute || sch.Offset != 7*time.Minute || !sch.Enabled {
		t.Fatalf("schedule = %+v", sch)
	}
	if sch.Quiet.Start != "02:00" || sch.Quiet.End != "03:00" {
		t.Fatalf("quiet = %+v", sch.Quiet)
	}

	// Commands were executed and acknowledged.
	paused, resumed, runNow := fake.counts()
	if paused != 1 || resumed != 1 || runNow != 1 {
		t.Fatalf("command counts = pause %d resume %d run_now %d, want 1 each", paused, resumed, runNow)
	}
	acks := center.Acks()
	if len(acks) != 4 {
		t.Fatalf("acks = %d, want 4", len(acks))
	}
	for _, a := range acks {
		if a.Ack.Status != "succeeded" {
			t.Fatalf("ack %+v not succeeded", a)
		}
	}
	ids := map[string]bool{}
	for _, a := range acks {
		ids[a.CommandID] = true
	}
	for _, want := range []string{"c1", "c2", "c3", "c4"} {
		if !ids[want] {
			t.Fatalf("missing ack for %s", want)
		}
	}
}

func TestControllerETagSecondSyncNotModified(t *testing.T) {
	center := testutil.NewCenter(testutil.WithConfig("1", testutil.AgentConfigFull{
		Revision: "1", Name: "node", ExpectedDown: 300, ExpectedUp: 150,
		Schedule: testutil.ScheduleInput{Interval: 30, Offset: 0, Enabled: true},
		Quiet:    testutil.QuietHours{Timezone: "UTC"},
	}))
	t.Cleanup(center.Close)

	fake := &fakeScheduler{}
	ctl := newController(t, center, fake)

	ctl.Once(context.Background())
	ctl.Once(context.Background())

	// First sync sends no If-None-Match, second sends the stored etag and is
	// answered with 304, so the schedule is applied only once.
	reqs := center.ConfigRequests()
	if len(reqs) != 2 || reqs[0] != "" || reqs[1] != "1" {
		t.Fatalf("config requests = %q, want [\"\", \"1\"]", reqs)
	}
	if fake.scheduleCount() != 1 {
		t.Fatalf("schedule applied %d times, want 1", fake.scheduleCount())
	}
}

func TestListServersCommandUsesRequestedEngine(t *testing.T) {
	center := testutil.NewCenter(testutil.WithCommands(testutil.Command{ID: "servers", Type: "list_servers", Engine: "librespeed"}))
	t.Cleanup(center.Close)
	ctl := newController(t, center, &fakeScheduler{})
	called := ""
	ctl.ListServers = func(_ context.Context, name string) ([]engine.ServerOption, error) {
		called = name
		return []engine.ServerOption{{ID: 7, Name: "Singapore"}}, nil
	}
	ctl.Once(context.Background())
	if called != "librespeed" {
		t.Fatalf("ListServers engine = %q", called)
	}
	acks := center.Acks()
	if len(acks) != 1 || acks[0].CommandID != "servers" || acks[0].Ack.Status != "succeeded" {
		t.Fatalf("server list acknowledgement = %+v", acks)
	}
}

func TestControllerUnreachableCenterKeepsSchedule(t *testing.T) {
	center := testutil.NewCenter()
	u, err := url.Parse(center.URL)
	if err != nil {
		t.Fatalf("parse center URL: %v", err)
	}
	center.Close()

	fake := &fakeScheduler{}
	client := Client{BaseURL: u, Credential: "test-node-credential", Agent: "speedtest-agent/1.2.3"}
	ctl := New(client, fake, WithNow(func() time.Time { return time.Unix(100, 0) }))

	ctl.Once(context.Background())

	if fake.scheduleCount() != 0 {
		t.Fatalf("schedule applied %d times on unreachable center, want 0", fake.scheduleCount())
	}
	if fake.paused != 0 || fake.resumed != 0 || fake.runNow != 0 {
		t.Fatalf("commands executed on unreachable center")
	}
}

func TestControllerRedeliveredCommandNotReexecuted(t *testing.T) {
	center := testutil.NewCenter(testutil.WithCommands(testutil.Command{ID: "c1", Type: "run_now"}))
	t.Cleanup(center.Close)

	fake := &fakeScheduler{}
	ctl := newController(t, center, fake)

	ctl.Once(context.Background())
	ctl.Once(context.Background())

	_, _, runNow := fake.counts()
	if runNow != 1 {
		t.Fatalf("run_now executed %d times, want 1", runNow)
	}
	if len(center.Acks()) != 1 {
		t.Fatalf("acks = %d, want 1", len(center.Acks()))
	}
}

func TestControllerUnsupportedCommandAckFailed(t *testing.T) {
	center := testutil.NewCenter(testutil.WithCommands(testutil.Command{ID: "c9", Type: "reboot"}))
	t.Cleanup(center.Close)

	fake := &fakeScheduler{}
	ctl := newController(t, center, fake)
	ctl.Once(context.Background())

	acks := center.Acks()
	if len(acks) != 1 {
		t.Fatalf("acks = %d, want 1", len(acks))
	}
	if acks[0].Ack.Status != "failed" || acks[0].Ack.Error == nil || !strings.Contains(*acks[0].Ack.Error, "unsupported command") {
		t.Fatalf("ack = %+v, want failed with unsupported detail", acks[0])
	}
}

func TestAgentConfigScheduleInvalidTimezone(t *testing.T) {
	cfg := AgentConfig{
		Schedule: ScheduleInput{Interval: 30, Offset: 0, Enabled: true},
		Quiet:    QuietHours{Timezone: "Not/AZone"},
	}
	_, err := cfg.schedule()
	if err == nil || !strings.Contains(err.Error(), "invalid timezone") {
		t.Fatalf("schedule() error = %v, want invalid timezone", err)
	}
}

func TestAgentConfigScheduleInvalidOffset(t *testing.T) {
	cfg := AgentConfig{
		Schedule: ScheduleInput{Interval: 30, Offset: 45, Enabled: true},
	}
	_, err := cfg.schedule()
	if err == nil || !strings.Contains(err.Error(), "offset") {
		t.Fatalf("schedule() error = %v, want offset error", err)
	}
}
