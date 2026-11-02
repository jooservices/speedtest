// Package testutil provides in-process test doubles for the speed-test agent's
// dependencies, so agent tests run without a real speed-test program or a real
// command center. The Center double speaks the agent side of the wire contract
// (enroll, results, heartbeat) and records everything it receives for later
// assertion. See docs/testing-harness.md.
package testutil

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"
)

// Center is an in-process command-center double. Start it with NewCenter, point
// the agent at its URL, then inspect Enrollments, Results and Heartbeats after
// the agent has run.
type Center struct {
	Server *httptest.Server
	// URL is the base URL (for example http://127.0.0.1:12345) the agent
	// should use as its command-center address.
	URL string

	mu          sync.Mutex
	token       string
	credential  string
	nodeID      string
	enrollments []EnrollRequest
	results     []ResultInput
	heartbeats  []Heartbeat
}

// Option customizes a Center created by NewCenter.
type Option func(*Center)

// WithEnrollmentToken sets the one-time token the double accepts on
// POST /api/agent/enroll.
func WithEnrollmentToken(token string) Option {
	return func(c *Center) { c.token = token }
}

// WithCredential sets the bearer credential the double issues on enrollment and
// requires on subsequent agent endpoints.
func WithCredential(credential string) Option {
	return func(c *Center) { c.credential = credential }
}

// WithNodeID sets the node identifier the double returns on enrollment.
func WithNodeID(nodeID string) Option {
	return func(c *Center) { c.nodeID = nodeID }
}

// NewCenter starts an in-process HTTP server implementing the agent endpoints.
// Callers must Close it when done.
func NewCenter(opts ...Option) *Center {
	c := &Center{
		token:      "test-enrollment-token",
		credential: "test-node-credential",
		nodeID:     "test-node-1",
	}
	for _, o := range opts {
		o(c)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/agent/enroll", c.handleEnroll)
	mux.HandleFunc("POST /api/agent/results", c.handleResults)
	mux.HandleFunc("POST /api/agent/heartbeat", c.handleHeartbeat)

	c.Server = httptest.NewServer(mux)
	c.URL = c.Server.URL
	return c
}

// Close shuts down the in-process server.
func (c *Center) Close() {
	c.Server.Close()
}

// Enrollments returns a snapshot of every enrollment request the double
// received, in order.
func (c *Center) Enrollments() []EnrollRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]EnrollRequest, len(c.enrollments))
	copy(out, c.enrollments)
	return out
}

// Results returns a snapshot of every result the double accepted, in the order
// it received them. Duplicate (replayed) results are not recorded a second
// time.
func (c *Center) Results() []ResultInput {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ResultInput, len(c.results))
	copy(out, c.results)
	return out
}

// Heartbeats returns a snapshot of every heartbeat the double received, in
// order.
func (c *Center) Heartbeats() []Heartbeat {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Heartbeat, len(c.heartbeats))
	copy(out, c.heartbeats)
	return out
}

func (c *Center) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req EnrollRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid enrollment request")
		return
	}
	if req.Token != c.token {
		writeProblem(w, http.StatusUnauthorized, "invalid or expired enrollment token")
		return
	}

	c.mu.Lock()
	c.enrollments = append(c.enrollments, req)
	c.mu.Unlock()

	writeSuccess(w, http.StatusCreated, Enrolled{
		NodeID:     c.nodeID,
		Credential: c.credential,
		Config: AgentConfig{
			Revision:     "1",
			Name:         req.Name,
			ExpectedDown: 300,
			ExpectedUp:   150,
		},
	})
}

func (c *Center) handleResults(w http.ResponseWriter, r *http.Request) {
	if !c.authorized(r) {
		writeProblem(w, http.StatusUnauthorized, "missing or invalid node credential")
		return
	}

	var batch ResultBatch
	if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid result batch")
		return
	}
	if len(batch.Results) == 0 || len(batch.Results) > 100 {
		writeProblem(w, http.StatusUnprocessableEntity, "batch must contain between 1 and 100 results")
		return
	}

	c.mu.Lock()
	outcomes := make([]BatchOutcome, 0, len(batch.Results))
	for i, item := range batch.Results {
		outcome := "accepted"
		status := http.StatusCreated
		if c.hasResultID(item.ID) {
			outcome = "duplicate"
			status = http.StatusOK
		} else {
			c.results = append(c.results, item)
		}
		outcomes = append(outcomes, BatchOutcome{
			Index:   i,
			ID:      nullableID(item.ID),
			Outcome: outcome,
			Status:  status,
			Problem: nil,
		})
	}
	c.mu.Unlock()

	writeSuccess(w, http.StatusMultiStatus, outcomes)
}

