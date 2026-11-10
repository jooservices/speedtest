package agentapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// Store commits each accepted result independently before returning.
type Store interface {
	SaveResult(context.Context, string, Result) (bool, error)
	Config(context.Context, string) (map[string]any, error)
	Heartbeat(context.Context, string, Heartbeat) (time.Time, error)
	Commands(context.Context, string) ([]Command, error)
	Ack(context.Context, string, string, Ack) (Command, error)
}

type Handler struct{ Store Store }

type ingestObserverKey struct{}

// WithIngestObserver attaches the optional operational item observer to authenticated requests.
func WithIngestObserver(ctx context.Context, observer func(string)) context.Context {
	return context.WithValue(ctx, ingestObserverKey{}, observer)
}
func observeIngest(ctx context.Context, status string) {
	if observer, ok := ctx.Value(ingestObserverKey{}).(func(string)); ok {
		observer(status)
	}
}

// Method reports the method for a recognized agent operation.
func Method(path string) string {
	switch path {
	case "/api/agent/results", "/api/agent/heartbeat":
		return http.MethodPost
	case "/api/agent/config", "/api/agent/commands", "/api/agent/ip":
		return http.MethodGet
	}
	parts := strings.Split(path, "/")
	if len(parts) == 6 && parts[1] == "api" && parts[2] == "agent" && parts[3] == "commands" && parts[4] != "" && parts[5] == "ack" {
		return http.MethodPost
	}
	return ""
}

type Problem struct {
	Type    string `json:"type"`
	Title   string `json:"title"`
	Status  int    `json:"status"`
	Detail  string `json:"detail"`
	TraceID string `json:"trace_id"`
}

func problem(status int, trace string) Problem {
	title := "Internal server error"
	switch status {
	case 400:
		title = "Invalid JSON request"
	case 404:
		title = "Resource not found or not owned"
	case 409:
		title = "Conflicting state"
	case 413:
		title = "Request too large"
	case 422:
		title = "Invalid input"
	case 503:
		title = "Service unavailable"
	}
	category := map[int]string{400: "badrequest", 404: "notfound", 409: "conflict", 413: "toolarge", 422: "validation", 503: "unavailable"}[status]
	if category == "" {
		category = "servererror"
	}
	return Problem{Type: "/problems/" + category, Title: title, Status: status, Detail: title + ".", TraceID: trace}
}
func writeProblem(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problem(status, w.Header().Get("X-Request-ID")))
}
func success(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "code": status, "message": http.StatusText(status), "data": data, "meta": map[string]any{}, "errors": nil})
}
func storeError(w http.ResponseWriter, err error) {
	status := 500
	switch {
	case errors.Is(err, ErrInvalid):
		status = 422
	case errors.Is(err, ErrNotFound):
		status = 404
	case errors.Is(err, ErrConflict):
		status = 409
	}
	writeProblem(w, status)
}
func body(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeProblem(w, 413)
		} else {
			writeProblem(w, 400)
		}
		return nil, false
	}
	if !json.Valid(data) {
		writeProblem(w, 400)
		return nil, false
	}
	return data, true
}

// ServeNode receives an identity already verified by the node bearer middleware.
// observedIP is computed by the server using its configured trusted proxies.
func (h Handler) ServeNode(w http.ResponseWriter, r *http.Request, node, observedIP string) {
	if h.Store == nil {
		writeProblem(w, 503)
		return
	}
	switch r.URL.Path {
	case "/api/agent/results":
		h.results(w, r, node)
	case "/api/agent/config":
		h.config(w, r, node)
	case "/api/agent/heartbeat":
		data, ok := body(w, r, 64<<10)
		if !ok {
			return
		}
		heartbeat, err := ParseHeartbeat(data)
		if err != nil {
			writeProblem(w, 422)
			return
		}
		received, err := h.Store.Heartbeat(r.Context(), node, heartbeat)
		if err != nil {
			storeError(w, err)
			return
		}
		success(w, 200, map[string]any{"receivedAt": received.UTC()})
	case "/api/agent/commands":
		commands, err := h.Store.Commands(r.Context(), node)
		if err != nil {
			storeError(w, err)
			return
		}
		success(w, 200, commands)
	case "/api/agent/ip":
		success(w, 200, map[string]string{"ip": observedIP})
	default:
		parts := strings.Split(r.URL.Path, "/")
		if Method(r.URL.Path) == "" || !UUID(parts[4]) {
			writeProblem(w, 404)
			return
		}
		data, ok := body(w, r, 3<<20)
		if !ok {
			return
		}
		ack, err := ParseAck(data)
		if err != nil {
			writeProblem(w, 422)
			return
		}
		command, err := h.Store.Ack(r.Context(), node, parts[4], ack)
		if err != nil {
			storeError(w, err)
			return
		}
		success(w, 200, command)
	}
}

type Outcome struct {
	Index   int      `json:"index"`
	ID      *string  `json:"id"`
	Outcome string   `json:"outcome"`
	Status  int      `json:"status"`
	Problem *Problem `json:"problem"`
}

func (h Handler) results(w http.ResponseWriter, r *http.Request, node string) {
	data, ok := body(w, r, 8<<20)
	if !ok {
		return
	}
	var batch struct {
		Results []json.RawMessage `json:"results"`
	}
	if Decode(data, &batch, "results") != nil || len(batch.Results) == 0 {
		writeProblem(w, 422)
		return
	}
	if len(batch.Results) > 100 {
		writeProblem(w, 413)
		return
	}
	outcomes := make([]Outcome, 0, len(batch.Results))
	for index, item := range batch.Results {
		result, err := ParseResult(item)
		outcome := Outcome{Index: index, Outcome: "rejected", Status: 422}
		if err == nil {
			outcome.ID = &result.ID
		} else if id := resultItemID(item); id != nil {
			outcome.ID = id
		}
		if err == nil {
			var duplicate bool
			duplicate, err = h.Store.SaveResult(r.Context(), node, result)
			switch {
			case err == nil:
				outcome.Status = 201
				outcome.Outcome = "accepted"
				if duplicate {
					outcome.Status = 200
					outcome.Outcome = "duplicate"
				}
			case errors.Is(err, ErrConflict):
				outcome.Status = 409
			default:
				observeIngest(r.Context(), "rejected")
				storeError(w, err)
				return
			}
		}
		if outcome.Outcome == "rejected" {
			p := problem(outcome.Status, w.Header().Get("X-Request-ID"))
			outcome.Problem = &p
		}
		observeIngest(r.Context(), outcome.Outcome)
		outcomes = append(outcomes, outcome)
	}
	success(w, 207, outcomes)
}

func resultItemID(item []byte) *string {
	var fields map[string]json.RawMessage
	var identity string
	if json.Unmarshal(item, &fields) != nil || json.Unmarshal(fields["id"], &identity) != nil || !UUID(identity) {
		return nil
	}
	identity = strings.ToLower(identity)
	return &identity
}

func (h Handler) config(w http.ResponseWriter, r *http.Request, node string) {
	config, err := h.Store.Config(r.Context(), node)
	if err != nil {
		storeError(w, err)
		return
	}
	data, err := json.Marshal(config)
	if err != nil {
		storeError(w, err)
		return
	}
	sum := sha256.Sum256(data)
	tag := `"` + hex.EncodeToString(sum[:]) + `"`
	w.Header().Set("ETag", tag)
	w.Header().Set("Cache-Control", "private, no-cache")
	for _, candidate := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == tag {
			w.WriteHeader(304)
			return
		}
	}
	success(w, 200, config)
}
