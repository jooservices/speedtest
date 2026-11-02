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
	request.Header.Set(requestIDHeader, "client-supplied")
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
	if len(requestID) != 32 || requestID == "client-supplied" {
		t.Fatalf("request id = %q, want a generated 32-character id", requestID)
	}

	var body healthResponse
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status body = %q, want ok", body.Status)
	}
	assertRequestLog(t, logs.String(), requestID, http.StatusOK)
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
			assertRequestLog(t, logs.String(), requestID, tt.wantStatus)
			if tt.wantBody != "" && !strings.Contains(recorder.Body.String(), tt.wantBody) {
				t.Errorf("body = %q, want it to contain %q", recorder.Body.String(), tt.wantBody)
			}
		})
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
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/panic", nil))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", got)
	}
	requestID := recorder.Header().Get(requestIDHeader)
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
	assertRequestLog(t, logs.String(), requestID, http.StatusInternalServerError)
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
	assertRequestLog(t, logs.String(), recorder.Header().Get(requestIDHeader), http.StatusOK)
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

func assertRequestLog(t *testing.T, output, requestID string, status int) {
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
		if fields["method"] != http.MethodGet {
			t.Errorf("logged method = %v, want GET", fields["method"])
		}
		return
	}
	t.Errorf("no HTTP request log found in %q", output)
}

var _ io.Writer = (*statusWriter)(nil)
