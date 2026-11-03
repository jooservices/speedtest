package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

type pingFunc func(context.Context) error

func (p pingFunc) PingContext(ctx context.Context) error {
	return p(ctx)
}

func TestHealthz(t *testing.T) {
	var logs bytes.Buffer
	handler := NewHandler(Options{
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
		UI:     fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ui")}},
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	request.Header.Set(requestIDHeader, "bad id")
	request.RemoteAddr = "203.0.113.9:4321"
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want application/json; charset=utf-8", got)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	requestID := recorder.Header().Get(requestIDHeader)
	if len(requestID) != 32 || requestID == "bad id" {
		t.Fatalf("request id = %q, want a generated 32-character id", requestID)
	}

	var body healthResponse
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status body = %q, want ok", body.Status)
	}
	assertRequestLog(t, logs.String(), requestID, http.StatusOK, http.MethodGet, "/healthz", request.RemoteAddr)
}

func TestRequestIDValidationAndEcho(t *testing.T) {
	validIDs := []string{
		"request_1",
		"AbCd1234",
		"id-" + strings.Repeat("a", 61),
	}
	for _, incoming := range validIDs {
		t.Run("valid/"+incoming, func(t *testing.T) {
			var logs bytes.Buffer
			handler := NewHandler(Options{Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			request.Header.Set(requestIDHeader, incoming)
			handler.ServeHTTP(recorder, request)
			if got := recorder.Header().Get(requestIDHeader); got != incoming {
				t.Fatalf("response request id = %q, want incoming %q", got, incoming)
			}
			assertRequestLog(t, logs.String(), incoming, http.StatusOK, http.MethodGet, "/healthz", request.RemoteAddr)
		})
	}

	invalidIDs := []struct {
		name   string
		values []string
	}{
		{name: "too short", values: []string{"short"}},
		{name: "too long", values: []string{strings.Repeat("a", 65)}},
		{name: "space", values: []string{"invalid id"}},
		{name: "punctuation", values: []string{"request.id"}},
		{name: "non-ascii", values: []string{"café1234"}},
		{name: "duplicate headers", values: []string{"request_1", "request_2"}},
	}
	for _, tt := range invalidIDs {
		t.Run("invalid/"+tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			handler := NewHandler(Options{Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			for _, value := range tt.values {
				request.Header.Add(requestIDHeader, value)
			}
			handler.ServeHTTP(recorder, request)
			generated := recorder.Header().Get(requestIDHeader)
			if !validRequestID(generated) {
				t.Fatalf("response request id = %q, want a different valid generated id", generated)
			}
			for _, value := range tt.values {
				if generated == value {
					t.Fatalf("response request id = %q, want a different generated id", generated)
				}
			}
			assertRequestLog(t, logs.String(), generated, http.StatusOK, http.MethodGet, "/healthz", request.RemoteAddr)
		})
	}
}

func TestReadyz(t *testing.T) {
	tests := []struct {
		name       string
		pinger     Pinger
		timeout    time.Duration
		wantStatus int
		wantDB     string
	}{
		{
			name:       "not configured",
			wantStatus: http.StatusServiceUnavailable,
			wantDB:     "not configured",
		},
		{
			name: "database responds",
			pinger: pingFunc(func(context.Context) error {
				return nil
			}),
			wantStatus: http.StatusOK,
			wantDB:     "ok",
		},
		{
			name: "database fails",
			pinger: pingFunc(func(context.Context) error {
				return errors.New("sensitive database detail")
			}),
			wantStatus: http.StatusServiceUnavailable,
			wantDB:     "unavailable",
		},
		{
			name: "database ping times out",
			pinger: pingFunc(func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			}),
			timeout:    10 * time.Millisecond,
			wantStatus: http.StatusServiceUnavailable,
			wantDB:     "unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewHandler(Options{
				Pinger:           tt.pinger,
				ReadinessTimeout: tt.timeout,
				UI:               fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ui")}},
			})
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))

			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, tt.wantStatus)
			}
			var body readinessResponse
			if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if body.Database != tt.wantDB {
				t.Errorf("database = %q, want %q", body.Database, tt.wantDB)
			}
			if tt.wantStatus == http.StatusOK && body.Status != "ok" {
				t.Errorf("status body = %q, want ok", body.Status)
			}
			if tt.wantStatus != http.StatusOK && body.Status != "not_ready" {
				t.Errorf("status body = %q, want not_ready", body.Status)
			}
			if strings.Contains(recorder.Body.String(), "sensitive database detail") {
				t.Fatal("database error details must not be exposed")
			}
		})
	}
}

