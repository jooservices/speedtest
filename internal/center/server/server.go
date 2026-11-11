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
	"strconv"
	"sync/atomic"
	"time"

	"github.com/jooservices/speedtest/internal/center/agentapi"
	"github.com/jooservices/speedtest/internal/center/apitoken"
	"github.com/jooservices/speedtest/internal/center/settings"
	"github.com/jooservices/speedtest/internal/center/telemetry"
	"github.com/jooservices/speedtest/internal/shared/errtrack"
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
	Pinger Pinger
	// Metrics optionally serves GET /metrics; MetricsToken, when set, is the bearer token required.
	Metrics      MetricsSource
	MetricsToken string
	// Telemetry wraps the entire chain when configured.
	Telemetry         *telemetry.Telemetry
	Auth              AuthService
	RequireHTTPS      bool
	TrustedProxyCIDRs string
	Logger            *slog.Logger
	UI                fs.FS
	ReadinessTimeout  time.Duration
	NodeCredentials   NodeCredentialService
	AgentRateLimit    int
	// OwnerNodes optionally serves the dashboard node API (list, detail, update, delete, commands).
	OwnerNodes OwnerNodesStore
	// OwnerSchedules optionally serves the dashboard schedule API.
	OwnerSchedules OwnerSchedulesStore
	// Audit optionally records owner changes and serves GET /api/audit.
	Audit AuditStore
	// OwnerJobs optionally serves the status of maintenance jobs.
	OwnerJobs OwnerJobsStore
	// OwnerDashboard optionally serves the dashboard snapshot and comparison series.
	OwnerDashboard OwnerDashboardStore
	// OwnerIP optionally serves the IP history, address, lookup and map views.
	OwnerIP            OwnerIPStore
	OwnerNotifications OwnerNotificationsStore
	// Settings optionally serves the settings API and enrolment-key rotation.
	Settings settings.Store
	// OwnerResults optionally serves the dashboard result API (list, detail, CSV export).
	OwnerResults OwnerResultsStore
	// OwnerStats optionally serves the owner statistics API.
	OwnerStats OwnerStatsStore
	// OwnerISP optionally serves ISP list, comparison, trend and plan APIs.
	OwnerISP OwnerISPStore
	// OwnerAlerts optionally serves alert-rule and event APIs.
	OwnerAlerts OwnerAlertsStore
	// Installer serves SSH fingerprint probes, install jobs, and replayable progress.
	Installer InstallerService
	// Install optionally serves /install.sh and /download/<artifact>.
	Install  http.Handler
	AgentAPI agentapi.Store
	// AccessTokens enables named owner bearer tokens and the authenticated /mcp endpoint.
	AccessTokens AccessTokenStore
	// AccessTokenRateLimit and AccessTokenBurst set each token's independent API rate bucket.
	AccessTokenRateLimit int
	AccessTokenBurst     int
}

// NewHandler builds the whole HTTP handler: owner APIs, agent API, health endpoints and the static UI, wrapped in
// the request id, access log, panic recovery, security headers and audit middleware.
func NewHandler(options Options) http.Handler {
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	readinessTimeout := options.ReadinessTimeout
	if readinessTimeout <= 0 {
		readinessTimeout = defaultReadinessTimeout
	}
	auth := &serverAuth{service: options.Auth, requireHTTPS: options.RequireHTTPS, trustedProxyCIDRs: options.TrustedProxyCIDRs,
		tokens: options.AccessTokens, tokenLimiter: newNodeRateLimiter(options.AccessTokenRateLimit, options.AccessTokenBurst)}
	owners := ownerAreas(options, auth)
	internalOwner := withAudit(dispatcher(owners, http.NotFoundHandler()), options.Audit, auth, logger)
	if options.AccessTokens != nil {
		owners.addToken(newMCPHandler(internalOwner), onPath(func(path string) bool { return path == "/mcp" }), func(*http.Request) (apitoken.Scope, bool) { return "", true })
	}
	platform := platformAreas(options, auth, readyz(options.Pinger, readinessTimeout))
	areas := make(routeTable, 0, len(owners)+len(platform))
	areas = append(areas, owners...)
	areas = append(areas, platform...)

	handler := withAudit(dispatcher(areas, uiHandler(options.UI)), options.Audit, auth, logger)
	handler = withAPITokenAuth(handler, owners, auth)
	handler = withSecurityHeaders(handler)
	handler = recoverPanics(handler, logger)
	handler = logRequests(handler, logger)
	handler = withRequestID(handler)
	if options.Telemetry != nil {
		handler = options.Telemetry.Middleware(handler)
	}
	return handler
}

