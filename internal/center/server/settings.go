package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jooservices/speedtest/internal/center/settings"
)

// maxSettingsBodyBytes bounds the PUT /api/settings request body.
const maxSettingsBodyBytes = 1 << 20

type ownerSettings struct {
	store   settings.Store
	limiter *nodeRateLimiter
}

// newOwnerSettingsHandler protects the settings API: reads need a session, writes also CSRF and origin checks.
func newOwnerSettingsHandler(store settings.Store, auth *serverAuth, perMinute int) http.Handler {
	if perMinute <= 0 {
		perMinute = defaultAgentRequestsPerMinute
	}
	h := &ownerSettings{store: store, limiter: newNodeRateLimiter(perMinute, defaultAgentBurst*2)}
	read := auth.requireSession(http.HandlerFunc(h.serve))
	write := auth.requireOwner(http.HandlerFunc(h.serve))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			read.ServeHTTP(w, r)
			return
		}
		write.ServeHTTP(w, r)
	})
}

func ownerSettingsRoute(path string) bool {
	return path == "/api/settings" || path == "/api/settings/enrolment-key"
}

func (h *ownerSettings) serve(w http.ResponseWriter, r *http.Request) {
	principal, err := ownerPrincipal(r)
	if err != nil {
		writeUnauthorized(w, r)
		return
	}
	if !allowRate(h.limiter, "owner:"+principal, w, r) {
		return
	}
	if r.URL.Path == "/api/settings/enrolment-key" {
		serveEnrolmentKey(w, r, h.store)
		return
	}
	serveSettings(w, r, h.store)
}

// serveSettings dispatches GET and PUT /api/settings.
func serveSettings(w http.ResponseWriter, r *http.Request, store settings.Store) {
	if store == nil {
		writeNotConfiguredProblem(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		handleGetSettings(w, r, store)
	case http.MethodPut:
		handlePutSettings(w, r, store)
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeMethodNotAllowedProblem(w, r)
	}
}

// serveEnrolmentKey dispatches POST /api/settings/enrolment-key.
func serveEnrolmentKey(w http.ResponseWriter, r *http.Request, store settings.Store) {
	if store == nil {
		writeNotConfiguredProblem(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeMethodNotAllowedProblem(w, r)
		return
	}
	handleRotateEnrolmentKey(w, r, store)
}

func handleGetSettings(w http.ResponseWriter, r *http.Request, store settings.Store) {
	view, etag, err := store.Get(r.Context())
	if err != nil {
		writeServerErrorProblem(w, r)
		return
	}
	w.Header().Set("ETag", etag)
	writeSuccess(w, r, http.StatusOK, "Settings retrieved", view)
}

func handlePutSettings(w http.ResponseWriter, r *http.Request, store settings.Store) {
	var input settings.SettingsInput
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSettingsBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeProblemResponse(w, http.StatusBadRequest, problemResponse{
			Type:    "/problems/badrequest",
			Title:   "Malformed request",
			Status:  http.StatusBadRequest,
			Detail:  "The request body is not valid JSON or contains unknown fields.",
			TraceID: requestIDFromContext(r.Context()),
		})
		return
	}
	if err := settings.Validate(input); err != nil {
		var ve *settings.ValidationError
		if errors.As(err, &ve) {
			writeProblemResponse(w, http.StatusUnprocessableEntity, problemResponse{
				Type:    "/problems/validation",
				Title:   "Field or cross-field validation failed",
				Status:  http.StatusUnprocessableEntity,
				Detail:  "One or more settings are invalid.",
				Errors:  ve.Fields,
				TraceID: requestIDFromContext(r.Context()),
			})
			return
		}
	}

	view, etag, err := store.Replace(r.Context(), r.Header.Get("If-Match"), input)
	if err != nil {
		switch {
		case errors.Is(err, settings.ErrPreconditionRequired):
			writePreconditionRequiredProblem(w, r)
		case errors.Is(err, settings.ErrStale):
			writeStaleProblem(w, r)
		default:
			writeServerErrorProblem(w, r)
		}
		return
	}
	w.Header().Set("ETag", etag)
	writeSuccess(w, r, http.StatusOK, "Settings updated", view)
}

func handleRotateEnrolmentKey(w http.ResponseWriter, r *http.Request, store settings.Store) {
	issued, err := store.RotateEnrollmentKey(r.Context())
	if err != nil {
		writeServerErrorProblem(w, r)
		return
	}
	writeSuccess(w, r, http.StatusCreated, "Enrolment key rotated", issued)
}

type successResponse struct {
	Success bool   `json:"success"`
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
	Meta    any    `json:"meta"`
	Errors  any    `json:"errors"`
}

// writeSuccess emits the shared JSON envelope with meta and errors set to null.
func writeSuccess(w http.ResponseWriter, r *http.Request, code int, message string, data any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set(requestIDHeader, requestIDFromContext(r.Context()))
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(successResponse{
		Success: true,
		Code:    code,
		Message: message,
		Data:    data,
		Meta:    nil,
		Errors:  nil,
	})
}

func writeServerErrorProblem(w http.ResponseWriter, r *http.Request) {
	const detail = "Internal failure; safe detail only."
	writeProblemResponse(w, http.StatusInternalServerError, problemResponse{
		Type:    "/problems/servererror",
		Title:   detail,
		Status:  http.StatusInternalServerError,
		Detail:  detail,
		TraceID: requestIDFromContext(r.Context()),
	})
}

func writeStaleProblem(w http.ResponseWriter, r *http.Request) {
	writeProblemResponse(w, http.StatusPreconditionFailed, problemResponse{
		Type:    "/problems/stale",
		Title:   "ETag does not match",
		Status:  http.StatusPreconditionFailed,
		Detail:  "The settings changed since they were loaded. Reload and retry.",
		TraceID: requestIDFromContext(r.Context()),
	})
}

func writePreconditionRequiredProblem(w http.ResponseWriter, r *http.Request) {
	writeProblemResponse(w, http.StatusPreconditionRequired, problemResponse{
		Type:    "/problems/preconditionrequired",
		Title:   "If-Match required",
		Status:  http.StatusPreconditionRequired,
		Detail:  "Provide the If-Match header from the latest GET.",
		TraceID: requestIDFromContext(r.Context()),
	})
}

func writeNotConfiguredProblem(w http.ResponseWriter, r *http.Request) {
	writeProblemResponse(w, http.StatusServiceUnavailable, problemResponse{
		Type:    "/problems/servererror",
		Title:   "Settings unavailable",
		Status:  http.StatusServiceUnavailable,
		Detail:  "Settings are not configured on this command center.",
		TraceID: requestIDFromContext(r.Context()),
	})
}
