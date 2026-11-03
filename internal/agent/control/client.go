package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jooservices/speedtest/internal/agent/update"
)

// DefaultTimeout bounds each control request so an unreachable center never
// blocks the control loop.
const DefaultTimeout = 10 * time.Second

// Heartbeat is the POST /api/agent/heartbeat body.
type Heartbeat struct {
	Agent        string        `json:"agent"`
	Uptime       int64         `json:"uptime"`
	LastTest     *string       `json:"lastTest"`
	UpdateResult *UpdateResult `json:"updateResult,omitempty"`
}

// UpdateResult is the outcome of the latest self-update, reported with the heartbeat.
type UpdateResult = update.Result

// ScheduleInput is the schedule part of the effective agent configuration.
// Interval and offset are whole minutes.
type ScheduleInput struct {
	Interval int  `json:"interval"`
	Offset   int  `json:"offset"`
	Enabled  bool `json:"enabled"`
}

// QuietHours is the daily quiet window in the configuration.
type QuietHours struct {
	QuietStart string `json:"quietStart"`
	QuietEnd   string `json:"quietEnd"`
	Timezone   string `json:"timezone"`
}

// AgentConfig is the node's own effective configuration returned by
// GET /api/agent/config. Only the fields the scheduler needs are decoded; the
// rest are ignored.
type AgentConfig struct {
	Engine  string          `json:"engine"`
	Threads int             `json:"threads"`
	Server  json.RawMessage `json:"server"`
	Binding *struct {
		Interface string  `json:"iface"`
		SourceIP  *string `json:"sourceIp"`
	} `json:"binding"`
	AutoUpdate   bool            `json:"autoUpdate"`
	Release      *update.Release `json:"release"`
	Revision     string          `json:"revision"`
	Name         string          `json:"name"`
	ExpectedDown float64         `json:"expectedDown"`
	ExpectedUp   float64         `json:"expectedUp"`
	Schedule     ScheduleInput   `json:"schedule"`
	Quiet        QuietHours      `json:"quiet"`
}

// Command is one queued control command. status/phase/pct/resultId/error carry
// progress and are ignored by the executor.
type Command struct {
	ID        string  `json:"id"`
	NodeID    string  `json:"nodeId"`
	Type      string  `json:"type"`
	Version   *string `json:"version"`
	Status    string  `json:"status"`
	CreatedAt string  `json:"createdAt"`
	AckedAt   *string `json:"ackedAt"`
	Phase     string  `json:"phase"`
	Pct       float64 `json:"pct"`
	ResultID  *string `json:"resultId"`
	Error     *string `json:"error"`
}

// CommandAck is the POST /api/agent/commands/{id}/ack body.
type CommandAck struct {
	Status   string  `json:"status"`
	Phase    string  `json:"phase,omitempty"`
	Pct      float64 `json:"pct"`
	ResultID *string `json:"resultId,omitempty"`
	Error    *string `json:"error,omitempty"`
}

// successEnvelope is the shared Success wrapper around the data field.
type successEnvelope struct {
	Success bool            `json:"success"`
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
	Meta    json.RawMessage `json:"meta"`
	Errors  json.RawMessage `json:"errors"`
}

// problem is an RFC 9457 error body.
type problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

// Client speaks the node side of the agent control contract against a command
// center. The credential is a secret and is only ever sent as a bearer token,
// never logged.
type Client struct {
	BaseURL    *url.URL
	HTTP       *http.Client
	Credential string
	Agent      string
	Timeout    time.Duration
}

func (c Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DefaultTimeout
}

// newRequest builds a bearer-authenticated request against the center.
func (c Client) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	if c.BaseURL == nil {
		return nil, errors.New("center URL is required")
	}
	if c.BaseURL.Scheme != "http" && c.BaseURL.Scheme != "https" {
		return nil, errors.New("center URL must be http or https")
	}

	if c.BaseURL.Host == "" || c.BaseURL.User != nil || c.BaseURL.RawQuery != "" || c.BaseURL.Fragment != "" {
		return nil, errors.New("center URL must have a host and no userinfo, query or fragment")
	}
	var r io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		r = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL.JoinPath(path).String(), r)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Credential)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// do performs the request with a bounded timeout and returns the status,
