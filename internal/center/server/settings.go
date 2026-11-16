package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/jooservices/speedtest/internal/center/settings"
)

// maxSettingsBodyBytes bounds the PUT and PATCH /api/settings request body.
const maxSettingsBodyBytes = 1 << 20

type ownerSettings struct {
	store   settings.Store
	limiter *nodeRateLimiter
}

// newOwnerSettingsHandler protects the settings API: reads need a session, writes also CSRF and origin checks.
func newOwnerSettingsHandler(store settings.Store, auth *serverAuth) http.Handler {
	h := &ownerSettings{store: store, limiter: newOwnerRateLimiter()}
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

// serveSettings dispatches GET, PUT and PATCH /api/settings.
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
	case http.MethodPatch:
		handlePatchSettings(w, r, store)
	default:
		w.Header().Set("Allow", "GET, PUT, PATCH")
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

// handlePutSettings replaces the preferences. A PUT that omits language keeps the saved language instead of
// resetting it, so clients written before the language setting keep working.
func handlePutSettings(w http.ResponseWriter, r *http.Request, store settings.Store) {
	body, ok := readSettingsBody(w, r)
	if !ok {
		return
	}
	var input settings.SettingsInput
	if err := decodeSettingsInput(body, &input); err != nil {
		writeMalformedSettingsProblem(w, r)
		return
	}
	var presence struct {
		Language json.RawMessage `json:"language"`
	}
	if err := json.Unmarshal(body, &presence); err != nil {
		writeMalformedSettingsProblem(w, r)
		return
	}
	if len(presence.Language) == 0 {
		current, _, err := store.Get(r.Context())
		if err != nil {
			writeServerErrorProblem(w, r)
			return
		}
		input.Language = current.Language
	}
	saveSettings(w, r, store, input)
}

// handlePatchSettings applies an RFC 7396 JSON merge patch to the saved preferences. Fields the body omits keep
// their saved value; every field it sends is validated exactly as PUT validates it. A null is rejected because each
// setting has a value, so a null cannot delete one. Like PUT, it needs the If-Match tag from the latest GET.
func handlePatchSettings(w http.ResponseWriter, r *http.Request, store settings.Store) {
	ifMatch := r.Header.Get("If-Match")
	if ifMatch == "" {
		writePreconditionRequiredProblem(w, r)
		return
	}
	body, ok := readSettingsBody(w, r)
	if !ok {
		return
	}
	var patch map[string]any
	if err := json.Unmarshal(body, &patch); err != nil || patch == nil {
		writeMalformedSettingsProblem(w, r)
		return
	}
	current, etag, err := store.Get(r.Context())
	if err != nil {
		writeServerErrorProblem(w, r)
		return
	}
	// The patch is merged into the view read here, so the tag must still be the one the client sent.
	if ifMatch != etag {
		writeStaleProblem(w, r)
		return
	}
	raw, err := json.Marshal(settings.InputOf(current))
	if err != nil {
		writeServerErrorProblem(w, r)
		return
	}
	var merged map[string]any
	if err := json.Unmarshal(raw, &merged); err != nil {
		writeServerErrorProblem(w, r)
		return
	}
	nulls := map[string][]string{}
	mergeSettingsPatch(merged, patch, "", nulls)
	if len(nulls) > 0 {
		writeValidationProblem(w, r, nulls)
		return
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		writeServerErrorProblem(w, r)
		return
	}
	var input settings.SettingsInput
	if err := decodeSettingsInput(encoded, &input); err != nil {
		writeMalformedSettingsProblem(w, r)
		return
	}
	saveSettings(w, r, store, input)
}

// mergeSettingsPatch merges patch into base as RFC 7396 does for objects, except that a null is recorded as a
// validation error under its dotted field path instead of deleting the field.
func mergeSettingsPatch(base, patch map[string]any, path string, nulls map[string][]string) {
	for key, value := range patch {
		field := key
		if path != "" {
			field = path + "." + key
		}
		if value == nil {
			nulls[field] = append(nulls[field], "must not be null")
			continue
		}
		object, isObject := value.(map[string]any)
		if !isObject {
			base[key] = value
			continue
		}
		current, ok := base[key].(map[string]any)
		if !ok {
			current = map[string]any{}
			base[key] = current
		}
		mergeSettingsPatch(current, object, field, nulls)
	}
}

// readSettingsBody reads the bounded request body; on failure it answers 400 and reports false.
func readSettingsBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSettingsBodyBytes))
	if err != nil {
		writeMalformedSettingsProblem(w, r)
		return nil, false
	}
	return body, true
}

// decodeSettingsInput decodes a settings body and rejects unknown fields.
func decodeSettingsInput(body []byte, input *settings.SettingsInput) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	return decoder.Decode(input)
}

// saveSettings validates the complete input and stores it under the If-Match precondition of the request.
func saveSettings(w http.ResponseWriter, r *http.Request, store settings.Store, input settings.SettingsInput) {
	if err := settings.Validate(input); err != nil {
		var ve *settings.ValidationError
		if errors.As(err, &ve) {
			writeValidationProblem(w, r, ve.Fields)
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

func writeMalformedSettingsProblem(w http.ResponseWriter, r *http.Request) {
	writeProblemResponse(w, http.StatusBadRequest, problemResponse{
		Type:    "/problems/badrequest",
		Title:   "Malformed request",
		Status:  http.StatusBadRequest,
		Detail:  "The request body is not valid JSON or contains unknown fields.",
		TraceID: requestIDFromContext(r.Context()),
	})
}

func writeValidationProblem(w http.ResponseWriter, r *http.Request, fields map[string][]string) {
	writeProblemResponse(w, http.StatusUnprocessableEntity, problemResponse{
		Type:    "/problems/validation",
		Title:   "Field or cross-field validation failed",
		Status:  http.StatusUnprocessableEntity,
		Detail:  "One or more settings are invalid.",
		Errors:  fields,
		TraceID: requestIDFromContext(r.Context()),
	})
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
		Meta:    map[string]any{},
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
