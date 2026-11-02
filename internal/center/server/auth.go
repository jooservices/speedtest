package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
	Meta    any    `json:"meta,omitempty"`
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
	service           authService
	requireHTTPS      bool
	trustedProxyCIDRs string
}

type authService interface {
	Bootstrap(context.Context) (auth.SessionView, error)
	Session(context.Context, string) (auth.SessionView, error)
	Login(context.Context, string, string, string, string) (auth.SessionView, error)
	Logout(context.Context, string) error
	ChangePassword(context.Context, string, string, string) (auth.SessionView, error)
}

func (s *serverAuth) isHTTPS(r *http.Request) bool {
	return requestIsHTTPS(r, s.trustedProxyCIDRs)
}

func (s *serverAuth) sameOrigin(r *http.Request) bool {
	return sameOriginWithScheme(r, s.isHTTPS(r))
}

func (s *serverAuth) setCookie(w http.ResponseWriter, r *http.Request, session auth.SessionView) {
	setSessionCookieSecure(w, session, s.isHTTPS(r))
}

func (s *serverAuth) clearCookie(w http.ResponseWriter, r *http.Request) {
	clearSessionCookieSecure(w, s.isHTTPS(r))
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

func (s *serverAuth) validSessionAndCSRF(w http.ResponseWriter, r *http.Request) (string, auth.SessionView, bool) {
	token, present, invalid := readSessionCookie(r)
	if !present || invalid {
		if invalid {
			s.clearCookie(w, r)
		}
		writeUnauthorized(w, r)
		return "", auth.SessionView{}, false
	}
	session, err := s.service.Session(r.Context(), token)
	if errors.Is(err, auth.ErrInvalidSession) {
		s.clearCookie(w, r)
		writeUnauthorized(w, r)
		return "", auth.SessionView{}, false
	}
	if err != nil {
		writeAuthInternalError(w, r)
		return "", auth.SessionView{}, false
	}
	csrf := r.Header.Get("X-CSRF-Token")
	if csrf == "" || !auth.MatchCSRF(csrf, session.CSRFToken) {
		writeForbidden(w, r)
		return "", auth.SessionView{}, false
	}
	return token, session, true
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

func readSessionCookie(r *http.Request) (token string, present, invalid bool) {
	var found []*http.Cookie
	for _, cookie := range r.Cookies() {
		if cookie.Name == sessionCookieName {
			found = append(found, cookie)
		}
	}
	if len(found) == 0 {
		return "", false, false
	}
	if len(found) != 1 || found[0].Value == "" {
		return "", true, true
	}
	return found[0].Value, true, false
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, session auth.SessionView) {
	setSessionCookieSecure(w, session, requestIsHTTPS(r, ""))
}

func setSessionCookieSecure(w http.ResponseWriter, session auth.SessionView, secure bool) {
	maxAge := int(time.Until(session.ExpiresAt).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: session.Token, Path: "/", Expires: session.ExpiresAt.UTC(), MaxAge: maxAge,
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
}

func clearSessionCookieSecure(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/", Expires: time.Unix(1, 0).UTC(), MaxAge: -1,
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
}

func requestIsHTTPS(r *http.Request, trustedProxyCIDRs string) bool {
	if r.TLS != nil {
		return true
	}
	peer := net.ParseIP(remoteIP(r.RemoteAddr, "", ""))
	if peer == nil || !containsIP(trustedProxyNetworks(trustedProxyCIDRs), peer) {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

func sameOrigin(r *http.Request) bool {
	return sameOriginWithScheme(r, requestIsHTTPS(r, ""))
}

func sameOriginWithScheme(r *http.Request, isHTTPS bool) bool {
	origins := r.Header.Values("Origin")
	if len(origins) != 1 {
		return false
	}
	origin, err := url.Parse(origins[0])
	if err != nil || origin.Opaque != "" || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || (origin.Path != "" && origin.Path != "/") {
		return false
	}
	scheme := "http"
	if isHTTPS {
		scheme = "https"
	}
	if !strings.EqualFold(origin.Scheme, scheme) {
		return false
	}
	originAuthority, ok := canonicalAuthority(origin.Host, scheme)
	if !ok {
		return false
	}
	requestAuthority, ok := canonicalAuthority(r.Host, scheme)
	return ok && originAuthority == requestAuthority
}

func canonicalAuthority(host, scheme string) (string, bool) {
	parsed, err := url.Parse("//" + host)
	if err != nil || parsed.Host == "" || parsed.Path != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", false
	}
	name := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if name == "" {
		return "", false
	}
	port := parsed.Port()
	if port == "" {
		if strings.Contains(parsed.Host, ":") && net.ParseIP(name) == nil {
			return "", false
		}
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return "", false
	}
	if scheme == "https" && port == "443" || scheme == "http" && port == "80" {
		return name, true
	}
	return net.JoinHostPort(name, port), true
}

func remoteIP(remoteAddr, forwardedFor, trustedProxyCIDRs string) string {
	peer := remoteAddr
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		peer = host
	}
	peerIP := net.ParseIP(peer)
	if peerIP == nil {
		return peer
	}
	networks := trustedProxyNetworks(trustedProxyCIDRs)
	if !containsIP(networks, peerIP) || strings.TrimSpace(forwardedFor) == "" {
		return peerIP.String()
	}
	chain := strings.Split(forwardedFor, ",")
	chain = append(chain, peerIP.String())
	for index := len(chain) - 1; index >= 0; index-- {
		current := net.ParseIP(strings.TrimSpace(chain[index]))
		if current == nil {
			return peerIP.String()
		}
		if containsIP(networks, current) {
			continue
		}
		return current.String()
	}
	return peerIP.String()
}

func trustedProxyNetworks(raw string) []*net.IPNet {
	var networks []*net.IPNet
	for _, item := range strings.Split(raw, ",") {
		_, network, err := net.ParseCIDR(strings.TrimSpace(item))
		if err == nil && network != nil {
			networks = append(networks, network)
		}
	}
	return networks
}

func containsIP(networks []*net.IPNet, ip net.IP) bool {
	for _, network := range networks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func sessionResponse(session auth.SessionView) authSessionResponse {
	var user *authUserResponse
	if session.Identity != nil {
		user = &authUserResponse{ID: session.Identity.ID, Email: session.Identity.Email}
	}
	return authSessionResponse{User: user, CSRFToken: session.CSRFToken, ExpiresAt: session.ExpiresAt.UTC()}
}

func writeAuthSuccess(w http.ResponseWriter, r *http.Request, status int, message string, data any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(authSuccessResponse{
		Success: true, Code: status, Message: message, Data: data, Errors: nil,
		TraceID: requestIDFromContext(r.Context()),
	})
}

func writeUnauthorized(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("WWW-Authenticate", "Session")
	writeProblemResponse(w, http.StatusUnauthorized, problemResponse{
		Type: "/problems/unauthorized", Title: "Missing, expired, revoked or invalid credential",
		Status: http.StatusUnauthorized, Detail: "Missing, expired, revoked or invalid credential.",
		TraceID: requestIDFromContext(r.Context()),
	})
}

func writeForbidden(w http.ResponseWriter, r *http.Request) {
	writeProblemResponse(w, http.StatusForbidden, problemResponse{
		Type: "/problems/forbidden", Title: "Not authorized or CSRF/Origin validation failed",
		Status: http.StatusForbidden, Detail: "Not authorized or CSRF/Origin validation failed.",
		TraceID: requestIDFromContext(r.Context()),
	})
}

func writeConflict(w http.ResponseWriter, r *http.Request) {
	writeProblemResponse(w, http.StatusConflict, problemResponse{
		Type: "/problems/conflict", Title: "Session state conflict", Status: http.StatusConflict,
		Detail:  "The requested session state conflicts with the current session.",
		TraceID: requestIDFromContext(r.Context()),
	})
}

func writeValidation(w http.ResponseWriter, r *http.Request, detail string) {
	writeProblemResponse(w, http.StatusUnprocessableEntity, problemResponse{
		Type: "/problems/validation", Title: "Validation failed", Status: http.StatusUnprocessableEntity,
		Detail: detail, TraceID: requestIDFromContext(r.Context()),
	})
}

func writeAuthInternalError(w http.ResponseWriter, r *http.Request) {
	writeProblemResponse(w, http.StatusInternalServerError, problemResponse{
		Type: "/problems/servererror", Title: "Internal server error", Status: http.StatusInternalServerError,
		Detail: "The request could not be completed.", TraceID: requestIDFromContext(r.Context()),
	})
}
