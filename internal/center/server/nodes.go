package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/jooservices/speedtest/internal/center/agentapi"
	"github.com/jooservices/speedtest/internal/center/nodes"
)

const (
	defaultAgentRequestsPerMinute = 120
	defaultAgentBurst             = 20
	maxNodeRequestBody            = 64 << 10
)

// NodeCredentialService is the persistent credential API used by the HTTP server.
type NodeCredentialService interface {
	CreateNode(context.Context, nodes.Input, string, string) (json.RawMessage, bool, error)
	Enroll(context.Context, string, string, string, string) (map[string]any, error)
	RotateCredential(context.Context, string, string, string) (json.RawMessage, bool, error)
	RevokeCredential(context.Context, string) error
	AuthenticateAgent(context.Context, string) (string, error)
	ValidateEnrollmentKey(context.Context, string) (string, error)
	RotateEnrollmentKey(context.Context, string, string) (json.RawMessage, bool, error)
}

type apiEnvelope struct {
	Success bool           `json:"success"`
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    any            `json:"data"`
	Meta    map[string]any `json:"meta"`
	Errors  any            `json:"errors"`
}

type nodeInputWire struct {
	Name         *string                `json:"name"`
	Label        *string                `json:"label"`
	Provider     *string                `json:"provider"`
	Plan         *string                `json:"plan"`
	ExpectedDown *float64               `json:"expectedDown"`
	ExpectedUp   *float64               `json:"expectedUp"`
	Threads      *int                   `json:"threads"`
	Server       *nodes.ServerSelection `json:"server"`
	Note         *string                `json:"note"`
	Enabled      *bool                  `json:"enabled"`
	Binding      *nodes.Binding         `json:"binding"`
	AutoUpdate   *bool                  `json:"autoUpdate"`
}

func (wire nodeInputWire) input() (nodes.Input, error) {
	if wire.Name == nil || wire.ExpectedDown == nil || wire.ExpectedUp == nil {
		return nodes.Input{}, nodes.ErrInvalidInput
	}
	i := nodes.Input{Name: *wire.Name, ExpectedDown: *wire.ExpectedDown, ExpectedUp: *wire.ExpectedUp,
		Label: "", Provider: "", Plan: "", Threads: 4, Enabled: true, Binding: nodes.Binding{}, AutoUpdate: false}
	if wire.Label != nil {
		i.Label = *wire.Label
	}
	if wire.Provider != nil {
		i.Provider = *wire.Provider
	}
	if wire.Plan != nil {
		i.Plan = *wire.Plan
	}
	if wire.Threads != nil {
		if *wire.Threads == 0 {
			return nodes.Input{}, nodes.ErrInvalidInput
		}
		i.Threads = *wire.Threads
	}
	if wire.Server != nil {
		i.ServerID = wire.Server.ID
	}
	if wire.Note != nil {
		i.Note = *wire.Note
	}
	if wire.Enabled != nil {
		i.Enabled = *wire.Enabled
	}
	if wire.Binding != nil {
		i.Binding = *wire.Binding
	}
	if wire.AutoUpdate != nil {
		i.AutoUpdate = *wire.AutoUpdate
	}
	return i.Normalize()
}

type createNodeWire struct {
	Node nodeInputWire `json:"node"`
}

type enrollWire struct {
	Token string `json:"token"`
	Name  string `json:"name"`
	Host  string `json:"host"`
	Agent string `json:"agent"`
}

// The owner works through the dashboard, which loads many pages of data at once; the limit only stops runaway scripts.
const (
	ownerRequestsPerMinute = 3000
	ownerBurst             = 400
)

func newAPIHandler(service NodeCredentialService, perMinute int, ownerAuth *serverAuth, agentStore agentapi.Store) http.Handler {
	limiter := newNodeRateLimiter(perMinute, defaultAgentBurst)
	dispatch := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isCredentialAPIPath(r.URL.Path) {
			writeNotFoundProblem(w, r)
			return
		}
		if methods := allowedNodeMethods(r.URL.Path); !hasAllowedMethod(methods, r.Method) {
			methodNotAllowed(w, r, methods...)
			return
		}
		if service == nil {
			writeAPIProblem(w, r, http.StatusServiceUnavailable, "Service unavailable", "The API service is not configured.")
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/agent/") && r.URL.Path != "/api/agent/enroll" && r.URL.Path != "/api/agent/tokens" {
			handleAgentProtected(service, limiter, w, r, agentStore, ownerAuth.trustedProxyCIDRs)
			return
		}
		switch {
		case r.URL.Path == "/api/nodes":
			handleCreateNode(service, limiter, w, r)
		case r.URL.Path == "/api/agent/tokens":
			handleCreateNodeWithKey(service, limiter, w, r)
		case r.URL.Path == "/api/agent/enroll":
			handleEnroll(service, limiter, w, r)
		case r.URL.Path == "/api/settings/enrolment-key":
			handleRotateEnrollmentKey(service, w, r)
		case strings.HasPrefix(r.URL.Path, "/api/nodes/"):
			handleNodeCredential(service, limiter, w, r)
		default:
			writeNotFoundProblem(w, r)
		}
	})
	protected := ownerAuth.requireOwner(dispatch)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isOwnerMutation(r) {
			protected.ServeHTTP(w, r)
			return
		}
		dispatch.ServeHTTP(w, r)
	})
}

func allowedNodeMethods(path string) []string {
	if method := agentapi.Method(path); method != "" {
		return []string{method}
	}
	switch path {
	case "/api/nodes", "/api/agent/tokens", "/api/agent/enroll", "/api/settings/enrolment-key":
		return []string{http.MethodPost}
	}
	if strings.HasPrefix(path, "/api/agent/") {
		return []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete}
	}
	return []string{http.MethodPost, http.MethodDelete}
}

