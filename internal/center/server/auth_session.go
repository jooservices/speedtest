package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jooservices/speedtest/internal/center/auth"
)

// requireOwner applies the same origin, session and CSRF checks used by the
// owner authentication routes to session-protected node operations.
func (s *serverAuth) requireOwner(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		if !s.sameOrigin(r) {
			writeForbidden(w, r)
			return
		}
		_, session, ok := s.validSessionAndCSRF(w, r)
		if !ok {
			return
		}
		if session.Identity == nil || session.Identity.ID == "" {
			writeUnauthorized(w, r)
			return
		}
		ctx := context.WithValue(r.Context(), ownerIdentityContextKey{}, session.Identity.ID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireSession admits requests that carry a valid signed-in session cookie. It is meant for
// read-only (safe method) routes, so unlike requireOwner it needs no CSRF token or origin check.
func (s *serverAuth) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		token, present, invalid := readSessionCookie(r)
		if !present || invalid {
			if invalid {
				s.clearCookie(w, r)
			}
			writeUnauthorized(w, r)
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
		if session.Identity == nil || session.Identity.ID == "" {
			writeUnauthorized(w, r)
			return
		}
		ctx := context.WithValue(r.Context(), ownerIdentityContextKey{}, session.Identity.ID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
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

func (s *serverAuth) setCookie(w http.ResponseWriter, r *http.Request, session auth.SessionView) {
	setSessionCookieSecure(w, session, s.isHTTPS(r))
}

func (s *serverAuth) clearCookie(w http.ResponseWriter, r *http.Request) {
	clearSessionCookieSecure(w, s.isHTTPS(r))
}
