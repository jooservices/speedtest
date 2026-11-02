// Package server provides the command-center HTTP server skeleton.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

const (
	defaultReadinessTimeout = 2 * time.Second
	shutdownTimeout         = 10 * time.Second
	requestIDHeader         = "X-Request-ID"
)

// Pinger reports whether the configured database is reachable.
type Pinger interface {
	PingContext(context.Context) error
}

// Options configures the HTTP handlers.
type Options struct {
	Pinger           Pinger
	Logger           *slog.Logger
	UI               fs.FS
	ReadinessTimeout time.Duration
}

// NewHandler builds the health, readiness, and static UI routes.
func NewHandler(options Options) http.Handler {
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}

	readinessTimeout := options.ReadinessTimeout
	if readinessTimeout <= 0 {
		readinessTimeout = defaultReadinessTimeout
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /readyz", readyz(options.Pinger, readinessTimeout))
	if options.UI == nil {
		mux.Handle("GET /", http.NotFoundHandler())
	} else {
		mux.Handle("GET /", http.FileServer(http.FS(options.UI)))
	}

	var handler http.Handler = mux
	handler = recoverPanics(handler, logger)
	handler = logRequests(handler, logger)
	handler = withRequestID(handler)
	return handler
}

// Serve runs an HTTP server on listener until it returns an error or ctx is
// canceled. Cancellation performs a bounded graceful shutdown.
func Serve(ctx context.Context, listener net.Listener, handler http.Handler) error {
	return serve(ctx, listener, handler, shutdownTimeout)
}

func serve(ctx context.Context, listener net.Listener, handler http.Handler, shutdownDuration time.Duration) error {
	if listener == nil {
		return errors.New("listener must not be nil")
	}
	if handler == nil {
		return errors.New("handler must not be nil")
	}

	httpServer := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- httpServer.Serve(listener)
	}()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownDuration)
		defer cancel()

		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			_ = httpServer.Close()
			return fmt.Errorf("shut down HTTP server: %w", err)
		}

		err := <-serveErr
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

type healthResponse struct {
	Status string `json:"status"`
}

type readinessResponse struct {
	Status   string `json:"status"`
	Database string `json:"database"`
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
}

func readyz(pinger Pinger, timeout time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if pinger == nil {
			writeJSON(w, http.StatusServiceUnavailable, readinessResponse{
				Status:   "not_ready",
				Database: "not configured",
			})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		if err := pinger.PingContext(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, readinessResponse{
				Status:   "not_ready",
				Database: "unavailable",
			})
			return
		}

		writeJSON(w, http.StatusOK, readinessResponse{
			Status:   "ok",
			Database: "ok",
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

type problemResponse struct {
	Type    string `json:"type"`
	Title   string `json:"title"`
	Status  int    `json:"status"`
	Detail  string `json:"detail"`
	TraceID string `json:"trace_id,omitempty"`
}

func writePanicProblem(w http.ResponseWriter, requestID string) {
	const safeDetail = "Internal failure; safe detail only."
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusInternalServerError)
	_ = json.NewEncoder(w).Encode(problemResponse{
		Type:    "/problems/servererror",
		Title:   "Internal failure; safe detail only",
		Status:  http.StatusInternalServerError,
		Detail:  safeDetail,
		TraceID: requestID,
	})
}

type requestIDKey struct{}

var requestIDFallbackSequence atomic.Uint64

func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := newRequestID()
		w.Header().Set(requestIDHeader, requestID)
		ctx := context.WithValue(r.Context(), requestIDKey{}, requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func requestIDFromContext(ctx context.Context) string {
	requestID, _ := ctx.Value(requestIDKey{}).(string)
	return requestID
}

func newRequestID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err == nil {
		return hex.EncodeToString(bytes[:])
	}
	return fmt.Sprintf("fallback-%x", requestIDFallbackSequence.Add(1))
}

func recoverPanics(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recover() == nil {
				return
			}

			requestID := requestIDFromContext(r.Context())
			logger.ErrorContext(r.Context(), "request panic recovered",
				slog.String("request_id", requestID),
				slog.String("method", r.Method),
			)
			writePanicProblem(w, requestID)
		}()
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

// Unwrap allows http.ResponseController to reach the original writer.
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func logRequests(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		response := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(response, r)
		status := response.status
		if status == 0 {
			status = http.StatusOK
		}
		logger.InfoContext(r.Context(), "http request",
			slog.String("request_id", requestIDFromContext(r.Context())),
			slog.String("method", r.Method),
			slog.Int("status", status),
			slog.Int64("duration_ms", time.Since(started).Milliseconds()),
		)
	})
}