func hasAllowedMethod(methods []string, method string) bool {
	for _, allowed := range methods {
		if method == allowed {
			return true
		}
	}
	return false
}

func isOwnerMutation(r *http.Request) bool {
	if r.URL.Path == "/api/nodes" || r.URL.Path == "/api/settings/enrolment-key" {
		return r.Method == http.MethodPost
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	return len(parts) == 4 && parts[0] == "api" && parts[1] == "nodes" && parts[3] == "credential" &&
		(r.Method == http.MethodPost || r.Method == http.MethodDelete)
}

func isCredentialAPIPath(path string) bool {
	if path == "/api/nodes" || path == "/api/agent/tokens" || path == "/api/agent/enroll" || path == "/api/settings/enrolment-key" {
		return true
	}
	if strings.HasPrefix(path, "/api/agent/") {
		return true
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	return len(parts) == 4 && parts[0] == "api" && parts[1] == "nodes" && parts[3] == "credential"
}

func handleCreateNode(service NodeCredentialService, limiter *nodeRateLimiter, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r, http.MethodPost)
		return
	}
	principal, err := ownerPrincipal(r)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	if !allowRate(limiter, "owner:"+principal, w, r) {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	wire := nodeInputWire{}
	if !decodeJSON(w, r, &wire) {
		return
	}
	input, err := wire.input()
	if err != nil {
		writeAPIProblem(w, r, http.StatusUnprocessableEntity, "Validation failed", "The node configuration is invalid.")
		return
	}
	data, _, err := service.CreateNode(r.Context(), input, principal, key)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	setLocation(w, data, "/api/nodes/")
	writeEnvelope(w, http.StatusCreated, "Node and enrollment token created", data)
}

func handleCreateNodeWithKey(service NodeCredentialService, limiter *nodeRateLimiter, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r, http.MethodPost)
		return
	}
	keyValue, ok := bearerValue(r)
	if !ok {
		writeAPIProblem(w, r, http.StatusUnauthorized, "Unauthorized", "Enrollment key is required.")
		return
	}
	principal, err := service.ValidateEnrollmentKey(r.Context(), keyValue)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	if !allowRate(limiter, "enrollment-key:"+principal, w, r) {
		return
	}
	idemKey, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	wire := createNodeWire{}
	if !decodeJSON(w, r, &wire) {
		return
	}
	input, err := wire.Node.input()
	if err != nil {
		writeAPIProblem(w, r, http.StatusUnprocessableEntity, "Validation failed", "The node configuration is invalid.")
		return
	}
	data, _, err := service.CreateNode(r.Context(), input, principal, idemKey)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	setLocation(w, data, "/api/nodes/")
	writeEnvelope(w, http.StatusCreated, "Node and enrollment token created", data)
}

func handleEnroll(service NodeCredentialService, limiter *nodeRateLimiter, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r, http.MethodPost)
		return
	}
	if !allowRate(limiter, "ip:"+clientIP(r), w, r) {
		return
	}
	wire := enrollWire{}
	if !decodeJSON(w, r, &wire) {
		return
	}
	data, err := service.Enroll(r.Context(), wire.Token, wire.Name, wire.Host, wire.Agent)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeEnvelope(w, http.StatusCreated, "Node enrolled", data)
}

func handleRotateEnrollmentKey(service NodeCredentialService, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r, http.MethodPost)
		return
	}
	principal, err := ownerPrincipal(r)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	data, _, err := service.RotateEnrollmentKey(r.Context(), principal, key)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeEnvelope(w, http.StatusCreated, "Enrollment key rotated", data)
}

func handleNodeCredential(service NodeCredentialService, limiter *nodeRateLimiter, w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != "api" || parts[1] != "nodes" || parts[3] != "credential" {
		writeNotFoundProblem(w, r)
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		methodNotAllowed(w, r, http.MethodPost, http.MethodDelete)
		return
	}
	principal, err := ownerPrincipal(r)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	if !allowRate(limiter, "owner:"+principal, w, r) {
		return
	}
	switch r.Method {
	case http.MethodPost:
		key, ok := idempotencyKey(w, r)
		if !ok {
			return
		}
		data, _, err := service.RotateCredential(r.Context(), parts[2], principal, key)
		if err != nil {
			writeServiceError(w, r, err)
			return
		}
		writeEnvelope(w, http.StatusCreated, "Node credential rotated", data)
	case http.MethodDelete:
		if err := service.RevokeCredential(r.Context(), parts[2]); err != nil {
			writeServiceError(w, r, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w, r, http.MethodPost, http.MethodDelete)
	}
}

func handleAgentProtected(service NodeCredentialService, limiter *nodeRateLimiter, w http.ResponseWriter, r *http.Request, store agentapi.Store, trustedProxies string) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodDelete {
		methodNotAllowed(w, r, http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete)
		return
	}
	bearer, ok := bearerValue(r)
	if !ok {
		writeAPIProblem(w, r, http.StatusUnauthorized, "Unauthorized", "A valid node credential is required.")
		return
	}
	nodeID, err := service.AuthenticateAgent(r.Context(), bearer)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	if !allowRate(limiter, "node:"+nodeID, w, r) {
		return
	}
	if agentapi.Method(r.URL.Path) != "" {
		(agentapi.Handler{Store: store}).ServeNode(w, r, nodeID, remoteIP(r.RemoteAddr, r.Header.Get("X-Forwarded-For"), trustedProxies))
		return
	}
	writeNotFoundProblem(w, r)
}
