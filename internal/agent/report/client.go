package report

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// maxResponseBody bounds how much of a center response is read into memory.
const maxResponseBody = 1 << 20
const maxBatchBytes = 8 << 20

// Outcome is one per-item result of a batch submission, mirroring BatchOutcome
// in docs/openapi.yaml.
type Outcome struct {
	Index   int      `json:"index"`
	ID      *string  `json:"id"`
	Outcome string   `json:"outcome"` // accepted | duplicate | rejected
	Status  int      `json:"status"`
	Problem *Problem `json:"problem"`
}

// Problem is an RFC 9457 error body returned for a rejected item.
type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

// SendError describes a failed batch submission. Retryable failures (transport
// errors, 429 and 5xx) must be retried with backoff; non-retryable failures
// (bad credential, malformed batch) must not loop.
type SendError struct {
	Retryable bool
	Status    int
	Err       error
}

func (e *SendError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("report: center returned status %d: %v", e.Status, e.Err)
	}
	return "report: " + e.Err.Error()
}

func (e *SendError) Unwrap() error { return e.Err }

// Retryable reports whether a SendBatch error is a transient failure that
// backoff should retry.
func Retryable(err error) bool {
	var se *SendError
	return errors.As(err, &se) && se.Retryable
}

// Sender submits one batch of results to the command center.
type Sender interface {
	SendBatch(context.Context, []Result) ([]Outcome, error)
}

// Client sends result batches to the command center's POST /api/agent/results
// endpoint with the node bearer credential. It never logs the credential.
type Client struct {
	BaseURL    *url.URL
	Credential string
	HTTP       *http.Client
}

var _ Sender = (*Client)(nil)

// SendBatch posts results to the center and returns the per-item outcomes. A
// nil error means every item has a terminal outcome (accepted, duplicate or
// rejected); a Retryable SendError means the batch must be resent unchanged.
func (c *Client) SendBatch(ctx context.Context, results []Result) ([]Outcome, error) {
	if c.BaseURL == nil || (c.BaseURL.Scheme != "http" && c.BaseURL.Scheme != "https") || c.BaseURL.Host == "" || c.BaseURL.User != nil {
		return nil, &SendError{Err: errors.New("center URL is required")}
	}
	if len(results) == 0 || len(results) > DefaultBatchSize {
		return nil, &SendError{Err: errors.New("batch must contain 1..100 results")}
	}
	body, err := json.Marshal(struct {
		Results []Result `json:"results"`
	}{Results: results})
	if err != nil {
		return nil, &SendError{Err: fmt.Errorf("encode batch: %w", err)}
	}

	if len(body) > maxBatchBytes {
		return nil, &SendError{Err: errors.New("batch exceeds 8 MiB")}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(), bytes.NewReader(body))
	if err != nil {
		return nil, &SendError{Err: fmt.Errorf("build request: %w", err)}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.Credential)

	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	safeClient := *client
	safeClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := safeClient.Do(httpReq)
	if err != nil {
		return nil, &SendError{Retryable: true, Err: errors.New("result transport failed")}
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if err != nil {
		return nil, &SendError{Retryable: true, Err: fmt.Errorf("read response: %w", err)}
	}

	if len(respBody) > maxResponseBody {
		return nil, &SendError{Retryable: true, Err: errors.New("center response too large")}
	}
	switch {
	case resp.StatusCode == http.StatusMultiStatus:
		out, err := decodeOutcomes(respBody)
		if err != nil {
			return nil, err
		}
		if _, err = validateOutcomes(results, out); err != nil {
			return nil, &SendError{Retryable: true, Err: err}
		}
		return out, nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, &SendError{Status: resp.StatusCode, Err: errors.New("node credential rejected")}
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusRequestEntityTooLarge ||
		resp.StatusCode == http.StatusUnprocessableEntity:
		return nil, &SendError{Status: resp.StatusCode, Err: errors.New("center rejected the batch as malformed")}
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return nil, &SendError{Retryable: true, Status: resp.StatusCode, Err: fmt.Errorf("center unavailable (status %d)", resp.StatusCode)}
	default:
		return nil, &SendError{Status: resp.StatusCode, Err: fmt.Errorf("unexpected status %d", resp.StatusCode)}
	}
}

func (c *Client) endpoint() string {
	return c.BaseURL.JoinPath("api", "agent", "results").String()
}

// decodeOutcomes parses the 207 multi-status envelope into ordered outcomes.
func decodeOutcomes(body []byte) ([]Outcome, error) {
	var env struct {
		Success bool            `json:"success"`
		Code    int             `json:"code"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, &SendError{Retryable: true, Status: http.StatusMultiStatus, Err: fmt.Errorf("decode response: %w", err)}
	}
	if !env.Success || env.Code != 207 {
		return nil, &SendError{Retryable: true, Err: errors.New("invalid batch envelope")}
	}
	var outcomes []Outcome
	if err := json.Unmarshal(env.Data, &outcomes); err != nil {
		return nil, &SendError{Retryable: true, Status: http.StatusMultiStatus, Err: fmt.Errorf("decode outcomes: %w", err)}
	}
	if outcomes == nil {
		outcomes = []Outcome{}
	}
	return outcomes, nil
}
