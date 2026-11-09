package apicontract

import (
	"encoding/json"
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
		"get_statistics": ScopeRead, "get_dashboard": ScopeRead, "list_alerts": ScopeRead,
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

func TestMarkedToolsRejectsEmptyCatalog(t *testing.T) {
	if _, err := markedTools([]Operation{{ID: "not-a-tool"}}); err == nil {
		t.Fatal("empty MCP tool catalog was accepted")
	}
}