func TestStaticUIAndNotFound(t *testing.T) {
	ui := fstest.MapFS{
		"index.html":         &fstest.MapFile{Data: []byte("<title>embedded UI</title>")},
		"assets/css/app.css": &fstest.MapFile{Data: []byte("body { color: black; }")},
	}
	tests := []struct {
		path       string
		wantStatus int
		wantBody   string
	}{
		{path: "/", wantStatus: http.StatusOK, wantBody: "<title>embedded UI</title>"},
		{path: "/assets/css/app.css", wantStatus: http.StatusOK, wantBody: "body { color: black; }"},
		{path: "/missing.js", wantStatus: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			var logs bytes.Buffer
			handler := NewHandler(Options{
				Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
				UI:     ui,
			})
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, tt.wantStatus)
			}
			requestID := recorder.Header().Get(requestIDHeader)
			if requestID == "" {
				t.Fatal("request id header is empty")
			}
			assertRequestLog(t, logs.String(), requestID, tt.wantStatus, http.MethodGet, tt.path, httptest.NewRequest(http.MethodGet, tt.path, nil).RemoteAddr)
			if tt.wantBody != "" && !strings.Contains(recorder.Body.String(), tt.wantBody) {
				t.Errorf("body = %q, want it to contain %q", recorder.Body.String(), tt.wantBody)
			}
			if tt.path == "/missing.js" {
				if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
					t.Errorf("non-API missing asset Content-Type = %q, want text/plain", got)
				}
				if got := recorder.Body.String(); got != "404 page not found\n" {
					t.Errorf("non-API missing asset body = %q, want the static file-server 404", got)
				}
			}
		})
	}
}

func TestUnknownAPIPathReturnsProblem(t *testing.T) {
	for _, target := range []string{"/api/unknown?token=do-not-log", "/api"} {
		var logs bytes.Buffer
		handler := NewHandler(Options{Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
		request := httptest.NewRequest(http.MethodGet, target, nil)
		request.Header.Set(requestIDHeader, "client_req-123")
		request.RemoteAddr = "198.51.100.4:5678"
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)

		assertProblemResponse(t, recorder, http.StatusNotFound, "/problems/notfound", "Resource not found or not owned", "Resource not found or not owned.", "client_req-123")
		assertRequestLog(t, logs.String(), "client_req-123", http.StatusNotFound, http.MethodGet, request.URL.Path, request.RemoteAddr)
		if strings.Contains(logs.String(), "do-not-log") {
			t.Fatal("query string was written to the access log")
		}
	}
}

func TestNonAPIWrongMethodKeepsStaticUIBehavior(t *testing.T) {
	handler := NewHandler(Options{UI: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ui")}}})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/missing.js", nil))

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusMethodNotAllowed)
	}
	if got := recorder.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q, want GET, HEAD", got)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("Content-Type = %q, want static ServeMux text/plain response", got)
	}
}

