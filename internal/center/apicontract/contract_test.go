package apicontract

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestOperationsIncludeScopedAPIAndResolveNestedSchemas(t *testing.T) {
	operations, err := Operations()
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) < 50 {
		t.Fatalf("loaded %d API operations, expected the complete contract", len(operations))
	}
	for _, operation := range operations {
		if strings.HasPrefix(operation.Path, "/api/") && !strings.HasPrefix(operation.Path, "/api/auth/") &&
			!strings.HasPrefix(operation.Path, "/api/agent/") && !strings.HasPrefix(operation.Path, "/api/access-tokens") && operation.Scope == "" {
			t.Errorf("owner operation %s %s has no access-token scope", operation.Method, operation.Path)
		}
		if operation.Tool != "" {
			input, err := operation.InputSchema()
			if err != nil || strings.Contains(string(input), `"$ref"`) {
				t.Errorf("tool %s input schema unresolved: %s (%v)", operation.Tool, input, err)
			}
		}
	}
	operation, ok, err := Find("GET", "/api/nodes/123e4567-e89b-12d3-a456-426614174000")
	if err != nil || !ok || operation.ID != "getNode" || operation.Scope != ScopeRead {
		t.Fatalf("Find node detail = (%+v, %v, %v)", operation, ok, err)
	}
	input, err := operation.InputSchema()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(input), `"$ref"`) {
		t.Fatalf("input schema contains unresolved references: %s", input)
	}
	var decoded any
	if err := json.Unmarshal(input, &decoded); err != nil {
		t.Fatalf("input schema is not JSON: %v", err)
	}
}

func TestMCPToolsMatchOperationCatalogAndScope(t *testing.T) {
	tools, err := MCPTools()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Scope{
		"list_nodes": ScopeRead, "get_node": ScopeRead, "list_results": ScopeRead,
		"get_statistics": ScopeRead, "get_dashboard": ScopeRead, "list_alerts": ScopeRead, "get_alert": ScopeRead,
		"acknowledge_alert": ScopeReadWrite, "run_test": ScopeReadWrite,
		"get_schedule": ScopeRead, "update_schedule": ScopeReadWrite,
	}
	if len(tools) != len(want) {
		t.Fatalf("tool count = %d, want %d", len(tools), len(want))
	}
	for _, operation := range tools {
		if expected, ok := want[operation.Tool]; !ok || expected != operation.Scope {
			t.Errorf("tool %q scope = %q, expected %q", operation.Tool, operation.Scope, expected)
		}
		delete(want, operation.Tool)
	}
	if len(want) != 0 {
		t.Errorf("missing MCP tools: %v", want)
	}
}

func TestMCPToolsReturnsCatalogError(t *testing.T) {
	want := errors.New("catalog unavailable")
	if _, err := toolsFromOperations(nil, want); !errors.Is(err, want) {
		t.Fatalf("toolsFromOperations error = %v, want %v", err, want)
	}
}

func TestMCPContractPreservesReferencedFiltersHeadersAndBodyAlternatives(t *testing.T) {
	operations, err := Operations()
	if err != nil {
		t.Fatal(err)
	}
	byTool := map[string]Operation{}
	for _, operation := range operations {
		if operation.Tool != "" {
			byTool[operation.Tool] = operation
		}
	}
	for _, tool := range []string{"list_nodes", "list_results", "list_alerts"} {
		operation := byTool[tool]
		if operation.ID == "" {
			t.Fatalf("tool %q is missing", tool)
		}
		if !hasParameter(operation.Parameters, "page", "query") || !hasParameter(operation.Parameters, "size", "query") {
			t.Errorf("tool %q is missing referenced pagination parameters: %+v", tool, operation.Parameters)
		}
	}
	results := byTool["list_results"]
	for _, name := range []string{"engine", "nodes", "range", "from", "to", "status", "sort", "dir", "page", "size"} {
		if !hasParameter(results.Parameters, name, "query") {
			t.Errorf("list_results is missing filter %q", name)
		}
	}
	for _, name := range []string{"If-Match"} {
		if !hasParameter(byTool["acknowledge_alert"].Parameters, name, "header") || !hasParameter(byTool["update_schedule"].Parameters, name, "header") {
			t.Errorf("write tools do not expose conditional header %q", name)
		}
	}
	for _, operation := range []Operation{byTool["get_alert"], byTool["get_schedule"]} {
		if !contains(operation.ResponseHeaders, "ETag") {
			t.Errorf("tool %q does not expose response ETag: %+v", operation.Tool, operation.ResponseHeaders)
		}
	}
	schedule := byTool["update_schedule"]
	if got := len(bodyAlternatives(schedule.BodySchema)); got != 2 {
		t.Fatalf("update_schedule has %d body alternatives, want 2", got)
	}
	input, err := schedule.InputSchema()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{`"interval"`, `"offset"`, `"enabled"`, `"engines"`, `"If-Match"`} {
		if !strings.Contains(string(input), name) {
			t.Errorf("update_schedule schema is missing %s: %s", name, input)
		}
	}
	if strings.Contains(string(input), `"X-CSRF-Token"`) || strings.Contains(string(input), `"Origin"`) {
		t.Fatalf("browser-only CSRF/Origin headers leaked into bearer MCP schema: %s", input)
	}
}

