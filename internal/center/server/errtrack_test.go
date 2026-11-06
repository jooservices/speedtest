package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/jooservices/speedtest/internal/shared/errtrack"
)

type eventRecorder struct {
	mu     sync.Mutex
	events []*sentry.Event
}

func (r *eventRecorder) Configure(sentry.ClientOptions) {}
func (r *eventRecorder) SendEvent(e *sentry.Event) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}
func (r *eventRecorder) Flush(time.Duration) bool              { return true }
func (r *eventRecorder) FlushWithContext(context.Context) bool { return true }
func (r *eventRecorder) Close()                                {}
func (r *eventRecorder) count() int                            { r.mu.Lock(); defer r.mu.Unlock(); return len(r.events) }

func trackErrors(t *testing.T) *eventRecorder {
	t.Helper()
	rec := &eventRecorder{}
	if _, err := errtrack.Init(errtrack.Options{DSN: "https://public@sentry.example.test/1", Component: "center", SampleRate: 1, Transport: rec}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = errtrack.Init(errtrack.Options{}) })
	return rec
}

func TestServerErrorsAndPanicsReachErrorTracking(t *testing.T) {
	rec := trackErrors(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	const id = "10000000-0000-4000-8000-000000000001"
	h := logRequests(recoverPanics(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/nodes/" + id:
			w.WriteHeader(http.StatusInternalServerError)
		case "/api/boom":
			panic("handler exploded for owner@example.com")
		default:
			w.WriteHeader(http.StatusOK)
		}
	}), logger), logger)
	for _, path := range []string{"/api/nodes/" + id, "/api/boom", "/api/nodes", "/wp-login.php"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	// A 500 and a panic (which also ends as a 500) are reported; successes and unknown pages are not.
	if rec.count() != 3 {
		t.Fatalf("events = %d, want the 500, the panic and the panic's 500", rec.count())
	}
	for _, e := range rec.events {
		if e.Request != nil || e.User.IPAddress != "" {
			t.Fatalf("request data leaked into an event: %+v", e)
		}
	}
	first := rec.events[0]
	if first.Tags["route"] != "/api/nodes/{id}" || first.Tags["status"] != "500" || len(first.Fingerprint) != 3 {
		t.Fatalf("500 event: tags %v fingerprint %v", first.Tags, first.Fingerprint)
	}
}