func TestWrongMethodOnKnownRouteReturnsProblemAndAllow(t *testing.T) {
	for _, path := range []string{"/healthz", "/readyz"} {
		var logs bytes.Buffer
		handler := NewHandler(Options{Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
		request := httptest.NewRequest(http.MethodPost, path, nil)
		request.Header.Set(requestIDHeader, "method_req-123")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)

		assertProblemResponse(t, recorder, http.StatusMethodNotAllowed, "/problems/methodnotallowed", "Method not allowed", "The request method is not allowed for this resource.", "method_req-123")
		if got := recorder.Header().Get("Allow"); got != "GET, HEAD" {
			t.Errorf("Allow = %q, want GET, HEAD", got)
		}
		assertRequestLog(t, logs.String(), "method_req-123", http.StatusMethodNotAllowed, http.MethodPost, path, request.RemoteAddr)
	}
}

func assertProblemResponse(t *testing.T, recorder *httptest.ResponseRecorder, status int, problemType, title, detail, traceID string) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d", recorder.Code, status)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", got)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := recorder.Header().Get(requestIDHeader); got != traceID {
		t.Errorf("response request id = %q, want trace_id %q", got, traceID)
	}
	var body problemResponse
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatalf("decode problem response: %v", err)
	}
	if body.Type != problemType || body.Title != title || body.Status != status || body.Detail != detail || body.TraceID != traceID {
		t.Errorf("problem = %+v, want type=%q title=%q status=%d detail=%q trace_id=%q", body, problemType, title, status, detail, traceID)
	}
}

func TestNilUIReturnsNotFound(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewHandler(Options{}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
}

func TestPanicRecoveryReturnsProblemEnvelope(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("private panic details")
	})
	handler := withRequestID(logRequests(recoverPanics(next, logger), logger))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/panic", nil)
	request.Header.Set(requestIDHeader, "panic_req-123")
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", got)
	}
	requestID := recorder.Header().Get(requestIDHeader)
	if requestID != "panic_req-123" {
		t.Fatalf("request id = %q, want panic_req-123", requestID)
	}
	var body problemResponse
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Type != "/problems/servererror" || body.Status != http.StatusInternalServerError {
		t.Errorf("problem = %+v, want server error type and status", body)
	}
	if body.TraceID != requestID || requestID == "" {
		t.Errorf("trace_id = %q, request id = %q", body.TraceID, requestID)
	}
	if strings.Contains(recorder.Body.String(), "private panic details") || strings.Contains(logs.String(), "private panic details") {
		t.Fatal("panic details must not be exposed in the response or logs")
	}
	assertRequestLog(t, logs.String(), requestID, http.StatusInternalServerError, http.MethodGet, "/panic", request.RemoteAddr)
}

func TestRequestIDsAreUnique(t *testing.T) {
	first := newRequestID()
	second := newRequestID()
	if first == "" || second == "" || first == second {
		t.Fatalf("request ids = %q and %q, want distinct non-empty values", first, second)
	}
}

func TestStatusWriterTracksFirstStatusAndWrites(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer := &statusWriter{ResponseWriter: recorder}
	writer.WriteHeader(http.StatusCreated)
	writer.WriteHeader(http.StatusAccepted)
	if _, err := writer.Write([]byte("created")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if writer.status != http.StatusCreated || recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d / %d, want first status %d", writer.status, recorder.Code, http.StatusCreated)
	}
	if got := recorder.Body.String(); got != "created" {
		t.Errorf("body = %q, want created", got)
	}
	if writer.Unwrap() != recorder {
		t.Error("Unwrap() did not return the underlying response writer")
	}

	implicitRecorder := httptest.NewRecorder()
	implicitWriter := &statusWriter{ResponseWriter: implicitRecorder}
	if _, err := implicitWriter.Write([]byte("implicit")); err != nil {
		t.Fatalf("implicit Write() error = %v", err)
	}
	if implicitWriter.status != http.StatusOK || implicitRecorder.Code != http.StatusOK {
		t.Errorf("implicit status = %d / %d, want %d", implicitWriter.status, implicitRecorder.Code, http.StatusOK)
	}
}

func TestRequestLogDefaultsToStatusOK(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	handler := withRequestID(logRequests(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), logger))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/empty", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	assertRequestLog(t, logs.String(), recorder.Header().Get(requestIDHeader), http.StatusOK, http.MethodGet, "/empty", httptest.NewRequest(http.MethodGet, "/empty", nil).RemoteAddr)
}