// uiHandler serves the single-page UI files; GET only, anything else under it is a 404/405 from the file server.
func uiHandler(files fs.FS) http.Handler {
	handler := http.NotFoundHandler()
	if files != nil {
		handler = http.FileServer(http.FS(files))
	}
	mux := http.NewServeMux()
	mux.Handle("GET /", handler)
	return mux
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
		// Request bodies are small; a slow sender must not hold a connection for ever. There is deliberately no
		// WriteTimeout: CSV exports and installer progress are long-lived streams.
		ReadTimeout: 60 * time.Second,
		IdleTimeout: 2 * time.Minute,
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
	TraceID string `json:"trace_id"`
	// Errors lists field-level validation messages keyed by JSON path; it is omitted when empty.
	Errors map[string][]string `json:"errors,omitempty"`
}

func writePanicProblem(w http.ResponseWriter, requestID string) {
	const safeDetail = "Internal failure; safe detail only."
	writeProblemResponse(w, http.StatusInternalServerError, problemResponse{
		Type:    "/problems/servererror",
		Title:   safeDetail,
		Status:  http.StatusInternalServerError,
		Detail:  safeDetail,
		TraceID: requestID,
	})
}

func writeNotFoundProblem(w http.ResponseWriter, r *http.Request) {
	writeProblemResponse(w, http.StatusNotFound, problemResponse{
		Type:    "/problems/notfound",
		Title:   "Resource not found or not owned",
		Status:  http.StatusNotFound,
		Detail:  "Resource not found or not owned.",
		TraceID: requestIDFromContext(r.Context()),
	})
}

func writeMethodNotAllowedProblem(w http.ResponseWriter, r *http.Request) {
	writeProblemResponse(w, http.StatusMethodNotAllowed, problemResponse{
		Type:    "/problems/methodnotallowed",
		Title:   "Method not allowed",
		Status:  http.StatusMethodNotAllowed,
		Detail:  "The request method is not allowed for this resource.",
		TraceID: requestIDFromContext(r.Context()),
	})
}

func writeProblemResponse(w http.ResponseWriter, status int, problem problemResponse) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problem)
}

func serveGetRoute(w http.ResponseWriter, r *http.Request, handler http.Handler) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeMethodNotAllowedProblem(w, r)
		return
	}
	handler.ServeHTTP(w, r)
}

type requestIDKey struct{}

var requestIDFallbackSequence atomic.Uint64

func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestIDs := r.Header.Values(requestIDHeader)
		requestID := ""
		if len(requestIDs) == 1 && validRequestID(requestIDs[0]) {
			requestID = requestIDs[0]
		} else {
			requestID = newRequestID()
		}
		w.Header().Set(requestIDHeader, requestID)
		ctx := context.WithValue(r.Context(), requestIDKey{}, requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func validRequestID(requestID string) bool {
	if len(requestID) < 8 || len(requestID) > 64 {
		return false
	}
	for i := 0; i < len(requestID); i++ {
		c := requestID[i]
		if (c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
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
			recovered := recover()
			if recovered == nil {
				return
			}
			if recovered == http.ErrAbortHandler {
				// A handler that already sent part of a streamed body aborts the connection on purpose.
				panic(recovered)
			}

			requestID := requestIDFromContext(r.Context())
			errtrack.CapturePanic(recovered, map[string]string{"route": routeLabel(r.URL.Path), "method": r.Method})
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
		if status >= http.StatusInternalServerError {
			route := routeLabel(r.URL.Path)
			errtrack.CaptureMessage(fmt.Sprintf("%s %s answered %d", r.Method, route, status), []string{r.Method, route, strconv.Itoa(status)},
				map[string]string{"route": route, "method": r.Method, "status": strconv.Itoa(status)})
		}
		logger.InfoContext(r.Context(), "http request",
			slog.String("request_id", requestIDFromContext(r.Context())),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("remote_addr", r.RemoteAddr),
			slog.Int("status", status),
			slog.Int64("duration_ms", time.Since(started).Milliseconds()),
		)
	})
}
