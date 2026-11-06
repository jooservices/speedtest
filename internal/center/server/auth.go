package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/jooservices/speedtest/internal/center/auth"
)

const (
	sessionCookieName = "speedtest_session"
	maxAuthBodyBytes  = 8 * 1024
)

type authSuccessResponse struct {
	Success bool   `json:"success"`
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
	Meta    any    `json:"meta"`
	Errors  any    `json:"errors"`
	TraceID string `json:"trace_id,omitempty"`
}

type authSessionResponse struct {
	User      *authUserResponse `json:"user"`
	CSRFToken string            `json:"csrfToken"`
	ExpiresAt time.Time         `json:"expiresAt"`
}

type authUserResponse struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type passwordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

type retryAfterError interface {
	RetryAfter() time.Duration
}

type ownerIdentityContextKey struct{}

// serveAuth dispatches the four owner auth operations. All mutating methods
// validate same-origin requests, the session cookie and its CSRF token.
func (s *serverAuth) serve(w http.ResponseWriter, r *http.Request) {
	if s.service == nil {
		writeProblemResponse(w, http.StatusServiceUnavailable, problemResponse{
			Type: "/problems/servererror", Title: "Service unavailable", Status: http.StatusServiceUnavailable,
			Detail: "Authentication service is not configured.", TraceID: requestIDFromContext(r.Context()),
		})
		return
	}
	if s.requireHTTPS && !s.isHTTPS(r) {
		writeProblemResponse(w, http.StatusForbidden, problemResponse{
			Type: "/problems/forbidden", Title: "HTTPS required", Status: http.StatusForbidden,
			Detail: "HTTPS is required outside local and test environments.", TraceID: requestIDFromContext(r.Context()),
		})
		return
	}
	switch r.URL.Path {
	case "/api/auth/me":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeMethodNotAllowedProblem(w, r)
			return
		}
		s.me(w, r)
	case "/api/auth/login":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeMethodNotAllowedProblem(w, r)
			return
		}
		s.login(w, r)
	case "/api/auth/logout":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeMethodNotAllowedProblem(w, r)
			return
		}
		s.logout(w, r)
	case "/api/auth/password":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeMethodNotAllowedProblem(w, r)
			return
		}
		s.changePassword(w, r)
	default:
		writeNotFoundProblem(w, r)
	}
}

type serverAuth struct {
	service           AuthService
	requireHTTPS      bool
	trustedProxyCIDRs string
}

// AuthService is the session and owner authentication contract used by the server.
type AuthService interface {
	Bootstrap(context.Context) (auth.SessionView, error)
	Session(context.Context, string) (auth.SessionView, error)
	Login(context.Context, string, string, string, string) (auth.SessionView, error)
	Logout(context.Context, string) error
	ChangePassword(context.Context, string, string, string) (auth.SessionView, error)
}

func (s *serverAuth) me(w http.ResponseWriter, r *http.Request) {
	token, present, invalid := readSessionCookie(r)
	if invalid {
		s.clearCookie(w, r)
		writeUnauthorized(w, r)
		return
	}
	if !present {
		session, err := s.service.Bootstrap(r.Context())
		if err != nil {
			writeAuthInternalError(w, r)
			return
		}
		s.setCookie(w, r, session)
		writeAuthSuccess(w, r, http.StatusOK, "Session ready.", sessionResponse(session))
		return
	}
	session, err := s.service.Session(r.Context(), token)
	if errors.Is(err, auth.ErrInvalidSession) {
		s.clearCookie(w, r)
		writeUnauthorized(w, r)
		return
	}
	if err != nil {
		writeAuthInternalError(w, r)
		return
	}
	writeAuthSuccess(w, r, http.StatusOK, "Session ready.", sessionResponse(session))
}

