package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/jooservices/speedtest/internal/center/ownerapi"
)

// OwnerNodesStore is the dashboard-side node API: reads need a session, writes also CSRF and origin checks.
type OwnerNodesStore interface {
	List(ctx context.Context, page, size int) ([]map[string]any, int, error)
	Get(ctx context.Context, id string) (map[string]any, string, error)
	Update(ctx context.Context, id string, patch map[string]json.RawMessage, ifMatch string) (map[string]any, string, error)
	Delete(ctx context.Context, id, ifMatch string) error
	QueueCommand(ctx context.Context, id, typ, version string) (map[string]any, error)
	Command(ctx context.Context, id, commandID string) (map[string]any, error)
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type ownerNodes struct {
	store   OwnerNodesStore
	auth    *serverAuth
	limiter *nodeRateLimiter
}

func newOwnerNodesHandler(store OwnerNodesStore, auth *serverAuth) http.Handler {
	h := &ownerNodes{store: store, auth: auth, limiter: newOwnerRateLimiter()}
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

// ownerNodesRoute reports whether the request belongs to the owner node API. Creating a node and the
// credential routes stay with the enrolment handler.
func ownerNodesRoute(path string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 || parts[0] != "api" || parts[1] != "nodes" {
		return false
	}
	switch len(parts) {
	case 2:
		return true
	case 3:
		return true
	case 4:
		return parts[3] == "run" || parts[3] == "commands"
	case 5:
		return parts[3] == "commands"
	}
	return false
}

func (h *ownerNodes) serve(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	principal, err := ownerPrincipal(r)
	if err != nil {
		writeUnauthorized(w, r)
		return
	}
	if !allowRate(h.limiter, "owner:"+principal, w, r) {
		return
	}
	if len(parts) >= 3 && !uuidPattern.MatchString(parts[2]) {
		writeNotFoundProblem(w, r)
		return
	}
	switch {
	case len(parts) == 2:
		if r.Method == http.MethodPost {
			// Creating a node is handled by the enrolment handler.
			writeAPIProblem(w, r, http.StatusInternalServerError, "Internal failure", "Internal failure; safe detail only.")
			return
		}
		h.list(w, r, r.Method)
	case len(parts) == 3:
		h.node(w, r, parts[2])
	case len(parts) == 4 && parts[3] == "run":
		h.runNow(w, r, parts[2])
	case len(parts) == 4 && parts[3] == "commands":
		h.createCommand(w, r, parts[2])
	case len(parts) == 5:
		h.commandProgress(w, r, parts[2], parts[4])
	default:
		writeRouteNotFound(w, r)
	}
}

// runNow queues a speed test (POST /api/nodes/{id}/run).
func (h *ownerNodes) runNow(w http.ResponseWriter, r *http.Request, nodeID string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r, http.MethodPost)
		return
	}
	h.command(w, r, nodeID, "run_now", "")
}

