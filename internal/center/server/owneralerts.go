package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/jooservices/speedtest/internal/center/alerts"
)

// OwnerAlertsStore is the session-protected API for rule configuration and alert activity.
type OwnerAlertsStore interface {
	ListRules(context.Context, int, int) ([]alerts.Rule, int, error)
	CreateRule(context.Context, string, string, alerts.RuleInput) (json.RawMessage, bool, error)
	GetRule(context.Context, string) (alerts.Rule, string, error)
	ReplaceRule(context.Context, string, alerts.RuleInput, string) (alerts.Rule, string, error)
	DeleteRule(context.Context, string, string) error
	ListEvents(context.Context, alerts.EventFilter, int, int) ([]alerts.Event, int, error)
	GetEvent(context.Context, string) (alerts.Event, string, error)
	Acknowledge(context.Context, string, bool, string) (alerts.Event, string, error)
	AcknowledgeAll(context.Context) (string, error)
}

type ownerAlerts struct {
	store   OwnerAlertsStore
	limiter *nodeRateLimiter
}

func newOwnerAlertsHandler(store OwnerAlertsStore, auth *serverAuth) http.Handler {
	h := &ownerAlerts{store: store, limiter: newOwnerRateLimiter()}
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

func ownerAlertsRoute(path string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 3 || len(parts) > 5 || parts[0] != "api" || parts[1] != "alerts" {
		return false
	}
	if parts[2] == "acknowledgements" {
		return len(parts) == 3
	}
	if parts[2] == "rules" {
		return len(parts) == 3 || len(parts) == 4
	}
	if parts[2] != "events" {
		return false
	}
	return len(parts) == 3 || len(parts) == 4 || (len(parts) == 5 && parts[4] == "ack")
}

func (h *ownerAlerts) serve(w http.ResponseWriter, r *http.Request) {
	principal, err := ownerPrincipal(r)
	if err != nil {
		writeUnauthorized(w, r)
		return
	}
	if !allowRate(h.limiter, "owner:"+principal, w, r) {
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) == 3 {
		switch parts[2] {
		case "rules":
			h.ruleCollection(w, r, principal)
		case "events":
			h.eventCollection(w, r)
		case "acknowledgements":
			h.acknowledgeAll(w, r)
		default:
			writeNotFoundProblem(w, r)
		}
		return
	}
	if len(parts) == 4 && parts[2] == "rules" {
		if !uuidPattern.MatchString(parts[3]) {
			writeNotFoundProblem(w, r)
			return
		}
		h.ruleResource(w, r, parts[3])
		return
	}
	if len(parts) == 4 && parts[2] == "events" {
		h.getEvent(w, r, parts[3])
		return
	}
	if len(parts) == 5 && parts[2] == "events" && parts[4] == "ack" {
		if !uuidPattern.MatchString(parts[3]) {
			writeNotFoundProblem(w, r)
			return
		}
		if r.Method != http.MethodPut {
			methodNotAllowed(w, r, http.MethodPut)
			return
		}
		h.acknowledge(w, r, strings.ToLower(parts[3]))
		return
	}
	writeNotFoundProblem(w, r)
}

// getEvent returns one alert event with its ETag (GET /api/alerts/events/{id}).
func (h *ownerAlerts) getEvent(w http.ResponseWriter, r *http.Request, id string) {
	if !uuidPattern.MatchString(id) {
		writeNotFoundProblem(w, r)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, r, http.MethodGet)
		return
	}
	event, etag, err := h.store.GetEvent(r.Context(), strings.ToLower(id))
	if err != nil {
		writeAlertError(w, r, err)
		return
	}
	w.Header().Set("ETag", etag)
	writeEnvelope(w, http.StatusOK, "Success", event)
}

