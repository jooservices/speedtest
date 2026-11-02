package testutil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// envelope is the shared success envelope used to extract data from responses.
type envelope struct {
	Success bool            `json:"success"`
	Code    int             `json:"code"`
	Data    json.RawMessage `json:"data"`
}

// postJSON sends a JSON POST to the double and returns the status code and
// response body.
func postJSON(t *testing.T, url, bearer string, body any) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode request: %v", err)
		}
	}
	req, err := http.NewRequest(http.MethodPost, url, &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var got bytes.Buffer
	if _, err := got.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode, got.Bytes()
}

// enroll posts a valid enrollment and returns the issued credential.
func enroll(t *testing.T, c *Center) string {
	t.Helper()
	code, body := postJSON(t, c.URL+"/api/agent/enroll", "", EnrollRequest{
		Token: "test-enrollment-token",
		Name:  "viettel-fast-50",
		Host:  "node.example.internal",
		Agent: "speedtest-agent/0.1.0",
	})
	if code != http.StatusCreated {
		t.Fatalf("enroll status = %d, want 201; body %s", code, body)
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("enroll response not an envelope: %v", err)
	}
	var enrolled Enrolled
	if err := json.Unmarshal(env.Data, &enrolled); err != nil {
		t.Fatalf("enroll data: %v", err)
	}
	if enrolled.Credential == "" {
		t.Fatal("enroll returned empty credential")
	}
	return enrolled.Credential
}

func newCenter(t *testing.T) *Center {
	t.Helper()
	c := NewCenter()
	t.Cleanup(c.Close)
	return c
}

func TestEnrollRecordsAndReturnsCredential(t *testing.T) {
	c := newCenter(t)

	code, body := postJSON(t, c.URL+"/api/agent/enroll", "", EnrollRequest{
		Token: "test-enrollment-token",
		Name:  "viettel-fast-50",
		Host:  "node.example.internal",
		Agent: "speedtest-agent/0.1.0",
	})

	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", code, body)
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("response not an envelope: %v", err)
	}
	if !env.Success || env.Code != 201 {
		t.Fatalf("envelope = %+v, want success with code 201", env)
	}
	var enrolled Enrolled
	if err := json.Unmarshal(env.Data, &enrolled); err != nil {
		t.Fatalf("enroll data: %v", err)
	}
	if enrolled.NodeID != "test-node-1" {
		t.Fatalf("nodeId = %q, want test-node-1", enrolled.NodeID)
	}
	if enrolled.Credential != "test-node-credential" {
		t.Fatalf("credential = %q, want test-node-credential", enrolled.Credential)
	}
	if enrolled.Config.Name != "viettel-fast-50" {
		t.Fatalf("config name = %q, want viettel-fast-50", enrolled.Config.Name)
	}

	got := c.Enrollments()
	if len(got) != 1 {
		t.Fatalf("Enrollments() = %d entries, want 1", len(got))
	}
	if got[0].Token != "test-enrollment-token" || got[0].Name != "viettel-fast-50" {
		t.Fatalf("Enrollments()[0] = %+v, want the posted request", got[0])
	}
}

func TestEnrollInvalidToken(t *testing.T) {
	c := newCenter(t)

	code, _ := postJSON(t, c.URL+"/api/agent/enroll", "", EnrollRequest{
		Token: "wrong-token",
		Name:  "node",
		Agent: "speedtest-agent/0.1.0",
	})

	if code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", code)
	}
	if n := len(c.Enrollments()); n != 0 {
		t.Fatalf("Enrollments() = %d entries, want 0", n)
	}
}

