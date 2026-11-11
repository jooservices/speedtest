package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
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
		if r.RemoteAddr != "127.0.0.1:443" || r.Header.Get("X-Forwarded-For") != "198.51.100.44" {
			t.Errorf("tool request lost incoming client address: remote=%q forwarded=%q", r.RemoteAddr, r.Header.Get("X-Forwarded-For"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":[{"id":"node-1"}]}`))
	})
	principal := apitoken.Principal{TokenID: "token-1", UserID: "user-1", Scope: apitoken.ReadOnly}
	handler := NewHandler(api)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = "127.0.0.1:443"
		r.Header.Set("X-Forwarded-For", "198.51.100.44")
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
	if len(listed.Tools) != 8 {
		t.Fatalf("read-only token sees %d tools, want 8", len(listed.Tools))
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

func TestMCPClientPreservesFiltersConditionalWritesAndScheduleFormats(t *testing.T) {
	api := &mcpContractTestAPI{t: t}
	session := connectTestClient(t, api, apitoken.Principal{TokenID: "writer", UserID: "owner", Scope: apitoken.ReadWrite})
	verifyMCPResultPages(t, session, api)
	verifyMCPScheduleFormats(t, session, api)
	verifyMCPAlertAcknowledgement(t, session)
}

func TestOperationRequestRejectsInvalidMethod(t *testing.T) {
	_, err := operationRequest(context.Background(), apicontract.Operation{Method: "bad method", Path: "/api/nodes"}, map[string]any{})
	if err == nil {
		t.Fatal("invalid HTTP method was accepted")
	}
	_, err = operationRequest(context.Background(), apicontract.Operation{
		Method: http.MethodPut, Path: "/api/schedules/node-1", BodySchema: map[string]any{"oneOf": []any{
			map[string]any{"properties": map[string]any{"interval": map[string]any{}}, "required": []any{"interval"}},
			map[string]any{"properties": map[string]any{"engines": map[string]any{}}, "required": []any{"engines"}},
		}},
	}, map[string]any{})
	if err == nil {
		t.Fatal("request with no matching body format was accepted")
	}
}

func TestRequestBodyRejectsInvalidFormatsAndNonJSONValues(t *testing.T) {
	field := map[string]any{"type": "integer"}
	format := func(required string) map[string]any {
		return map[string]any{
			"type": "object", "properties": map[string]any{required: field}, "required": []any{required},
		}
	}
	for _, test := range []struct {
		name   string
		schema map[string]any
		args   map[string]any
	}{
		{"no matching format", map[string]any{"oneOf": []any{format("interval"), format("engines")}}, map[string]any{}},
		{"ambiguous format", map[string]any{"oneOf": []any{format("interval"), format("interval")}}, map[string]any{"interval": 30}},
		{"unmarshalable value", map[string]any{"properties": map[string]any{"value": field}}, map[string]any{"value": make(chan int)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := requestBody(test.schema, test.args); err == nil {
				t.Fatal("invalid request body was accepted")
			}
		})
	}
}

func TestCallOperationRejectsMalformedAndNonObjectArguments(t *testing.T) {
	api := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("invalid arguments reached API") })
	for _, raw := range []json.RawMessage{[]byte("{"), []byte("null"), []byte("[]")} {
		result, err := callOperation(context.Background(), api, apitoken.Principal{}, apicontract.Operation{}, raw, requestSource{})
		if err != nil || !result.IsError {
			t.Errorf("callOperation(%q) = %+v, error=%v; want tool error", raw, result, err)
		}
	}
}

func connectTestClient(t *testing.T, api http.Handler, principal apitoken.Principal) *mcp.ClientSession {
	t.Helper()
	handler := NewHandler(api)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(apitoken.WithPrincipal(r.Context(), principal)))
	}))
	t.Cleanup(server.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "mcp-contract-client", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: server.URL, HTTPClient: server.Client()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeSession(t, session) })
	return session
}

func verifyMCPResultPages(t *testing.T, session *mcp.ClientSession, api *mcpContractTestAPI) {
	t.Helper()
	for page := 1; page <= 2; page++ {
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_results", Arguments: map[string]any{
			"engine": "ookla", "nodes": []any{"node-1"}, "range": "7d", "status": "ok", "page": page, "size": 1, "sort": "ts", "dir": "desc",
		}})
		if err != nil || result.IsError {
			t.Fatalf("list_results page %d = %+v, error=%v", page, result, err)
		}
	}
	if len(api.resultPages) != 2 || api.resultPages[0] != "1" || api.resultPages[1] != "2" {
		t.Fatalf("MCP pages reached API as %v, want [1 2]", api.resultPages)
	}
}