func (h *ownerAlerts) ruleCollection(w http.ResponseWriter, r *http.Request, principal string) {
	switch r.Method {
	case http.MethodGet:
		page, size, ok := paging(w, r)
		if !ok {
			return
		}
		items, total, err := h.store.ListRules(r.Context(), page, size)
		if err != nil {
			writeAlertError(w, r, err)
			return
		}
		writePage(w, items, page, size, total)
	case http.MethodPost:
		in, ok := decodeAlertRule(w, r)
		if !ok {
			return
		}
		key, ok := idempotencyKey(w, r)
		if !ok {
			return
		}
		data, _, err := h.store.CreateRule(r.Context(), principal, key, in)
		if err != nil {
			writeAlertError(w, r, err)
			return
		}
		var created struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(data, &created) != nil || created.ID == "" {
			writeAPIProblem(w, r, http.StatusInternalServerError, "Internal failure", "Internal failure; safe detail only.")
			return
		}
		w.Header().Set("Location", "/api/alerts/rules/"+created.ID)
		writeEnvelope(w, http.StatusCreated, "Alert rule created", data)
	default:
		methodNotAllowed(w, r, http.MethodGet, http.MethodPost)
	}
}

func (h *ownerAlerts) ruleResource(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodGet:
		rule, etag, err := h.store.GetRule(r.Context(), strings.ToLower(id))
		if err != nil {
			writeAlertError(w, r, err)
			return
		}
		w.Header().Set("ETag", etag)
		writeEnvelope(w, http.StatusOK, "Success", rule)
	case http.MethodPut:
		ifMatch, ok := requireIfMatch(w, r)
		if !ok {
			return
		}
		in, ok := decodeAlertRule(w, r)
		if !ok {
			return
		}
		rule, etag, err := h.store.ReplaceRule(r.Context(), strings.ToLower(id), in, ifMatch)
		if err != nil {
			writeAlertError(w, r, err)
			return
		}
		w.Header().Set("ETag", etag)
		writeEnvelope(w, http.StatusOK, "Alert rule replaced", rule)
	case http.MethodDelete:
		ifMatch, ok := requireIfMatch(w, r)
		if !ok {
			return
		}
		if err := h.store.DeleteRule(r.Context(), strings.ToLower(id), ifMatch); err != nil {
			writeAlertError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w, r, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}

func (h *ownerAlerts) eventCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, r, http.MethodGet)
		return
	}
	page, size, ok := paging(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	filter := alerts.EventFilter{State: q.Get("state"), Severity: q.Get("severity"), NodeID: strings.ToLower(q.Get("node"))}
	if filter.State == "" {
		filter.State = "open"
	}
	if filter.State != "open" && filter.State != "all" {
		invalid(w, r, "state must be open or all.")
		return
	}
	if filter.Severity != "" && filter.Severity != "info" && filter.Severity != "warn" && filter.Severity != "crit" {
		invalid(w, r, "severity must be info, warn or crit.")
		return
	}
	if filter.NodeID != "" && !uuidPattern.MatchString(filter.NodeID) {
		invalid(w, r, "node must be a valid node ID.")
		return
	}
	items, total, err := h.store.ListEvents(r.Context(), filter, page, size)
	if err != nil {
		writeAlertError(w, r, err)
		return
	}
	writePage(w, items, page, size, total)
}

func (h *ownerAlerts) acknowledge(w http.ResponseWriter, r *http.Request, id string) {
	ifMatch, ok := requireIfMatch(w, r)
	if !ok {
		return
	}
	var input struct {
		Acked *bool `json:"acked"`
	}
	if !decodeExactJSON(w, r, &input, "acked") {
		return
	}
	if input.Acked == nil {
		invalid(w, r, "acked is required and must be a boolean.")
		return
	}
	event, etag, err := h.store.Acknowledge(r.Context(), id, *input.Acked, ifMatch)
	if err != nil {
		writeAlertError(w, r, err)
		return
	}
	w.Header().Set("ETag", etag)
	writeEnvelope(w, http.StatusOK, "Alert acknowledgement updated", event)
}

type alertRuleWire struct {
	Name     *string         `json:"name"`
	Metric   *string         `json:"metric"`
	Op       *string         `json:"op"`
	Value    *float64        `json:"value"`
	ForTests *int            `json:"forTests"`
	Nodes    json.RawMessage `json:"nodes"`
	Severity *string         `json:"severity"`
	Channels *[]string       `json:"channels"`
	Enabled  *bool           `json:"enabled"`
}