func TestEnrollMalformedBody(t *testing.T) {
	c := newCenter(t)

	req, _ := http.NewRequest(http.MethodPost, c.URL+"/api/agent/enroll", strings.NewReader("{not-json"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestResultsAcceptedAndRecorded(t *testing.T) {
	c := newCenter(t)
	cred := enroll(t, c)

	batch := ResultBatch{Results: []ResultInput{{
		ID:     "00000000-0000-4000-8000-000000000001",
		TS:     "2026-11-03T07:00:00Z",
		Status: "ok",
		Down:   ptr(300.0),
		Up:     ptr(150.0),
		Ping:   ptr(8.5),
		IP:     "203.0.113.10",
	}}}

	code, body := postJSON(t, c.URL+"/api/agent/results", cred, batch)

	if code != http.StatusMultiStatus {
		t.Fatalf("status = %d, want 207; body %s", code, body)
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("response not an envelope: %v", err)
	}
	var outcomes []BatchOutcome
	if err := json.Unmarshal(env.Data, &outcomes); err != nil {
		t.Fatalf("data: %v", err)
	}
	if len(outcomes) != 1 {
		t.Fatalf("outcomes = %d, want 1", len(outcomes))
	}
	if outcomes[0].Outcome != "accepted" || outcomes[0].Status != http.StatusCreated {
		t.Fatalf("outcomes[0] = %+v, want accepted/201", outcomes[0])
	}

	got := c.Results()
	if len(got) != 1 {
		t.Fatalf("Results() = %d, want 1", len(got))
	}
	if got[0].ID != batch.Results[0].ID || *got[0].Down != 300.0 {
		t.Fatalf("Results()[0] = %+v, want the posted result", got[0])
	}
}

func TestResultsDuplicateReplayed(t *testing.T) {
	c := newCenter(t)
	cred := enroll(t, c)

	batch := ResultBatch{Results: []ResultInput{{
		ID:     "00000000-0000-4000-8000-000000000002",
		TS:     "2026-11-03T07:00:00Z",
		Status: "ok",
		Down:   ptr(300.0),
		Up:     ptr(150.0),
		Ping:   ptr(8.5),
		IP:     "203.0.113.10",
	}}}

	_, _ = postJSON(t, c.URL+"/api/agent/results", cred, batch)
	code, body := postJSON(t, c.URL+"/api/agent/results", cred, batch)

	if code != http.StatusMultiStatus {
		t.Fatalf("status = %d, want 207", code)
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("response not an envelope: %v", err)
	}
	var outcomes []BatchOutcome
	if err := json.Unmarshal(env.Data, &outcomes); err != nil {
		t.Fatalf("data: %v", err)
	}
	if outcomes[0].Outcome != "duplicate" || outcomes[0].Status != http.StatusOK {
		t.Fatalf("outcomes[0] = %+v, want duplicate/200", outcomes[0])
	}
	if n := len(c.Results()); n != 1 {
		t.Fatalf("Results() = %d, want 1 (no second record)", n)
	}
}

func TestResultsUnauthorized(t *testing.T) {
	c := newCenter(t)
	_ = enroll(t, c)

	code, _ := postJSON(t, c.URL+"/api/agent/results", "wrong-credential", ResultBatch{
		Results: []ResultInput{{ID: "00000000-0000-4000-8000-000000000003"}},
	})

	if code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", code)
	}
}

func TestResultsEmptyBatch(t *testing.T) {
	c := newCenter(t)
	cred := enroll(t, c)

	code, _ := postJSON(t, c.URL+"/api/agent/results", cred, ResultBatch{Results: []ResultInput{}})

	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", code)
	}
}

func TestHeartbeatRecordsAndReceipts(t *testing.T) {
	c := newCenter(t)
	cred := enroll(t, c)

	code, body := postJSON(t, c.URL+"/api/agent/heartbeat", cred, Heartbeat{
		Agent:  "speedtest-agent/0.1.0",
		Uptime: 42,
	})

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", code, body)
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("response not an envelope: %v", err)
	}
	var receipt Receipt
	if err := json.Unmarshal(env.Data, &receipt); err != nil {
		t.Fatalf("data: %v", err)
	}
	if _, err := time.Parse(time.RFC3339, receipt.ReceivedAt); err != nil {
		t.Fatalf("receivedAt %q is not RFC 3339: %v", receipt.ReceivedAt, err)
	}

	got := c.Heartbeats()
	if len(got) != 1 {
		t.Fatalf("Heartbeats() = %d, want 1", len(got))
	}
	if got[0].Agent != "speedtest-agent/0.1.0" || got[0].Uptime != 42 {
		t.Fatalf("Heartbeats()[0] = %+v, want the posted heartbeat", got[0])
	}
}

