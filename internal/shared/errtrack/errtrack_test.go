package errtrack

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
)

type recorder struct {
	mu     sync.Mutex
	events []*sentry.Event
}

func (r *recorder) Configure(sentry.ClientOptions) {}
func (r *recorder) SendEvent(e *sentry.Event) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}
func (r *recorder) Flush(_ time.Duration) bool            { return true }
func (r *recorder) FlushWithContext(context.Context) bool { return true }
func (r *recorder) Close()                                {}

func start(t *testing.T) *recorder {
	t.Helper()
	rec := &recorder{}
	if _, err := Init(Options{DSN: "https://public@sentry.example.test/1", Environment: "test", Release: "9.9.9", Component: "center", SampleRate: 1, Transport: rec}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { enabled = false })
	return rec
}

func TestDisabledWithoutDSNIsANoOp(t *testing.T) {
	flush, err := Init(Options{})
	if err != nil || Enabled() {
		t.Fatalf("Init without DSN: enabled=%v err=%v", Enabled(), err)
	}
	flush()
	CaptureError(errors.New("x"), nil)
	CaptureMessage("m", nil, nil)
	CapturePanic("boom", nil)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("Recover must re-panic")
			}
		}()
		defer Recover(nil)
		panic("still stops")
	}()
}

func TestInvalidDSNKeepsTheProgramRunning(t *testing.T) {
	if _, err := Init(Options{DSN: "not a dsn"}); err == nil || Enabled() {
		t.Fatalf("a bad DSN must turn tracking off with an error: %v", err)
	}
}

func TestEventsCarryReleaseEnvironmentComponentAndTags(t *testing.T) {
	rec := start(t)
	CaptureError(errors.New("database unavailable"), map[string]string{"loop": "alerts"})
	CaptureMessage("HTTP 500", []string{"GET", "/api/x", "500"}, nil)
	CapturePanic("boom", map[string]string{"route": "/api/y"})
	if len(rec.events) != 3 {
		t.Fatalf("events = %d", len(rec.events))
	}
	e := rec.events[0]
	if e.Release != "9.9.9" || e.Environment != "test" || e.Tags["component"] != "center" || e.Tags["loop"] != "alerts" {
		t.Fatalf("event metadata: %+v tags %v", e, e.Tags)
	}
	if got := rec.events[1].Fingerprint; len(got) != 3 || got[1] != "/api/x" {
		t.Fatalf("fingerprint %v", got)
	}
	if len(rec.events[2].Exception) == 0 || !strings.Contains(rec.events[2].Exception[0].Value, "boom") {
		t.Fatalf("panic event: %+v", rec.events[2].Exception)
	}
}

func TestScrubRemovesRequestIdentityAndSecrets(t *testing.T) {
	e := &sentry.Event{
		Message:     "login failed for owner@example.com from 203.0.113.9 password=hunter2 Authorization: Bearer abc.def.ghi",
		Request:     &sentry.Request{URL: "https://x/api/nodes?token=1", Cookies: "speedtest_session=abc", Headers: map[string]string{"Authorization": "Bearer x"}, Data: `{"password":"p"}`},
		User:        sentry.User{ID: "1", Email: "owner@example.com", IPAddress: "203.0.113.9"},
		Exception:   []sentry.Exception{{Value: "enrol with token sp_abcdefghijklmnopqrstuvwxyz failed"}},
		Breadcrumbs: []*sentry.Breadcrumb{{Message: "GET /api/auth/me"}},
		Contexts:    map[string]sentry.Context{"body": {"x": "secret"}},
		Tags:        map[string]string{"node": "node-a@example.com"},
		ServerName:  "dev-workspaces",
	}
	got := Scrub(e)
	text := got.Message + " " + got.Exception[0].Value + " " + got.Tags["node"]
	for _, leak := range []string{"owner@example.com", "203.0.113.9", "hunter2", "abc.def.ghi", "sp_abcdefghijklmnop", "node-a@example.com"} {
		if strings.Contains(text, leak) {
			t.Errorf("%q survived scrubbing: %s", leak, text)
		}
	}
	if got.Request != nil || !reflect.DeepEqual(got.User, sentry.User{}) || got.Breadcrumbs != nil || got.Contexts != nil || got.ServerName != "" {
		t.Fatalf("request/user/breadcrumbs/contexts/server name must be cleared: %+v", got)
	}
	if Scrub(nil) != nil {
		t.Fatal("nil event")
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("SPEEDTEST_SENTRY_DSN", " https://k@s.example.test/2 ")
	t.Setenv("APP_ENV", "staging")
	t.Setenv("SPEEDTEST_SENTRY_ENVIRONMENT", "")
	t.Setenv("SPEEDTEST_SENTRY_SAMPLE_RATE", "")
	o, err := FromEnv("agent", "1.2.3")
	if err != nil || o.DSN != "https://k@s.example.test/2" || o.Environment != "staging" || o.SampleRate != 1 || o.Component != "agent" || o.Release != "1.2.3" {
		t.Fatalf("defaults: %+v %v", o, err)
	}
	t.Setenv("SPEEDTEST_SENTRY_ENVIRONMENT", "prod-eu")
	t.Setenv("SPEEDTEST_SENTRY_SAMPLE_RATE", "0.25")
	if o, err = FromEnv("center", "x"); err != nil || o.Environment != "prod-eu" || o.SampleRate != 0.25 {
		t.Fatalf("overrides: %+v %v", o, err)
	}
	for _, bad := range []string{"2", "-1", "lots"} {
		t.Setenv("SPEEDTEST_SENTRY_SAMPLE_RATE", bad)
		if _, err := FromEnv("center", "x"); err == nil {
			t.Errorf("sample rate %q accepted", bad)
		}
	}
	t.Setenv("SPEEDTEST_SENTRY_SAMPLE_RATE", "")
	t.Setenv("SPEEDTEST_SENTRY_ENVIRONMENT", "")
	t.Setenv("APP_ENV", "")
	if o, _ := FromEnv("center", "x"); o.Environment != "production" {
		t.Fatalf("fallback environment %q", o.Environment)
	}
}