func verifyMCPScheduleFormats(t *testing.T, session *mcp.ClientSession, api *mcpContractTestAPI) {
	t.Helper()
	for _, args := range []map[string]any{
		{"interval": 45, "offset": 5, "enabled": true},
		{"engines": []any{map[string]any{"engine": "ookla", "interval": 60, "offset": 3, "enabled": true}}},
	} {
		current, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_schedule", Arguments: map[string]any{"node": "node-1"}})
		if err != nil || current.IsError {
			t.Fatalf("get_schedule = %+v, error=%v", current, err)
		}
		arguments := map[string]any{"node": "node-1", "If-Match": responseETag(t, current)}
		for key, value := range args {
			arguments[key] = value
		}
		updated, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "update_schedule", Arguments: arguments})
		if err != nil || updated.IsError {
			t.Fatalf("update_schedule args=%v = %+v, error=%v", args, updated, err)
		}
	}
	if len(api.scheduleFormats) != 2 || api.scheduleFormats[0] != "schedule" || api.scheduleFormats[1] != "engines" {
		t.Fatalf("MCP schedule formats reached API as %v", api.scheduleFormats)
	}
	stale, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "update_schedule", Arguments: map[string]any{"node": "node-1", "If-Match": `"schedule-v1"`, "interval": 30, "offset": 1, "enabled": true}})
	if err != nil || !stale.IsError {
		t.Fatalf("stale schedule update = %+v, error=%v", stale, err)
	}
}

func verifyMCPAlertAcknowledgement(t *testing.T, session *mcp.ClientSession) {
	t.Helper()
	current, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_alert", Arguments: map[string]any{"id": "alert-1"}})
	if err != nil || current.IsError {
		t.Fatalf("get_alert = %+v, error=%v", current, err)
	}
	updated, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "acknowledge_alert", Arguments: map[string]any{"id": "alert-1", "If-Match": responseETag(t, current), "acked": true}})
	if err != nil || updated.IsError || responseETag(t, updated) != `"alert-v2"` {
		t.Fatalf("acknowledge_alert = %+v, error=%v", updated, err)
	}
	stale, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "acknowledge_alert", Arguments: map[string]any{"id": "alert-1", "If-Match": `"alert-v1"`, "acked": false}})
	if err != nil || !stale.IsError {
		t.Fatalf("stale acknowledge_alert = %+v, error=%v", stale, err)
	}
}

func responseETag(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	httpMeta, ok := result.Meta["http"].(map[string]any)
	if !ok {
		return responseEnvelopeETag(t, result)
	}
	headers, ok := httpMeta["headers"].(map[string]any)
	if !ok {
		return responseEnvelopeETag(t, result)
	}
	etag, _ := headers["ETag"].(string)
	if etag == "" {
		return responseEnvelopeETag(t, result)
	}
	return etag
}

func responseEnvelopeETag(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	envelope, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("MCP result has no structured API envelope: %#v", result.StructuredContent)
	}
	meta, ok := envelope["meta"].(map[string]any)
	if !ok {
		t.Fatalf("MCP result has no envelope metadata: %#v", envelope)
	}
	etag, _ := meta["etag"].(string)
	if etag == "" {
		t.Fatalf("MCP result has no ETag in envelope metadata: %#v", meta)
	}
	return etag
}

type mcpContractTestAPI struct {
	t                 *testing.T
	scheduleVersion   int
	alertVersion      int
	resultPages       []string
	scheduleFormats   []string
	alertAcknowledged bool
}