func decodeAlertRule(w http.ResponseWriter, r *http.Request) (alerts.RuleInput, bool) {
	var wire alertRuleWire
	if !decodeExactJSON(w, r, &wire, "name", "metric", "op", "value", "forTests", "nodes", "severity", "channels", "enabled") {
		return alerts.RuleInput{}, false
	}
	if wire.Name == nil || wire.Metric == nil || wire.Op == nil || wire.Value == nil || wire.ForTests == nil ||
		len(wire.Nodes) == 0 || wire.Severity == nil || wire.Channels == nil || wire.Enabled == nil {
		invalid(w, r, "All alert rule fields are required.")
		return alerts.RuleInput{}, false
	}
	in := alerts.RuleInput{Name: *wire.Name, Metric: *wire.Metric, Op: *wire.Op, Value: *wire.Value, ForTests: *wire.ForTests,
		Severity: *wire.Severity, Channels: *wire.Channels, Enabled: *wire.Enabled}
	if bytes.Equal(bytes.TrimSpace(wire.Nodes), []byte(`"all"`)) {
		in.AllNodes = true
	} else if err := json.Unmarshal(wire.Nodes, &in.NodeIDs); err != nil || in.NodeIDs == nil {
		invalid(w, r, "nodes must be \"all\" or a list of node IDs.")
		return alerts.RuleInput{}, false
	}
	if _, err := alerts.NormalizeRule(in); err != nil {
		invalid(w, r, "The alert rule configuration is invalid.")
		return alerts.RuleInput{}, false
	}
	return in, true
}

func decodeExactJSON(w http.ResponseWriter, r *http.Request, target any, fields ...string) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxNodeRequestBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeAPIProblem(w, r, http.StatusRequestEntityTooLarge, "Request too large", "The request body exceeds the allowed size.")
		} else {
			writeAPIProblem(w, r, http.StatusBadRequest, "Bad request", "The JSON request body is invalid.")
		}
		return false
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil || object == nil {
		writeAPIProblem(w, r, http.StatusBadRequest, "Bad request", "The JSON request body is invalid.")
		return false
	}
	allowed := make(map[string]bool, len(fields))
	for _, field := range fields {
		allowed[field] = true
	}
	for field := range object {
		if !allowed[field] {
			writeAPIProblem(w, r, http.StatusBadRequest, "Bad request", "The JSON request body contains an unknown field.")
			return false
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeAPIProblem(w, r, http.StatusBadRequest, "Bad request", "The JSON request body is invalid.")
		return false
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		writeAPIProblem(w, r, http.StatusBadRequest, "Bad request", "The request body must contain one JSON value.")
		return false
	}
	return true
}

func writeAlertError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, alerts.ErrNotFound):
		writeNotFoundProblem(w, r)
	case errors.Is(err, alerts.ErrStale):
		writeProblemResponse(w, http.StatusPreconditionFailed, problemResponse{Type: "/problems/stale", Title: "ETag does not match",
			Status: http.StatusPreconditionFailed, Detail: "ETag does not match.", TraceID: requestIDFromContext(r.Context())})
	case errors.Is(err, alerts.ErrConflict):
		writeAPIProblem(w, r, http.StatusConflict, "Conflict", "The request conflicts with current alert state.")
	case errors.Is(err, alerts.ErrInvalid):
		invalid(w, r, "The alert request is invalid.")
	default:
		writeAPIProblem(w, r, http.StatusInternalServerError, "Internal failure", "Internal failure; safe detail only.")
	}
}

// acknowledgeAll starts the bulk acknowledgement of the events that are open now and returns the job to poll.
func (h *ownerAlerts) acknowledgeAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r, http.MethodPost)
		return
	}
	if _, ok := idempotencyKey(w, r); !ok {
		return
	}
	id, err := h.store.AcknowledgeAll(r.Context())
	if err != nil {
		writeAlertError(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/jobs/"+id)
	writeEnvelope(w, http.StatusAccepted, "Acknowledgement started", map[string]any{"id": id, "status": "succeeded", "error": nil})
}
