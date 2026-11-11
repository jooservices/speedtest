package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/jooservices/speedtest/internal/center/apitoken"
	"github.com/jooservices/speedtest/internal/center/audit"
)

// AuditStore records owner changes and lists them.
type AuditStore interface {
	Record(ctx context.Context, e audit.Entry) error
	List(ctx context.Context, f audit.Filter, page, size int) ([]audit.Entry, int, error)
}

type auditRecordedKey struct{}
type auditRecordedState struct{ recorded atomic.Bool }

func markAuditRecorded(ctx context.Context) {
	if state, ok := ctx.Value(auditRecordedKey{}).(*auditRecordedState); ok {
		state.recorded.Store(true)
	}
}

type auditMetadataContextKey struct{}

type auditMetadata struct {
	targetID string
}

func setAuditTargetID(ctx context.Context, targetID string) {
	metadata, _ := ctx.Value(auditMetadataContextKey{}).(*auditMetadata)
	if metadata != nil {
		metadata.targetID = targetID
	}
}

// withAudit records every successful owner write (see audit.Classify). The actor is read from the session before
// the request runs, so a password change or a sign-out is attributed to the session that made it. Recording is
// best effort: a failure is logged and never changes the response.
func withAudit(next http.Handler, store AuditStore, auth *serverAuth, events NotificationEvents, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (store == nil && events == nil) || r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		entry, ok := audit.Classify(r.Method, r.URL.Path)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		actor := auth.actor(r)
		state := &auditRecordedState{}
		metadata := &auditMetadata{}
		ctx := context.WithValue(r.Context(), auditRecordedKey{}, state)
		ctx = context.WithValue(ctx, auditMetadataContextKey{}, metadata)
		r = r.WithContext(ctx)
		response := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(response, r)
		status := response.status
		if status == 0 {
			status = http.StatusOK
		}
		if status < 200 || status > 299 || actor == "" {
			return
		}
		if store != nil && !state.recorded.Load() {
			entry.Actor = actor
			entry.AuthMethod = audit.AuthMethodSession
			if principal, ok := apitoken.PrincipalFromContext(r.Context()); ok {
				entry.AuthMethod = audit.AuthMethodBearer
				entry.CredentialID = principal.TokenID
			}
			if metadata.targetID != "" {
				entry.TargetID = metadata.targetID
			}
			entry.Remote = remoteIP(r.RemoteAddr, r.Header.Get("X-Forwarded-For"), auth.trustedProxyCIDRs)
			if err := store.Record(context.WithoutCancel(r.Context()), entry); err != nil && logger != nil {
				logger.ErrorContext(r.Context(), "audit entry not recorded", slog.String("action", entry.Action), slog.String("request_id", requestIDFromContext(r.Context())))
			}
		}
		publishAuditNotification(r, events, entry)
	})
}

func publishAuditNotification(r *http.Request, events NotificationEvents, entry audit.Entry) {
	if events == nil {
		return
	}
	key, value := auditNotification(entry.Action)
	if key == "" {
		return
	}
	payload := map[string]string{}
	if value != "" {
		payload["value"] = value
	}
	_ = events.PublishEvent(context.WithoutCancel(r.Context()), key, "", payload)
}

func auditNotification(action string) (string, string) {
	switch action {
	case "auth.password":
		return "security.password_changed", ""
	case "access_token.create":
		return "security.api_token_created", ""
	case "auth.two_factor_enable":
		return "security.2fa_changed", "enabled"
	case "auth.two_factor_disable":
		return "security.2fa_changed", "disabled"
	default:
		return "", ""
	}
}

// actor returns the signed-in owner's e-mail for the request's session, or "" without a valid session.
func (s *serverAuth) actor(r *http.Request) string {
	if principal, ok := apitoken.PrincipalFromContext(r.Context()); ok {
		if principal.Email != "" {
			return principal.Email
		}
		return principal.UserID
	}
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
	return ownerReader(auth, newOwnerRateLimiter(), func(w http.ResponseWriter, r *http.Request) {
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
		writePage(w, items, page, size, total)
	})
}