func (api *mcpContractTestAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/results":
		api.handleResults(w, r)
	case "/api/schedules/node-1":
		api.handleSchedule(w, r)
	case "/api/alerts/events/alert-1":
		api.handleAlert(w, r)
	case "/api/alerts/events/alert-1/ack":
		api.handleAcknowledgement(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (api *mcpContractTestAPI) handleResults(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if query.Get("engine") != "ookla" || query.Get("nodes") != "node-1" || query.Get("range") != "7d" ||
		query.Get("status") != "ok" || query.Get("size") != "1" || query.Get("sort") != "ts" || query.Get("dir") != "desc" {
		api.t.Errorf("list_results query lost filters: %v", query)
	}
	api.resultPages = append(api.resultPages, query.Get("page"))
	writeMCPJSON(w, http.StatusOK, map[string]any{"success": true, "data": []any{map[string]any{"id": "result-" + query.Get("page")}}})
}

func (api *mcpContractTestAPI) handleSchedule(w http.ResponseWriter, r *http.Request) {
	etag := fmt.Sprintf(`"schedule-v%d"`, api.scheduleVersion+1)
	if r.Method == http.MethodGet {
		w.Header().Set("ETag", fmt.Sprintf(`"schedule-v%d"`, api.scheduleVersion+1))
		writeMCPJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"interval": 30}})
		return
	}
	if r.Header.Get("If-Match") != etag || r.Header.Get("X-CSRF-Token") != "" {
		writeMCPJSON(w, http.StatusPreconditionFailed, map[string]any{"title": "ETag does not match"})
		return
	}
	var body map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		api.t.Fatal(err)
	}
	format := "schedule"
	if body["engines"] != nil {
		format = "engines"
	} else if body["interval"] == nil || body["offset"] == nil || body["enabled"] == nil {
		api.t.Errorf("schedule body did not match either format: %s", body)
	}
	api.scheduleFormats = append(api.scheduleFormats, format)
	api.scheduleVersion++
	w.Header().Set("ETag", fmt.Sprintf(`"schedule-v%d"`, api.scheduleVersion+1))
	writeMCPJSON(w, http.StatusOK, map[string]any{"success": true, "data": body})
}

func (api *mcpContractTestAPI) handleAlert(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("ETag", fmt.Sprintf(`"alert-v%d"`, api.alertVersion+1))
	writeMCPJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"id": "alert-1", "acked": api.alertAcknowledged}})
}

func (api *mcpContractTestAPI) handleAcknowledgement(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("If-Match") != fmt.Sprintf(`"alert-v%d"`, api.alertVersion+1) || r.Header.Get("X-CSRF-Token") != "" {
		writeMCPJSON(w, http.StatusPreconditionFailed, map[string]any{"title": "ETag does not match"})
		return
	}
	var body struct {
		Acked bool `json:"acked"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		api.t.Fatal(err)
	}
	api.alertAcknowledged = body.Acked
	api.alertVersion++
	w.Header().Set("ETag", fmt.Sprintf(`"alert-v%d"`, api.alertVersion+1))
	writeMCPJSON(w, http.StatusOK, map[string]any{"success": true, "data": body})
}

func writeMCPJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		panic(err)
	}
}

func TestCallOperationRejectsUnknownAndMissingArguments(t *testing.T) {
	operation := structOperation()
	api := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("invalid arguments reached API") })
	for _, args := range []string{`{"unexpected":true}`, `{}`} {
		result, err := callOperation(context.Background(), api, apitoken.Principal{}, operation, json.RawMessage(args), requestSource{})
		if err != nil || result == nil || !result.IsError {
			t.Errorf("callOperation(%s) = (%+v, %v), want tool error", args, result, err)
		}
	}
	for _, raw := range []json.RawMessage{json.RawMessage(`{`), json.RawMessage(`null`)} {
		result, err := callOperation(context.Background(), api, apitoken.Principal{}, operation, raw, requestSource{})
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
	if body, err := requestBody(nil, nil); err != nil || body != nil {
		t.Fatalf("empty request body = %q, %v", body, err)
	}
	if _, err := requestBody(map[string]any{"properties": map[string]any{"bad": true}}, map[string]any{"bad": make(chan int)}); err == nil {
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
	if err != nil || len(listed.Tools) != 11 {
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
	result := responseResult(response, nil)
	if !result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "502") {
		t.Fatalf("invalid API result = %+v", result)
	}
	response = newResponseRecorder()
	response.header.Set("ETag", `"version-1"`)
	_, _ = response.Write([]byte(`{"success":true,"data":{}}`))
	result = responseResult(response, []string{"ETag"})
	if result.Meta["http"] == nil {
		t.Fatalf("ETag metadata missing from response result: %+v", result.Meta)
	}
}

func TestAddToolSkipsUnserializableInputSchema(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	addTool(server, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), apitoken.Principal{}, apicontract.Operation{
		Tool: "bad_schema", BodySchema: map[string]any{"properties": map[string]any{"bad": make(chan int)}},
	}, requestSource{})
}

func structOperation() apicontract.Operation {
	return apicontract.Operation{Method: http.MethodGet, Path: "/api/nodes/{id}", Parameters: []apicontract.Parameter{{Name: "id", In: "path", Required: true}}}
}