func (s *serverAuth) login(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		writeForbidden(w, r)
		return
	}
	token, session, ok := s.validSessionAndCSRF(w, r)
	if !ok {
		return
	}
	if session.Identity != nil {
		writeConflict(w, r)
		return
	}
	var input loginRequest
	if !decodeAuthJSON(w, r, &input) {
		return
	}
	if !auth.ValidEmailForAPI(input.Email) || len(input.Password) == 0 || len(input.Password) > 1024 {
		writeValidation(w, r, "Email or password is invalid.")
		return
	}
	clientIP := remoteIP(r.RemoteAddr, r.Header.Get("X-Forwarded-For"), s.trustedProxyCIDRs)
	created, err := s.service.Login(r.Context(), token, input.Email, input.Password, clientIP)
	if errors.Is(err, auth.ErrInvalidSession) {
		s.clearCookie(w, r)
		writeUnauthorized(w, r)
		return
	}
	if errors.Is(err, auth.ErrInvalidCredentials) {
		w.Header().Set("WWW-Authenticate", "Session")
		writeProblemResponse(w, http.StatusUnauthorized, problemResponse{
			Type: "/problems/unauthorized", Title: "Invalid email or password", Status: http.StatusUnauthorized,
			Detail: "Invalid email or password.", TraceID: requestIDFromContext(r.Context()),
		})
		return
	}
	if errors.Is(err, auth.ErrRateLimited) {
		var limited retryAfterError
		retrySeconds := 1
		if errors.As(err, &limited) {
			retrySeconds = max(1, int((limited.RetryAfter()+time.Second-1)/time.Second))
		}
		w.Header().Set("Retry-After", strconv.Itoa(retrySeconds))
		writeProblemResponse(w, http.StatusTooManyRequests, problemResponse{
			Type: "/problems/ratelimited", Title: "Too many login attempts", Status: http.StatusTooManyRequests,
			Detail: "Too many login attempts. Try again later.", TraceID: requestIDFromContext(r.Context()),
		})
		return
	}
	if errors.Is(err, auth.ErrSessionConflict) {
		writeConflict(w, r)
		return
	}
	if err != nil {
		writeAuthInternalError(w, r)
		return
	}
	s.setCookie(w, r, created)
	writeAuthSuccess(w, r, http.StatusOK, "Signed in.", sessionResponse(created))
}

func (s *serverAuth) logout(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		writeForbidden(w, r)
		return
	}
	token, _, ok := s.validSessionAndCSRF(w, r)
	if !ok {
		return
	}
	if err := s.service.Logout(r.Context(), token); errors.Is(err, auth.ErrInvalidSession) {
		s.clearCookie(w, r)
		writeUnauthorized(w, r)
		return
	} else if err != nil {
		writeAuthInternalError(w, r)
		return
	}
	s.clearCookie(w, r)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (s *serverAuth) changePassword(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		writeForbidden(w, r)
		return
	}
	token, session, ok := s.validSessionAndCSRF(w, r)
	if !ok {
		return
	}
	if session.Identity == nil {
		writeUnauthorized(w, r)
		return
	}
	var input passwordRequest
	if !decodeAuthJSON(w, r, &input) {
		return
	}
	if len(input.CurrentPassword) == 0 || len(input.CurrentPassword) > 1024 || len(input.NewPassword) == 0 || len(input.NewPassword) > 1024 {
		writeValidation(w, r, "Current and new passwords are required.")
		return
	}
	rotated, err := s.service.ChangePassword(r.Context(), token, input.CurrentPassword, input.NewPassword)
	if errors.Is(err, auth.ErrInvalidSession) {
		s.clearCookie(w, r)
		writeUnauthorized(w, r)
		return
	}
	if errors.Is(err, auth.ErrInvalidCurrentPass) {
		writeProblemResponse(w, http.StatusUnauthorized, problemResponse{
			Type: "/problems/unauthorized", Title: "Current password is incorrect", Status: http.StatusUnauthorized,
			Detail: "Current password is incorrect.", TraceID: requestIDFromContext(r.Context()),
		})
		return
	}
	if errors.Is(err, auth.ErrPasswordPolicy) {
		writeValidation(w, r, "New password must contain at least 12 characters.")
		return
	}
	if err != nil {
		writeAuthInternalError(w, r)
		return
	}
	s.setCookie(w, r, rotated)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func decodeAuthJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxAuthBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeProblemResponse(w, http.StatusRequestEntityTooLarge, problemResponse{
				Type: "/problems/toolarge", Title: "Request body too large", Status: http.StatusRequestEntityTooLarge,
				Detail: "Request body is too large.", TraceID: requestIDFromContext(r.Context()),
			})
			return false
		}
		writeProblemResponse(w, http.StatusBadRequest, problemResponse{
			Type: "/problems/badrequest", Title: "Malformed request", Status: http.StatusBadRequest,
			Detail: "Malformed request.", TraceID: requestIDFromContext(r.Context()),
		})
		return false
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		writeProblemResponse(w, http.StatusBadRequest, problemResponse{
			Type: "/problems/badrequest", Title: "Malformed request", Status: http.StatusBadRequest,
			Detail: "Malformed request.", TraceID: requestIDFromContext(r.Context()),
		})
		return false
	}
	return true
}
