package server

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"

	"github.com/jooservices/speedtest/internal/center/nodes"
)

func ownerPrincipal(r *http.Request) (string, error) {
	principal, ok := r.Context().Value(ownerIdentityContextKey{}).(string)
	if !ok || principal == "" {
		return "", nodes.ErrUnauthorized
	}
	return principal, nil
}

func bearerValue(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || len(parts[1]) > 8192 {
		return "", false
	}
	return parts[1], true
}

func idempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	values := r.Header.Values("Idempotency-Key")
	if len(values) != 1 || len(values[0]) == 0 || len(values[0]) > 128 {
		writeAPIProblem(w, r, http.StatusBadRequest, "Bad request", "A valid Idempotency-Key header is required.")
		return "", false
	}
	return values[0], true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxNodeRequestBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeAPIProblem(w, r, http.StatusRequestEntityTooLarge, "Request too large", "The request body exceeds the allowed size.")
		} else {
			writeAPIProblem(w, r, http.StatusBadRequest, "Bad request", "The JSON request body is invalid.")
		}
		return false
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		writeAPIProblem(w, r, http.StatusBadRequest, "Bad request", "The request body must contain one JSON value.")
		return false
	}
	return true
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func setLocation(w http.ResponseWriter, data json.RawMessage, prefix string) {
	var decoded struct {
		Node struct {
			ID string `json:"id"`
		} `json:"node"`
	}
	if json.Unmarshal(data, &decoded) == nil && decoded.Node.ID != "" {
		w.Header().Set("Location", prefix+decoded.Node.ID)
	}
}

func methodNotAllowed(w http.ResponseWriter, r *http.Request, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	writeMethodNotAllowedProblem(w, r)
}

func writeEnvelope(w http.ResponseWriter, status int, message string, data any) {
	var payload = data
	if raw, ok := data.(json.RawMessage); ok {
		payload = json.RawMessage(raw)
	}
	writeJSON(w, status, apiEnvelope{Success: true, Code: status, Message: message, Data: payload, Meta: map[string]any{}, Errors: nil})
}

func writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, nodes.ErrUnauthorized), errors.Is(err, nodes.ErrInvalidAuth), errors.Is(err, nodes.ErrInvalidToken):
		writeAPIProblem(w, r, http.StatusUnauthorized, "Unauthorized", "Authentication is required or invalid.")
	case errors.Is(err, nodes.ErrForbidden):
		writeAPIProblem(w, r, http.StatusForbidden, "Forbidden", "The request failed authorization checks.")
	case errors.Is(err, nodes.ErrNotFound):
		writeNotFoundProblem(w, r)
	case errors.Is(err, nodes.ErrConflict):
		writeAPIProblem(w, r, http.StatusConflict, "Conflict", "The request conflicts with current resource state.")
	case errors.Is(err, nodes.ErrInvalidInput):
		writeAPIProblem(w, r, http.StatusUnprocessableEntity, "Validation failed", "The request configuration is invalid.")
	case errors.Is(err, nodes.ErrCryptoKeyUnset):
		writeAPIProblem(w, r, http.StatusServiceUnavailable, "Service unavailable", "Secure issuance is not configured.")
	default:
		writeAPIProblem(w, r, http.StatusInternalServerError, "Internal failure", "Internal failure; safe detail only.")
	}
}

func writeAPIProblem(w http.ResponseWriter, r *http.Request, status int, title, detail string) {
	types := map[int]string{http.StatusBadRequest: "/problems/badrequest", http.StatusUnauthorized: "/problems/unauthorized", http.StatusForbidden: "/problems/forbidden", http.StatusNotFound: "/problems/notfound", http.StatusConflict: "/problems/conflict", http.StatusRequestEntityTooLarge: "/problems/toolarge", http.StatusUnprocessableEntity: "/problems/validation", http.StatusTooManyRequests: "/problems/ratelimited", http.StatusServiceUnavailable: "/problems/servererror", http.StatusInternalServerError: "/problems/servererror"}
	writeProblemResponse(w, status, problemResponse{Type: types[status], Title: title, Status: status, Detail: detail, TraceID: requestIDFromContext(r.Context())})
}

// writePage answers a list request: the items and the standard pagination block.
func writePage(w http.ResponseWriter, items any, page, size, total int) {
	last := (total + size - 1) / size
	if last < 1 {
		last = 1
	}
	writeJSON(w, http.StatusOK, apiEnvelope{Success: true, Code: http.StatusOK, Message: "Success", Data: items,
		Meta: map[string]any{"pagination": map[string]any{"current_page": page, "per_page": size, "total": total, "last_page": last}}})
}

var idSegment = regexp.MustCompile(`^(?:[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}|\d+)$`)

// routeWords are the fixed words that appear in this API's paths; any other segment is shown as *.
var routeWords = map[string]bool{
	"api": true, "auth": true, "agent": true, "nodes": true, "schedules": true, "alerts": true, "settings": true, "notifications": true,
	"ip": true, "stats": true, "results": true, "export": true, "dashboard": true, "series": true, "installations": true,
	"installer": true, "jobs": true, "audit": true, "rules": true, "events": true, "stagger": true, "ack": true, "acknowledgements": true,
	"run": true, "commands": true, "credential": true, "uninstallations": true, "tokens": true, "enroll": true, "heartbeat": true,
	"config": true, "test": true, "deliveries": true, "summary": true, "trend": true, "hour-of-day": true, "heatmap": true,
	"distribution": true, "password": true, "2fa": true, "setup": true, "enable": true, "disable": true, "me": true, "login": true, "logout": true, "enrolment-key": true, "lookup": true,
	"map": true, "history": true, "addresses": true, "probes": true, "csv": true, "json": true, "csv.": true, "results.csv": true,
	"results.json": true, "export.csv": true, "export.json": true, "events.": true, "ack.": true, "install.sh": true, "download": true,
	"healthz": true, "readyz": true, "metrics": true,
}

// routeLabel turns a request path into a low-cardinality route name for error reports: ids become {id}, words this
// API does not use become *, and anything outside the API, health and install paths is "ui". Paths a scanner invents
// therefore never create new groups.
func routeLabel(path string) string {
	if !strings.HasPrefix(path, "/api/") && path != "/healthz" && path != "/readyz" && path != "/metrics" && path != "/install.sh" && !strings.HasPrefix(path, "/download/") {
		return "ui"
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) > 5 {
		parts = parts[:5]
	}
	for i, p := range parts {
		switch {
		case idSegment.MatchString(p):
			parts[i] = "{id}"
		case !routeWords[p]:
			parts[i] = "*"
		}
	}
	return "/" + strings.Join(parts, "/")
}