// response headers and body.
func (c Client) do(ctx context.Context, req *http.Request) (int, http.Header, []byte, error) {
	if c.timeout() > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout())
		defer cancel()
		req = req.WithContext(ctx)
	}
	client := *c.client()
	// Control requests carry a credential and must stay on the configured origin.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, c.redact(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return 0, nil, nil, c.redact(err)
	}
	if len(body) > 1<<20 {
		return 0, nil, nil, errors.New("control response exceeds 1 MiB")
	}
	return resp.StatusCode, resp.Header, body, nil
}

// Heartbeat reports node liveness to the center.
func (c Client) Heartbeat(ctx context.Context, hb Heartbeat) error {
	req, err := c.newRequest(ctx, http.MethodPost, "api/agent/heartbeat", hb)
	if err != nil {
		return err
	}
	status, _, body, err := c.do(ctx, req)
	if err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	if status < 200 || status > 299 {
		return c.redact(decodeProblem(status, body))
	}
	return nil
}

// Config fetches the node's effective configuration. When etag is non-empty it
// is sent as If-None-Match; a 304 response returns notModified=true and the
// same etag. On 200 the returned etag is the new revision.
func (c Client) Config(ctx context.Context, etag string) (cfg AgentConfig, newETag string, notModified bool, err error) {
	req, err := c.newRequest(ctx, http.MethodGet, "api/agent/config", nil)
	if err != nil {
		return AgentConfig{}, "", false, err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	status, header, body, err := c.do(ctx, req)
	if err != nil {
		return AgentConfig{}, "", false, fmt.Errorf("fetch config: %w", err)
	}
	if status == http.StatusNotModified {
		return AgentConfig{}, etag, true, nil
	}
	if status < 200 || status > 299 {
		return AgentConfig{}, "", false, c.redact(decodeProblem(status, body))
	}
	if err := decodeData(body, &cfg); err != nil {
		return AgentConfig{}, "", false, fmt.Errorf("decode config: %w", err)
	}
	return cfg, header.Get("ETag"), false, nil
}

// PollCommands returns the node's own unacknowledged commands, oldest first.
func (c Client) PollCommands(ctx context.Context) ([]Command, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "api/agent/commands", nil)
	if err != nil {
		return nil, err
	}
	status, _, body, err := c.do(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("poll commands: %w", err)
	}
	if status < 200 || status > 299 {
		return nil, c.redact(decodeProblem(status, body))
	}
	var cmds []Command
	if err := decodeData(body, &cmds); err != nil {
		return nil, fmt.Errorf("decode commands: %w", err)
	}
	if len(cmds) > 100 {
		return nil, errors.New("command response exceeds 100 commands")
	}
	return cmds, nil
}

// Ack acknowledges a command with a terminal status.
func (c Client) Ack(ctx context.Context, id string, ack CommandAck) error {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/?#%") {
		return errors.New("invalid command ID")
	}
	req, err := c.newRequest(ctx, http.MethodPost, "api/agent/commands/"+id+"/ack", ack)
	if err != nil {
		return err
	}
	status, _, body, err := c.do(ctx, req)
	if err != nil {
		return fmt.Errorf("ack command %s: %w", id, err)
	}
	if status < 200 || status > 299 {
		return c.redact(decodeProblem(status, body))
	}
	return nil
}

// decodeData unwraps the success envelope and decodes its data field.
func decodeData(body []byte, out any) error {
	var env successEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return err
	}
	if !env.Success {
		return errors.New("control response is not a success envelope")
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return err
	}
	return nil
}

// decodeProblem turns a non-2xx body into an error without echoing secrets.
func decodeProblem(status int, body []byte) error {
	var p problem
	if err := json.Unmarshal(body, &p); err != nil {
		return fmt.Errorf("request failed with status %d", status)
	}
	detail := p.Detail
	if detail == "" {
		detail = p.Title
	}
	if detail == "" {
		return fmt.Errorf("request failed with status %d", status)
	}
	return fmt.Errorf("request failed: %s", detail)
}

// redact prevents a center or transport error from echoing the node credential.
func (c Client) redact(err error) error {
	if c.Credential == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), c.Credential, "[redacted]"))
}
