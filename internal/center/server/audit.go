package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jooservices/speedtest/internal/center/audit"
)

// AuditStore records owner changes and lists them.
type AuditStore interface {
	Record(ctx context.Context, e audit.Entry) error
	List(ctx context.Context, f audit.Filter, page, size int) ([]audit.Entry, int, error)
}

// withAudit records every successful owner write (see audit.Classify). The actor is read from the session before
// the request runs, so a password change or a sign-out is attributed to the session that made it. Recording is
// best effort: a failure is logged and never changes the response.
func withAudit(next http.Handler, store AuditStore, auth *serverAuth, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if store == nil || r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		entry, ok := audit.Classify(r.Method, r.URL.Path)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		actor := auth.actor(r)
		response := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(response, r)
		status := response.status
		if status == 0 {
			status = http.StatusOK
		}
		if actor == "" || status < 200 || status > 299 {
			return
		}
		entry.Actor = actor
		entry.Remote = remoteIP(r.RemoteAddr, r.Header.Get("X-Forwarded-For"), auth.trustedProxyCIDRs)
		if err := store.Record(context.WithoutCancel(r.Context()), entry); err != nil && logger != nil {
			logger.ErrorContext(r.Context(), "audit entry not recorded", slog.String("action", entry.Action), slog.String("request_id", requestIDFromContext(r.Context())))
		}
	})
}

// actor returns the signed-in owner's e-mail for the request's session, or "" without a valid session.
func (s *serverAuth) actor(r *http.Request) string {
	if s == nil || s.service == nil {
		return ""
	}
	token, present, invalid := readSessionCookie(r)
	if !present || invalid {
		return ""
	}
	session, err := s.service.Session(r.Context(), token)
	if err != nil || session.Identity == nil {
		return ""
	}
	if session.Identity.Email != "" {
		return session.Identity.Email
	}
	return session.Identity.ID
}

func auditRoute(path string) bool { return path == "/api/audit" }

func newOwnerAuditHandler(store AuditStore, auth *serverAuth) http.Handler {
	limiter := newOwnerRateLimiter()
	return auth.requireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, err := ownerPrincipal(r)
		if err != nil {
			writeUnauthorized(w, r)
			return
		}
		if !allowRate(limiter, "owner:"+principal, w, r) {
			return
		}
		if r.Method != http.MethodGet {
			methodNotAllowed(w, r, http.MethodGet)
			return
		}
		page, size, ok := paging(w, r)
		if !ok {
			return
		}
		filter := audit.Filter{Action: strings.TrimSpace(r.URL.Query().Get("action"))}
		if len(filter.Action) > 60 {
			writeAPIProblem(w, r, http.StatusUnprocessableEntity, "Validation failed", "action must be at most 60 characters.")
			return
		}
		items, total, err := store.List(r.Context(), filter, page, size)
		if err != nil {
			writeAPIProblem(w, r, http.StatusInternalServerError, "Internal failure", "Internal failure; safe detail only.")
			return
		}
		last := (total + size - 1) / size
		if last < 1 {
			last = 1
		}
		writeJSON(w, http.StatusOK, apiEnvelope{Success: true, Code: http.StatusOK, Message: "Success", Data: items,
			Meta: map[string]any{"pagination": map[string]any{"current_page": page, "per_page": size, "total": total, "last_page": last}}})
	}))
}
