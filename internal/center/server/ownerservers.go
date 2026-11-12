package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// OwnerServersStore handles discovered server catalogs and per-engine pins.
type OwnerServersStore interface {
	ServerCatalog(ctx context.Context, nodeID, engine string) (map[string]any, string, error)
	SelectServer(ctx context.Context, nodeID, engine string, serverID *int64, tag string) (map[string]any, string, error)
	QueueServerList(ctx context.Context, nodeID, engine string) (map[string]any, error)
}

type ownerServers struct {
	store   OwnerServersStore
	limiter *nodeRateLimiter
}

func newOwnerServersHandler(store OwnerServersStore, auth *serverAuth) http.Handler {
	h := &ownerServers{store: store, limiter: newOwnerRateLimiter()}
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

func (h *ownerServers) serve(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != "api" || parts[1] != "nodes" || !uuidPattern.MatchString(parts[2]) {
		writeNotFoundProblem(w, r)
		return
	}
	principal, err := ownerPrincipal(r)
	if err != nil {
		writeUnauthorized(w, r)
		return
	}
	if !allowRate(h.limiter, "owner:"+principal, w, r) {
		return
	}
	switch parts[3] {
	case "servers":
		serveServerCatalog(w, r, h.store, parts[2])
	case "server-selection":
		serveServerSelection(w, r, h.store, parts[2])
	default:
		writeNotFoundProblem(w, r)
	}
}

func serveServerCatalog(w http.ResponseWriter, r *http.Request, store OwnerServersStore, nodeID string) {
	engine, ok := serverEngineQuery(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		catalog, tag, err := store.ServerCatalog(r.Context(), nodeID, engine)
		if err != nil {
			writeOwnerError(w, r, err)
			return
		}
		w.Header().Set("ETag", tag)
		writeEnvelope(w, http.StatusOK, "Server catalog", catalog)
	case http.MethodPost:
		command, err := store.QueueServerList(r.Context(), nodeID, engine)
		if err != nil {
			writeOwnerError(w, r, err)
			return
		}
		w.Header().Set("Location", "/api/nodes/"+nodeID+"/commands/"+command["id"].(string))
		writeEnvelope(w, http.StatusAccepted, "Server discovery queued", command)
	default:
		methodNotAllowed(w, r, http.MethodGet, http.MethodPost)
	}
}

func serveServerSelection(w http.ResponseWriter, r *http.Request, store OwnerServersStore, nodeID string) {
	if r.Method != http.MethodPut {
		methodNotAllowed(w, r, http.MethodPut)
		return
	}
	tag, ok := requireIfMatch(w, r)
	if !ok {
		return
	}
	var input struct {
		Engine   string          `json:"engine"`
		ServerID json.RawMessage `json:"serverId"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if input.Engine == "" || len(input.ServerID) == 0 {
		writeAPIProblem(w, r, http.StatusUnprocessableEntity, "Validation failed", "engine and serverId are required.")
		return
	}
	serverID, valid := selectedServerID(input.ServerID)
	if !valid {
		writeAPIProblem(w, r, http.StatusUnprocessableEntity, "Validation failed", "serverId must be auto or a positive server ID.")
		return
	}
	selection, nextTag, err := store.SelectServer(r.Context(), nodeID, input.Engine, serverID, tag)
	if err != nil {
		writeOwnerError(w, r, err)
		return
	}
	w.Header().Set("ETag", nextTag)
	writeEnvelope(w, http.StatusOK, "Server selection saved", selection)
}

func serverEngineQuery(w http.ResponseWriter, r *http.Request) (string, bool) {
	values := r.URL.Query()["engine"]
	if len(values) != 1 || len(r.URL.Query()) != 1 || values[0] != "ookla" && values[0] != "librespeed" {
		writeAPIProblem(w, r, http.StatusUnprocessableEntity, "Validation failed", "engine must be ookla or librespeed.")
		return "", false
	}
	return values[0], true
}

func selectedServerID(raw json.RawMessage) (*int64, bool) {
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return nil, value == "auto"
	}
	var id int64
	if json.Unmarshal(raw, &id) != nil || id < 1 {
		return nil, false
	}
	return &id, true
}

func ownerServersRoute(path string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	return len(parts) == 4 && parts[0] == "api" && parts[1] == "nodes" && uuidPattern.MatchString(parts[2]) && (parts[3] == "servers" || parts[3] == "server-selection")
}
