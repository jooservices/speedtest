package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/agentapi"
)

type stubAgentStore struct{}

func (stubAgentStore) SaveResult(context.Context, string, agentapi.Result) (bool, error) {
	return false, nil
}
func (stubAgentStore) Config(context.Context, string) (map[string]any, error) {
	return map[string]any{"name": "only-own-node"}, nil
}
func (stubAgentStore) Heartbeat(context.Context, string, agentapi.Heartbeat) (time.Time, error) {
	return time.Now(), nil
}
func (stubAgentStore) Commands(context.Context, string) ([]agentapi.Command, error) {
	return []agentapi.Command{}, nil
}
func (stubAgentStore) Ack(context.Context, string, string, agentapi.Ack) (agentapi.Command, error) {
	return agentapi.Command{}, nil
}
func TestAgentRoutesUseBearerAndCorrectMethods(t *testing.T) {
	service := &fakeNodeCredentialService{}
	h := NewHandler(Options{NodeCredentials: service, AgentAPI: stubAgentStore{}, TrustedProxyCIDRs: "127.0.0.0/8"})
	paths := []struct{ path, method, body string }{{"/api/agent/config", "GET", ""}, {"/api/agent/ip", "GET", ""}, {"/api/agent/commands", "GET", ""}, {"/api/agent/heartbeat", "POST", `{"agent":"1","uptime":1,"lastTest":null}`}, {"/api/agent/commands/00000000-0000-4000-8000-000000000001/ack", "POST", `{"status":"running"}`}, {"/api/agent/results", "POST", `{"results":[null]}`}}
	for _, tc := range paths {
		t.Run(tc.path, func(t *testing.T) {
			for _, token := range []string{"", "wrong", "revoked", "good"} {
				r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
				r.Header.Set("Authorization", "Bearer "+token)
				r.RemoteAddr = "127.0.0.1:1234"
				r.Header.Set("X-Forwarded-For", "203.0.113.10")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				want := 401
				if token == "good" {
					want = 200
					if tc.path == "/api/agent/results" {
						want = 207
					}
				}
				if w.Code != want || w.Header().Get("X-Request-ID") == "" {
					t.Fatal(w.Code, w.Body.String())
				}
				if tc.path == "/api/agent/ip" && token == "good" && !strings.Contains(w.Body.String(), "203.0.113.10") {
					t.Fatal(w.Body.String())
				}
			}
			r := httptest.NewRequest(http.MethodDelete, tc.path, nil)
			r.Header.Set("Authorization", "Bearer good")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 405 || w.Header().Get("Allow") != tc.method {
				t.Fatal(w.Code, w.Header())
			}
		})
	}
}