func TestHeartbeatUnauthorized(t *testing.T) {
	c := newCenter(t)
	_ = enroll(t, c)

	code, _ := postJSON(t, c.URL+"/api/agent/heartbeat", "", Heartbeat{Agent: "speedtest-agent/0.1.0"})

	if code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", code)
	}
}

func TestConcurrentRecording(t *testing.T) {
	c := newCenter(t)
	cred := enroll(t, c)

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = postJSON(t, c.URL+"/api/agent/results", cred, ResultBatch{Results: []ResultInput{{
				ID:     fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1),
				TS:     "2026-11-03T07:00:00Z",
				Status: "ok",
				Down:   ptr(300.0),
				Up:     ptr(150.0),
				Ping:   ptr(8.5),
				IP:     "203.0.113.10",
			}}})
			_, _ = postJSON(t, c.URL+"/api/agent/heartbeat", cred, Heartbeat{Agent: "speedtest-agent/0.1.0", Uptime: int64(i)})
		}(i)
	}
	wg.Wait()

	if got := len(c.Results()); got != n {
		t.Fatalf("Results() = %d, want %d", got, n)
	}
	if got := len(c.Heartbeats()); got != n {
		t.Fatalf("Heartbeats() = %d, want %d", got, n)
	}
}

func ptr[T any](v T) *T { return &v }

func TestOptionsAndMalformedBodies(t *testing.T) {
	c := NewCenter(WithEnrollmentToken("custom-token"), WithCredential("custom-credential"), WithNodeID("custom-node"))
	t.Cleanup(c.Close)
	code, body := postJSON(t, c.URL+"/api/agent/enroll", "", EnrollRequest{Token: "custom-token", Name: "node"})
	if code != 201 || !bytes.Contains(body, []byte(`"nodeId":"custom-node"`)) || !bytes.Contains(body, []byte(`"credential":"custom-credential"`)) {
		t.Fatalf("enroll: %d %s", code, body)
	}
	for _, endpoint := range []string{"results", "heartbeat"} {
		code, _ := postJSON(t, c.URL+"/api/agent/"+endpoint, "custom-credential", nil)
		if code != 400 {
			t.Fatalf("%s malformed: %d", endpoint, code)
		}
		code, _ = postJSON(t, c.URL+"/api/agent/"+endpoint, "wrong", nil)
		if code != 401 {
			t.Fatalf("%s wrong credential: %d", endpoint, code)
		}
	}
	if len(c.Results()) != 0 || len(c.Heartbeats()) != 0 {
		t.Fatal("rejected requests were recorded")
	}
	if nullableID("") != nil {
		t.Fatal("empty id should be null")
	}
}

func TestBatchLimitAndSliceSnapshots(t *testing.T) {
	c := newCenter(t)
	cred := enroll(t, c)
	items := make([]ResultInput, 100)
	for i := range items {
		items[i].ID = fmt.Sprintf("id-%d", i)
	}
	code, _ := postJSON(t, c.URL+"/api/agent/results", cred, ResultBatch{Results: items})
	if code != 207 || len(c.Results()) != 100 {
		t.Fatalf("maximum batch: %d", code)
	}
	code, _ = postJSON(t, c.URL+"/api/agent/results", cred, ResultBatch{Results: append(items, ResultInput{ID: "extra"})})
	if code != 422 || len(c.Results()) != 100 {
		t.Fatalf("oversize batch: %d", code)
	}
	snapshot := c.Results()
	snapshot[0].ID = "changed"
	if c.Results()[0].ID != "id-0" {
		t.Fatal("result slice aliases recorded entries")
	}
	enrollments := c.Enrollments()
	enrollments[0].Name = "changed"
	if c.Enrollments()[0].Name == "changed" {
		t.Fatal("enrollment slice aliases recorded entries")
	}
	code, _ = postJSON(t, c.URL+"/api/agent/heartbeat", cred, Heartbeat{Agent: "original"})
	if code != 200 {
		t.Fatal(code)
	}
	heartbeats := c.Heartbeats()
	heartbeats[0].Agent = "changed"
	if c.Heartbeats()[0].Agent != "original" {
		t.Fatal("heartbeat slice aliases recorded entries")
	}
}
