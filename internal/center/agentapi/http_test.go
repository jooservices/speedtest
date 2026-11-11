package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeStore struct {
	err       error
	duplicate bool
	config    map[string]any
	saved     int
}

func (s *fakeStore) SaveResult(context.Context, string, Result) (bool, error) {
	s.saved++
	return s.duplicate, s.err
}
func (s *fakeStore) Config(context.Context, string) (map[string]any, error) {
	if s.config == nil {
		s.config = map[string]any{"revision": "1", "name": "edge"}
	}
	return s.config, s.err
}
func (s *fakeStore) Heartbeat(context.Context, string, Heartbeat) (time.Time, error) {
	return time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC), s.err
}
func (s *fakeStore) Commands(context.Context, string) ([]Command, error) { return []Command{}, s.err }
func (s *fakeStore) Ack(context.Context, string, string, Ack) (Command, error) {
	return Command{Status: "running"}, s.err
}
func request(store Store, path, data string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(Method(path), path, strings.NewReader(data))
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	w.Header().Set("X-Request-ID", "trace123")
	(Handler{Store: store}).ServeNode(w, r, "node-1", "203.0.113.8")
	return w
}
func TestBatchOutcomes(t *testing.T) {
	store := &fakeStore{}
	data := `{"results":[` + validResult + `,{"id":"00000000-0000-4000-8000-000000000002"},{"id":"bad"},null,7]}`
	w := request(store, "/api/agent/results", data, nil)
	var response struct {
		Data []Outcome `json:"data"`
	}
	if w.Code != 207 || json.Unmarshal(w.Body.Bytes(), &response) != nil || len(response.Data) != 5 || store.saved != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	if response.Data[0].Status != 201 || response.Data[0].ID == nil || *response.Data[0].ID != "00000000-0000-4000-8000-000000000001" ||
		response.Data[1].Status != 422 || response.Data[1].ID == nil || response.Data[2].ID != nil || response.Data[3].ID != nil || response.Data[4].ID != nil {
		t.Fatal(response)
	}
	for i, outcome := range response.Data {
		if outcome.Index != i {
			t.Fatal(outcome)
		}
	}
	upper := strings.Replace(validResult, "00000000-0000-4000-8000-000000000001", "ABCDEF00-0000-4000-8000-000000000001", 1)
	w = request(&fakeStore{}, "/api/agent/results", `{"results":[`+upper+`]}`, nil)
	_ = json.Unmarshal(w.Body.Bytes(), &response)
	if w.Code != 207 || response.Data[0].ID == nil || *response.Data[0].ID != "abcdef00-0000-4000-8000-000000000001" {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, tc := range []struct {
		err       error
		duplicate bool
		status    int
		outcome   string
	}{{nil, true, 200, "duplicate"}, {ErrConflict, false, 409, "rejected"}} {
		w = request(&fakeStore{err: tc.err, duplicate: tc.duplicate}, "/api/agent/results", `{"results":[`+validResult+`]}`, nil)
		_ = json.Unmarshal(w.Body.Bytes(), &response)
		if w.Code != 207 || response.Data[0].Status != tc.status || response.Data[0].Outcome != tc.outcome {
			t.Fatal(w.Body.String())
		}
	}
	w = request(&fakeStore{err: errors.New("secret SQL detail")}, "/api/agent/results", `{"results":[`+validResult+`]}`, nil)
	if w.Code != 500 || strings.Contains(w.Body.String(), "secret") {
		t.Fatal(w.Body.String())
	}
	for _, data := range []string{`null`, `[]`, `{}`, `{"results":null}`, `{"results":[]}`, `{"results":[null],"extra":1}`} {
		if w = request(store, "/api/agent/results", data, nil); w.Code != 422 {
			t.Fatal(data, w.Code)
		}
	}
	if w = request(store, "/api/agent/results", `{`, nil); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w = request(store, "/api/agent/results", `{"results":[`+strings.TrimSuffix(strings.Repeat(`null,`, 101), ",")+`]}`, nil); w.Code != 413 {
		t.Fatal(w.Code)
	}
	if w = request(store, "/api/agent/results", `{"results":["`+strings.Repeat("x", 8<<20)+`"]}`, nil); w.Code != 413 {
		t.Fatal(w.Code)
	}
}
func TestConfigConditionalRequests(t *testing.T) {
	store := &fakeStore{}
	first := request(store, "/api/agent/config", "", nil)
	etag := first.Header().Get("ETag")
	if first.Code != 200 || etag == "" || first.Header().Get("Cache-Control") != "private, no-cache" {
		t.Fatal(first)
	}
	for _, match := range []string{etag, "W/" + etag, `"other", ` + etag, "*"} {
		w := request(store, "/api/agent/config", "", map[string]string{"If-None-Match": match})
		if w.Code != 304 || w.Body.Len() != 0 || w.Header().Get("ETag") != etag {
			t.Fatal(w)
		}
	}
	store.config["name"] = "renamed"
	w := request(store, "/api/agent/config", "", map[string]string{"If-None-Match": etag})
	if w.Code != 200 || w.Header().Get("ETag") == etag {
		t.Fatal(w)
	}
	store.config["bad"] = make(chan int)
	if w = request(store, "/api/agent/config", "", nil); w.Code != 500 {
		t.Fatal(w)
	}
}
func TestOperationsAndErrors(t *testing.T) {
	ackPath := "/api/agent/commands/00000000-0000-4000-8000-000000000001/ack"
	for _, tc := range []struct{ path, body string }{{"/api/agent/heartbeat", `{"agent":"v1","uptime":1,"lastTest":null}`}, {"/api/agent/commands", ""}, {ackPath, `{"status":"running"}`}, {"/api/agent/ip", ""}, {"/api/agent/config", ""}} {
		if w := request(&fakeStore{}, tc.path, tc.body, nil); w.Code != 200 {
			t.Fatal(tc, w.Code, w.Body.String())
		}
		if w := request(nil, tc.path, tc.body, nil); w.Code != 503 {
			t.Fatal(w)
		}
		if tc.path == "/api/agent/ip" {
			continue
		}
		for _, testErr := range []error{ErrInvalid, ErrConflict, ErrNotFound, errors.New("unsafe database error")} {
			w := request(&fakeStore{err: testErr}, tc.path, tc.body, nil)
			want := map[error]int{ErrInvalid: 422, ErrConflict: 409, ErrNotFound: 404}[testErr]
			if want == 0 {
				want = 500
			}
			if w.Code != want || strings.Contains(w.Body.String(), "unsafe") || w.Header().Get("Content-Type") != "application/problem+json" {
				t.Fatal(w.Code, w.Body.String())
			}
		}
	}
	for _, path := range []string{"/api/agent/heartbeat", ackPath} {
		for _, data := range []string{`{`, `{}`, strings.Repeat("x", 65<<10)} {
			w := request(&fakeStore{}, path, data, nil)
			if w.Code != 400 && w.Code != 422 && w.Code != 413 {
				t.Fatal(w.Code)
			}
		}
	}
	if w := request(&fakeStore{}, "/api/agent/commands/invalid/ack", `{}`, nil); w.Code != 404 {
		t.Fatal(w)
	}
	if w := request(&fakeStore{}, "/api/agent/unknown", "", nil); w.Code != 404 {
		t.Fatal(w)
	}
	for _, path := range []string{"/api/agent/commands//ack", "/api/agent/commands/1/ack/extra", "/api/agent/unknown"} {
		if Method(path) != "" {
			t.Fatal(path)
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Body = io.NopCloser(brokenReader{})
	if _, ok := body(w, r, 100); ok || w.Code != 400 {
		t.Fatal(w)
	}
}

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestInvalidItemIdentityIsExactCase(t *testing.T) {
	w := request(&fakeStore{}, "/api/agent/results", `{"results":[{"ID":"00000000-0000-4000-8000-000000000001"}]}`, nil)
	var response struct {
		Data []Outcome `json:"data"`
	}
	if json.Unmarshal(w.Body.Bytes(), &response) != nil || w.Code != 207 || response.Data[0].ID != nil || response.Data[0].Status != 422 {
		t.Fatal(w.Body.String())
	}
}

func TestIngestionObserverCountsCommittedItemOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		store            *fakeStore
		code             int
	}{
		{"accepted", `{"results":[` + validResult + `]}`, "accepted", &fakeStore{}, 207},
		{"duplicate", `{"results":[` + validResult + `]}`, "duplicate", &fakeStore{duplicate: true}, 207},
		{"invalid item", `{"results":[{}]}`, "rejected", &fakeStore{}, 207},
		{"conflict", `{"results":[` + validResult + `]}`, "rejected", &fakeStore{err: ErrConflict}, 207},
		{"store error", `{"results":[` + validResult + `]}`, "rejected", &fakeStore{err: errors.New("failure")}, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var outcomes []string
			r := httptest.NewRequest("POST", "/api/agent/results", strings.NewReader(tc.body))
			r = r.WithContext(WithIngestObserver(r.Context(), func(status string) { outcomes = append(outcomes, status) }))
			w := httptest.NewRecorder()
			(Handler{Store: tc.store}).ServeNode(w, r, "node", "203.0.113.1")
			if w.Code != tc.code || len(outcomes) != 1 || outcomes[0] != tc.want {
				t.Fatal(w.Code, outcomes)
			}
		})
	}
}