func TestServeGracefulShutdown(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}

	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { close(releaseRequest) })
	}
	defer release()

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-releaseRequest
		w.WriteHeader(http.StatusNoContent)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- Serve(ctx, listener, handler)
	}()

	responseDone := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 3 * time.Second}
		response, err := client.Get("http://" + listener.Addr().String())
		if err != nil {
			responseDone <- err
			return
		}
		status := response.StatusCode
		if err := response.Body.Close(); err != nil {
			responseDone <- err
			return
		}
		if status != http.StatusNoContent {
			responseDone <- errors.New("response status was not 204")
			return
		}
		responseDone <- nil
	}()

	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach the server")
	}
	cancel()
	select {
	case err := <-serveDone:
		t.Fatalf("Serve returned before active request finished: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	release()
	select {
	case err := <-responseDone:
		if err != nil {
			t.Fatalf("HTTP request error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP request did not finish")
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve() error = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve() did not finish after graceful shutdown")
	}
}

func TestServeReturnsListenerError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("listener.Close() error = %v", err)
	}

	if err := Serve(context.Background(), listener, http.NotFoundHandler()); err == nil {
		t.Fatal("Serve() error = nil for closed listener, want an error")
	}
}

func TestServeClosesConnectionsAfterShutdownTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { close(releaseRequest) })
	}
	defer release()

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-releaseRequest
		w.WriteHeader(http.StatusNoContent)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- serve(ctx, listener, handler, 10*time.Millisecond)
	}()

	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		response, err := (&http.Client{Timeout: time.Second}).Get("http://" + listener.Addr().String())
		if err != nil {
			return
		}
		if err := response.Body.Close(); err != nil {
			return
		}
	}()

	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach the server")
	}
	cancel()
	select {
	case err := <-serveDone:
		if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
			t.Fatalf("serve() error = %v, want shutdown timeout error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve() did not return after shutdown timeout")
	}
	release()
	select {
	case <-requestDone:
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP request did not finish after connection close")
	}
}

func TestServeRejectsMissingArguments(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Errorf("listener.Close() error = %v", err)
		}
	})

	if err := Serve(context.Background(), nil, http.NotFoundHandler()); err == nil {
		t.Error("Serve() with nil listener error = nil, want an error")
	}
	if err := Serve(context.Background(), listener, nil); err == nil {
		t.Error("Serve() with nil handler error = nil, want an error")
	}
}

func assertRequestLog(t *testing.T, output, requestID string, status int, method, path, remoteAddr string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		var fields map[string]any
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		if fields["msg"] != "http request" {
			continue
		}
		if fields["request_id"] != requestID {
			t.Errorf("logged request_id = %v, want %q", fields["request_id"], requestID)
		}
		if fields["status"] != float64(status) {
			t.Errorf("logged status = %v, want %d", fields["status"], status)
		}
		if fields["method"] != method {
			t.Errorf("logged method = %v, want %s", fields["method"], method)
		}
		if fields["path"] != path {
			t.Errorf("logged path = %v, want %q", fields["path"], path)
		}
		if fields["remote_addr"] != remoteAddr {
			t.Errorf("logged remote_addr = %v, want %q", fields["remote_addr"], remoteAddr)
		}
		if _, ok := fields["duration_ms"].(float64); !ok {
			t.Errorf("logged duration_ms = %T(%v), want a JSON number", fields["duration_ms"], fields["duration_ms"])
		}
		return
	}
	t.Errorf("no HTTP request log found in %q", output)
}

var _ io.Writer = (*statusWriter)(nil)

func TestInstallRoutesAreOptional(t *testing.T) {
	without := NewHandler(Options{})
	for _, path := range []string{"/install.sh", "/download/SHA256SUMS"} {
		rec := httptest.NewRecorder()
		without.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s without an installer: status = %d, want 404", path, rec.Code)
		}
	}
	marker := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("installer:" + r.URL.Path))
	})
	with := NewHandler(Options{Install: marker})
	for _, path := range []string{"/install.sh", "/download/SHA256SUMS"} {
		rec := httptest.NewRecorder()
		with.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || rec.Body.String() != "installer:"+path {
			t.Fatalf("%s with an installer: status = %d body = %q", path, rec.Code, rec.Body.String())
		}
	}
}