// createCommand queues a typed command such as an agent update (POST /api/nodes/{id}/commands).
func (h *ownerNodes) createCommand(w http.ResponseWriter, r *http.Request, nodeID string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r, http.MethodPost)
		return
	}
	var in struct {
		Type    string `json:"type"`
		Version string `json:"version"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	h.command(w, r, nodeID, in.Type, in.Version)
}

// commandProgress reports how a queued command is doing (GET /api/nodes/{id}/commands/{commandId}).
func (h *ownerNodes) commandProgress(w http.ResponseWriter, r *http.Request, nodeID, commandID string) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, r, http.MethodGet)
		return
	}
	if !uuidPattern.MatchString(commandID) {
		writeNotFoundProblem(w, r)
		return
	}
	cmd, err := h.store.Command(r.Context(), nodeID, commandID)
	if err != nil {
		writeOwnerError(w, r, err)
		return
	}
	writeEnvelope(w, http.StatusOK, "Command progress", cmd)
}

func (h *ownerNodes) list(w http.ResponseWriter, r *http.Request, method string) {
	if method != http.MethodGet {
		methodNotAllowed(w, r, http.MethodGet, http.MethodPost)
		return
	}
	page, size, ok := paging(w, r)
	if !ok {
		return
	}
	items, total, err := h.store.List(r.Context(), page, size)
	if err != nil {
		writeOwnerError(w, r, err)
		return
	}
	writePage(w, items, page, size, total)
}

func paging(w http.ResponseWriter, r *http.Request) (int, int, bool) {
	page, size := 1, 25
	q := r.URL.Query()
	for key, dest := range map[string]*int{"page": &page, "size": &size} {
		if raw := q.Get(key); raw != "" {
			v, err := strconv.Atoi(raw)
			if err != nil || v < 1 || (key == "size" && v > 100) {
				writeAPIProblem(w, r, http.StatusUnprocessableEntity, "Validation failed", "page and size must be positive integers (size at most 100).")
				return 0, 0, false
			}
			*dest = v
		}
	}
	return page, size, true
}

func (h *ownerNodes) node(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodGet:
		node, etag, err := h.store.Get(r.Context(), id)
		if err != nil {
			writeOwnerError(w, r, err)
			return
		}
		w.Header().Set("ETag", etag)
		writeEnvelope(w, http.StatusOK, "Success", node)
	case http.MethodPatch:
		ifMatch, ok := requireIfMatch(w, r)
		if !ok {
			return
		}
		var patch map[string]json.RawMessage
		if !decodeJSON(w, r, &patch) {
			return
		}
		node, etag, err := h.store.Update(r.Context(), id, patch, ifMatch)
		if err != nil {
			writeOwnerError(w, r, err)
			return
		}
		w.Header().Set("ETag", etag)
		writeEnvelope(w, http.StatusOK, "Node updated", node)
	case http.MethodDelete:
		ifMatch, ok := requireIfMatch(w, r)
		if !ok {
			return
		}
		if r.URL.Query().Get("confirm") != "true" {
			writeAPIProblem(w, r, http.StatusUnprocessableEntity, "Validation failed", "Deleting a node needs confirm=true.")
			return
		}
		if err := h.store.Delete(r.Context(), id, ifMatch); err != nil {
			writeOwnerError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w, r, http.MethodGet, http.MethodPatch, http.MethodDelete)
	}
}

func (h *ownerNodes) command(w http.ResponseWriter, r *http.Request, id, typ, version string) {
	cmd, err := h.store.QueueCommand(r.Context(), id, typ, version)
	if err != nil {
		writeOwnerError(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/nodes/"+id+"/commands/"+cmd["id"].(string))
	writeEnvelope(w, http.StatusAccepted, "Command queued", cmd)
}

func requireIfMatch(w http.ResponseWriter, r *http.Request) (string, bool) {
	values := r.Header.Values("If-Match")
	if len(values) != 1 || strings.TrimSpace(values[0]) == "" {
		writeProblemResponse(w, http.StatusPreconditionRequired, problemResponse{Type: "/problems/preconditionrequired", Title: "If-Match required",
			Status: http.StatusPreconditionRequired, Detail: "If-Match required.", TraceID: requestIDFromContext(r.Context())})
		return "", false
	}
	return strings.TrimSpace(values[0]), true
}

func writeOwnerError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ownerapi.ErrNotFound):
		writeNotFoundProblem(w, r)
	case errors.Is(err, ownerapi.ErrStale):
		writeProblemResponse(w, http.StatusPreconditionFailed, problemResponse{Type: "/problems/stale", Title: "ETag does not match",
			Status: http.StatusPreconditionFailed, Detail: "ETag does not match.", TraceID: requestIDFromContext(r.Context())})
	case errors.Is(err, ownerapi.ErrConflict):
		writeAPIProblem(w, r, http.StatusConflict, "Conflict", "The request conflicts with current resource state.")
	case errors.Is(err, ownerapi.ErrInvalid):
		writeAPIProblem(w, r, http.StatusUnprocessableEntity, "Validation failed", "The request is invalid.")
	default:
		writeAPIProblem(w, r, http.StatusInternalServerError, "Internal failure", "Internal failure; safe detail only.")
	}
}
