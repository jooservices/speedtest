package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/jooservices/speedtest/internal/center/auth"
)

// TwoFactorService is the optional second factor of owner sign-in (authenticator app and recovery codes).
type TwoFactorService interface {
	TwoFactorStatus(context.Context, string) (auth.TwoFactorStatus, error)
	BeginTwoFactor(context.Context, string, string) (auth.TwoFactorSetup, error)
	EnableTwoFactor(context.Context, string, string) ([]string, error)
	DisableTwoFactor(context.Context, string, string, string) error
}

type twoFactorStatusResponse struct {
	Available         bool `json:"available"`
	Enabled           bool `json:"enabled"`
	RecoveryRemaining int  `json:"recoveryRemaining"`
}

type twoFactorSetupResponse struct {
	Secret string `json:"secret"`
	URI    string `json:"uri"`
	QR     string `json:"qr"`
}

type twoFactorRequest struct {
	CurrentPassword string `json:"currentPassword,omitempty"`
	Code            string `json:"code,omitempty"`
}

// twoFactor handles /api/auth/2fa (GET status) and the setup, enable and disable actions (POST). Writes need the
// same-origin check, the session cookie and its CSRF token, like every other owner write.
func (s *serverAuth) twoFactor(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/auth/2fa" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeMethodNotAllowedProblem(w, r)
			return
		}
		s.twoFactorStatus(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeMethodNotAllowedProblem(w, r)
		return
	}
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
	var input twoFactorRequest
	if !decodeAuthJSON(w, r, &input) {
		return
	}
	if len(input.CurrentPassword) > 1024 || len(input.Code) > 32 {
		writeValidation(w, r, "Password or code is invalid.")
		return
	}
	switch r.URL.Path {
	case "/api/auth/2fa/setup":
		setup, err := s.service.BeginTwoFactor(r.Context(), token, input.CurrentPassword)
		if s.writeTwoFactorFailure(w, r, err) {
			return
		}
		writeAuthSuccess(w, r, http.StatusCreated, "Scan the code with your authenticator app, then confirm with a code.",
			twoFactorSetupResponse{Secret: setup.Secret, URI: setup.URI, QR: setup.QR})
	case "/api/auth/2fa/enable":
		codes, err := s.service.EnableTwoFactor(r.Context(), token, input.Code)
		if s.writeTwoFactorFailure(w, r, err) {
			return
		}
		writeAuthSuccess(w, r, http.StatusOK, "Two-factor sign-in is on. Keep the recovery codes: they are shown only once.", map[string]any{"recoveryCodes": codes})
	default:
		err := s.service.DisableTwoFactor(r.Context(), token, input.CurrentPassword, input.Code)
		if s.writeTwoFactorFailure(w, r, err) {
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *serverAuth) twoFactorStatus(w http.ResponseWriter, r *http.Request) {
	token, present, invalid := readSessionCookie(r)
	if !present || invalid {
		if invalid {
			s.clearCookie(w, r)
		}
		writeUnauthorized(w, r)
		return
	}
	status, err := s.service.TwoFactorStatus(r.Context(), token)
	if s.writeTwoFactorFailure(w, r, err) {
		return
	}
	writeAuthSuccess(w, r, http.StatusOK, "Success", twoFactorStatusResponse{Available: status.Available, Enabled: status.Enabled, RecoveryRemaining: status.RecoveryRemaining})
}

// writeTwoFactorFailure maps service errors to problems; it reports whether it wrote a response.
func (s *serverAuth) writeTwoFactorFailure(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, auth.ErrInvalidSession):
		s.clearCookie(w, r)
		writeUnauthorized(w, r)
	case errors.Is(err, auth.ErrInvalidCurrentPass):
		writeProblemResponse(w, http.StatusUnauthorized, problemResponse{
			Type: "/problems/unauthorized", Title: "Current password is incorrect", Status: http.StatusUnauthorized,
			Detail: "Current password is incorrect.", TraceID: requestIDFromContext(r.Context()),
		})
	case errors.Is(err, auth.ErrRateLimited):
		writeRateLimited(w, r, err)
	case writeTwoFactorProblem(w, r, err):
	default:
		writeAuthInternalError(w, r)
	}
	return true
}

// writeTwoFactorProblem writes the second-factor problems shared by sign-in and the settings actions.
func writeTwoFactorProblem(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case errors.Is(err, auth.ErrTwoFactorRequired):
		w.Header().Set("WWW-Authenticate", "Session")
		writeProblemResponse(w, http.StatusUnauthorized, problemResponse{
			Type: "/problems/two-factor-required", Title: "Authentication code required", Status: http.StatusUnauthorized,
			Detail: "Enter the code from your authenticator app or a recovery code.", TraceID: requestIDFromContext(r.Context()),
		})
	case errors.Is(err, auth.ErrInvalidTwoFactorCode):
		w.Header().Set("WWW-Authenticate", "Session")
		writeProblemResponse(w, http.StatusUnauthorized, problemResponse{
			Type: "/problems/unauthorized", Title: "Invalid authentication code", Status: http.StatusUnauthorized,
			Detail: "The authentication code is invalid or was already used.", TraceID: requestIDFromContext(r.Context()),
		})
	case errors.Is(err, auth.ErrTwoFactorUnavailable):
		writeProblemResponse(w, http.StatusServiceUnavailable, problemResponse{
			Type: "/problems/servererror", Title: "Two-factor sign-in unavailable", Status: http.StatusServiceUnavailable,
			Detail: "Two-factor sign-in needs APP_KEY to be configured.", TraceID: requestIDFromContext(r.Context()),
		})
	case errors.Is(err, auth.ErrTwoFactorEnabled), errors.Is(err, auth.ErrTwoFactorNotPending), errors.Is(err, auth.ErrTwoFactorNotEnabled):
		writeProblemResponse(w, http.StatusConflict, problemResponse{
			Type: "/problems/conflict", Title: "Two-factor state conflict", Status: http.StatusConflict,
			Detail: err.Error() + ".", TraceID: requestIDFromContext(r.Context()),
		})
	default:
		return false
	}
	return true
}
