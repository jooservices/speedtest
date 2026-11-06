package server

import (
	"encoding/json"
	"net/http"

	"github.com/jooservices/speedtest/internal/center/auth"
)

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
		Success: true, Code: status, Message: message, Data: data, Meta: map[string]any{}, Errors: nil,
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