func (c *Center) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if !c.authorized(r) {
		writeProblem(w, http.StatusUnauthorized, "missing or invalid node credential")
		return
	}

	var hb Heartbeat
	if err := json.NewDecoder(r.Body).Decode(&hb); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid heartbeat")
		return
	}

	c.mu.Lock()
	c.heartbeats = append(c.heartbeats, hb)
	c.mu.Unlock()

	writeSuccess(w, http.StatusOK, Receipt{
		ReceivedAt: time.Now().UTC().Format(time.RFC3339),
	})
}

// authorized reports whether the request carries the issued bearer credential.
func (c *Center) authorized(r *http.Request) bool {
	return r.Header.Get("Authorization") == "Bearer "+c.credential
}

// hasResultID reports whether a result with the same id has already been
// recorded. Callers must hold the lock.
func (c *Center) hasResultID(id string) bool {
	for _, r := range c.results {
		if r.ID == id {
			return true
		}
	}
	return false
}

// nullableID returns a pointer to id, or nil when id is empty (a missing or
// invalid UUID).
func nullableID(id string) *string {
	if id == "" {
		return nil
	}
	return &id
}

// EnrollRequest is the POST /api/agent/enroll body.
type EnrollRequest struct {
	Token string `json:"token"`
	Name  string `json:"name"`
	Host  string `json:"host"`
	Agent string `json:"agent"`
}

// Enrolled is the data of the enrollment response.
type Enrolled struct {
	NodeID     string      `json:"nodeId"`
	Credential string      `json:"credential"`
	Config     AgentConfig `json:"config"`
}

// AgentConfig is the minimal effective configuration the double returns on
// enrollment.
type AgentConfig struct {
	Revision     string  `json:"revision"`
	Name         string  `json:"name"`
	ExpectedDown float64 `json:"expectedDown"`
	ExpectedUp   float64 `json:"expectedUp"`
}

// Heartbeat is the POST /api/agent/heartbeat body.
type Heartbeat struct {
	Agent    string  `json:"agent"`
	Uptime   int64   `json:"uptime"`
	LastTest *string `json:"lastTest"`
}

// Receipt is the data of the heartbeat response.
type Receipt struct {
	ReceivedAt string `json:"receivedAt"`
}

// ResultBatch is the POST /api/agent/results body.
type ResultBatch struct {
	Results []ResultInput `json:"results"`
}

// ResultInput is one independently validated result item. Nullable measurements
// use pointers so a missing or null value decodes to nil, not zero.
type ResultInput struct {
	ID        string          `json:"id"`
	TS        string          `json:"ts"`
	Status    string          `json:"status"`
	Down      *float64        `json:"down"`
	Up        *float64        `json:"up"`
	Ping      *float64        `json:"ping"`
	Jitter    *float64        `json:"jitter"`
	DLat      *float64        `json:"dLat"`
	ULat      *float64        `json:"uLat"`
	Loss      *float64        `json:"loss"`
	ServerID  *int64          `json:"serverId"`
	IP        string          `json:"ip"`
	Threads   int             `json:"threads"`
	BytesDown int64           `json:"bytesDown"`
	BytesUp   int64           `json:"bytesUp"`
	Duration  float64         `json:"duration"`
	Error     *string         `json:"error"`
	Manual    bool            `json:"manual"`
	Raw       json.RawMessage `json:"raw"`
}

// BatchOutcome is one per-item outcome in the results response.
type BatchOutcome struct {
	Index   int      `json:"index"`
	ID      *string  `json:"id"`
	Outcome string   `json:"outcome"`
	Status  int      `json:"status"`
	Problem *Problem `json:"problem"`
}

// Problem is an RFC 9457 error body.
type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

// successEnvelope is the shared JSON envelope the center returns on success.
type successEnvelope struct {
	Success bool           `json:"success"`
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    any            `json:"data"`
	Meta    map[string]any `json:"meta"`
	Errors  any            `json:"errors"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	contentType := "application/json"
	if status >= 400 {
		contentType = "application/problem+json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeSuccess(w http.ResponseWriter, code int, data any) {
	writeJSON(w, code, successEnvelope{
		Success: true,
		Code:    code,
		Message: http.StatusText(code),
		Data:    data,
		Meta:    map[string]any{},
		Errors:  nil,
	})
}

func writeProblem(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, Problem{
		Type:   "about:blank",
		Title:  http.StatusText(status),
		Status: status,
		Detail: detail,
	})
}