func hasParameter(parameters []Parameter, name, location string) bool {
	for _, parameter := range parameters {
		if parameter.Name == name && parameter.In == location {
			return true
		}
	}
	return false
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestAllowsScopeHierarchy(t *testing.T) {
	for _, test := range []struct {
		grant, required Scope
		want            bool
	}{
		{ScopeRead, ScopeRead, true}, {ScopeRead, ScopeReadWrite, false}, {ScopeRead, ScopeFull, false},
		{ScopeReadWrite, ScopeRead, true}, {ScopeReadWrite, ScopeReadWrite, true}, {ScopeReadWrite, ScopeFull, false},
		{ScopeFull, ScopeRead, true}, {ScopeFull, ScopeReadWrite, true}, {ScopeFull, ScopeFull, true},
		{ScopeFull, "", false},
	} {
		if got := Allows(test.grant, test.required); got != test.want {
			t.Errorf("Allows(%q, %q) = %t, want %t", test.grant, test.required, got, test.want)
		}
	}
}

func TestResolveSchemaRejectsReferencesOutsideComponents(t *testing.T) {
	_, err := resolveSchema(map[string]any{"$ref": "https://example.test/schema"}, nil)
	if err == nil {
		t.Fatal("unsupported schema reference was accepted")
	}
	_, err = resolveSchema(map[string]any{"properties": map[string]any{"nested": map[string]any{"$ref": "#/components/schemas/Missing"}}}, nil)
	if err == nil {
		t.Fatal("missing nested schema reference was accepted")
	}
}

func TestParseOpenAPIErrorsAndPathParameters(t *testing.T) {
	for _, raw := range []string{
		"paths: [",
		"paths:\n  /broken: scalar\n",
		"paths:\n  /broken:\n    parameters: scalar\n",
		"paths:\n  /broken:\n    get: scalar\n",
		"paths:\n  /broken:\n    post:\n      operationId: broken\n      requestBody:\n        content:\n          application/json:\n            schema:\n              $ref: '#/components/schemas/Missing'\n",
	} {
		if _, err := parseOpenAPI([]byte(raw)); err == nil {
			t.Errorf("parseOpenAPI(%q) unexpectedly succeeded", raw)
		}
	}
	operations, err := parseOpenAPI([]byte("paths:\n  /items/{id}:\n    parameters:\n      - name: id\n        in: path\n        required: true\n        schema: {type: string}\n    get:\n      operationId: getItem\n      parameters:\n        - name: include\n          in: query\n          schema: {type: boolean}\n    x-note: ignored\n"))
	if err != nil || len(operations) != 1 || len(operations[0].Parameters) != 2 {
		t.Fatalf("parsed operations = %+v, error = %v", operations, err)
	}
}

func TestParseOpenAPIResolvesComponentParametersAndResponseHeaders(t *testing.T) {
	raw := []byte(`components:
  schemas:
    ID: {type: string}
    A: {type: object, properties: {interval: {type: integer}}, required: [interval]}
    B: {type: object, properties: {engines: {type: array}}, required: [engines]}
  parameters:
    NodeID: {name: id, in: path, required: true, schema: {$ref: '#/components/schemas/ID'}}
    IfMatch: {name: If-Match, in: header, required: true, schema: {type: string}}
  headers:
    ETag: {schema: {type: string}}
paths:
  /nodes/{id}:
    parameters:
      - {$ref: '#/components/parameters/NodeID'}
    get:
      operationId: readNode
      x-mcp-tool: get_node
      parameters:
        - {$ref: '#/components/parameters/IfMatch'}
      responses:
        '200':
          description: OK
          headers:
            ETag: {$ref: '#/components/headers/ETag'}
    put:
      operationId: updateNode
      requestBody:
        content:
          application/json:
            schema:
              oneOf:
                - {$ref: '#/components/schemas/A'}
                - {$ref: '#/components/schemas/B'}
      responses:
        '204': {description: Updated}
`)
	operations, err := parseOpenAPI(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 2 || len(operations[0].Parameters) != 2 || !contains(operations[0].ResponseHeaders, "ETag") {
		t.Fatalf("resolved operations = %+v", operations)
	}
	input, err := operations[1].InputSchema()
	if err != nil || !strings.Contains(string(input), `"interval"`) || !strings.Contains(string(input), `"engines"`) || !strings.Contains(string(input), `"oneOf"`) {
		t.Fatalf("resolved oneOf input = %s, error = %v", input, err)
	}
	if _, err := parseOpenAPI([]byte("components:\n  parameters: {}\npaths:\n  /nodes:\n    get:\n      operationId: broken\n      parameters:\n        - {$ref: '#/components/parameters/Missing'}\n")); err == nil {
		t.Fatal("unknown component parameter reference was accepted")
	}
}

func TestMarkedToolsRejectsEmptyCatalog(t *testing.T) {
	if _, err := markedTools([]Operation{{ID: "not-a-tool"}}); err == nil {
		t.Fatal("empty MCP tool catalog was accepted")
	}
}
