package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jooservices/speedtest/internal/center/apicontract"
	"github.com/jooservices/speedtest/internal/center/apitoken"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func closeSession(t *testing.T, session *mcp.ClientSession) {
	t.Helper()
	if err := session.Close(); err != nil {
		t.Errorf("close MCP session: %v", err)
	}
}

func TestStreamableHTTPExposesToolsByTokenScopeAndDispatchesAPI(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := apitoken.PrincipalFromContext(r.Context()); !ok {
			t.Fatal("tool dispatch did not carry its verified principal")
		}
		if r.Method != http.MethodGet || r.URL.Path != "/api/nodes" {
			t.Errorf("tool request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":[{"id":"node-1"}]}`))
	})
	principal := apitoken.Principal{TokenID: "token-1", UserID: "user-1", Scope: apitoken.ReadOnly}
	handler := NewHandler(api)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(apitoken.WithPrincipal(r.Context(), principal)))
	}))
	defer server.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: server.URL, HTTPClient: server.Client()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeSession(t, session)
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 7 {
		t.Fatalf("read-only token sees %d tools, want 7", len(listed.Tools))
	}
	for _, tool := range listed.Tools {
		if tool.Name == "run_test" || tool.Name == "update_schedule" || tool.Name == "acknowledge_alert" {
			t.Errorf("read-only token sees write tool %q", tool.Name)
		}
	}
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_nodes", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("list_nodes returned tool error: %+v", result.Content)
	}
	if result.StructuredContent == nil {
		t.Fatal("list_nodes returned no structured result")
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil || string(encoded) != `{"data":[{"id":"node-1"}],"success":true}` {
		t.Fatalf("structured result = %s, error = %v", encoded, err)
	}
}

func TestCallOperationRejectsUnknownAndMissingArguments(t *testing.T) {
	operation := structOperation()
	api := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("invalid arguments reached API") })
	for _, args := range []string{`{"unexpected":true}`, `{}`} {
		result, err := callOperation(context.Background(), api, apitoken.Principal{}, operation, json.RawMessage(args))
		if err != nil || result == nil || !result.IsError {
			t.Errorf("callOperation(%s) = (%+v, %v), want tool error", args, result, err)
		}
	}
	for _, raw := range []json.RawMessage{json.RawMessage(`{`), json.RawMessage(`null`)} {
		result, err := callOperation(context.Background(), api, apitoken.Principal{}, operation, raw)
		if err != nil || result == nil || !result.IsError {
			t.Errorf("callOperation(%s) = (%+v, %v), want JSON object error", raw, result, err)
		}
	}
}

func TestOperationRequestBuildsPathQueryAndBody(t *testing.T) {
	operation := apicontract.Operation{Method: http.MethodPost, Path: "/api/nodes/{id}", Parameters: []apicontract.Parameter{
		{Name: "id", In: "path", Required: true}, {Name: "labels", In: "query"},
	}, BodySchema: map[string]any{"properties": map[string]any{"name": map[string]any{"type": "string"}}}}
	request, err := operationRequest(context.Background(), operation, map[string]any{"id": "node-1", "labels": []any{"west", "fast"}, "name": "test"})
	if err != nil || request.URL.Path != "/api/nodes/node-1" || request.URL.Query().Get("labels") != "west,fast" {
		t.Fatalf("request = %+v, error = %v", request, err)
	}
	if request.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("request content type = %q", request.Header.Get("Content-Type"))
	}
	var body map[string]string
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body["name"] != "test" {
		t.Fatalf("request body = %+v, error = %v", body, err)
	}
	if _, err := operationRequest(context.Background(), operation, map[string]any{"id": "ok", "extra": true}); err == nil {
		t.Fatal("unknown argument was accepted")
	}
	if _, err := operationRequest(context.Background(), operation, map[string]any{}); err == nil {
		t.Fatal("missing path parameter was accepted")
	}
}

func TestOperationRequestHandlesBodyAndHTTPConstructionErrors(t *testing.T) {
	if body, err := requestBody(nil, nil, nil); err != nil || body != nil {
		t.Fatalf("empty request body = %q, %v", body, err)
	}
	if _, err := requestBody(map[string]any{}, map[string]any{"bad": true}, map[string]any{"bad": make(chan int)}); err == nil {
		t.Fatal("unsupported JSON body value was accepted")
	}
	if _, err := operationRequest(context.Background(), apicontract.Operation{Method: "BAD METHOD", Path: "/api/nodes"}, nil); err == nil {
		t.Fatal("invalid HTTP method was accepted")
	}
	if got := queryValue("value"); got != "value" {
		t.Fatalf("scalar query value = %q", got)
	}
}

func TestNewHandlerRequiresPrincipalAndFullScopeGetsWriteToolsAndDocs(t *testing.T) {
	denied := httptest.NewRecorder()
	NewHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(denied, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if denied.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated MCP status = %d", denied.Code)
	}
	api := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal := apitoken.Principal{TokenID: "full", UserID: "owner", Scope: apitoken.Full}
		NewHandler(api).ServeHTTP(w, r.WithContext(apitoken.WithPrincipal(r.Context(), principal)))
	}))
	defer server.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "full-client", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: server.URL, HTTPClient: server.Client()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeSession(t, session)
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil || len(listed.Tools) != 10 {
		t.Fatalf("full-scope tool count = %d, error = %v", len(listed.Tools), err)
	}
	resource, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "speedtest://docs/metrics"})
	if err != nil || len(resource.Contents) != 1 || !strings.Contains(resource.Contents[0].Text, "Mbps") {
		t.Fatalf("metrics resource = %+v, error = %v", resource, err)
	}
}

func TestResponseResultHandlesInvalidJSONAndErrors(t *testing.T) {
	response := newResponseRecorder()
	response.WriteHeader(http.StatusBadGateway)
	_, _ = response.Write([]byte("not-json"))
	result := responseResult(response)
	if !result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "502") {
		t.Fatalf("invalid API result = %+v", result)
	}
}

func TestAddToolSkipsUnserializableInputSchema(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	addTool(server, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), apitoken.Principal{}, apicontract.Operation{
		Tool: "bad_schema", BodySchema: map[string]any{"properties": map[string]any{"bad": make(chan int)}},
	})
}

func structOperation() apicontract.Operation {
	return apicontract.Operation{Method: http.MethodGet, Path: "/api/nodes/{id}", Parameters: []apicontract.Parameter{{Name: "id", In: "path", Required: true}}}
}
