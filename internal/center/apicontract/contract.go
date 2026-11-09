// Package apicontract reads the embedded OpenAPI contract for route permissions and MCP tools.
package apicontract

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/jooservices/speedtest/docs"
	"gopkg.in/yaml.v3"
)

type Scope string

const (
	ScopeRead      Scope = "read"
	ScopeReadWrite Scope = "read-write"
	ScopeFull      Scope = "full"
)

// Operation describes an OpenAPI operation that may be used for route authorization or MCP.
type Operation struct {
	ID, Tool, Method, Path, Summary, Description string
	Scope                                        Scope
	Parameters                                   []Parameter
	BodySchema                                   map[string]any
}

// Parameter maps one MCP input field to an OpenAPI path or query parameter.
type Parameter struct {
	Name, In string
	Required bool
	Schema   map[string]any
}

type document struct {
	Paths      map[string]yaml.Node `yaml:"paths"`
	Components struct {
		Schemas map[string]map[string]any `yaml:"schemas"`
	} `yaml:"components"`
}

type rawOperation struct {
	ID          string      `yaml:"operationId"`
	Summary     string      `yaml:"summary"`
	Description string      `yaml:"description"`
	Tool        string      `yaml:"x-mcp-tool"`
	Scope       Scope       `yaml:"x-access-token-scope"`
	Parameters  []parameter `yaml:"parameters"`
	RequestBody requestBody `yaml:"requestBody"`
}

type parameter struct {
	Name     string         `yaml:"name"`
	In       string         `yaml:"in"`
	Required bool           `yaml:"required"`
	Schema   map[string]any `yaml:"schema"`
}

type requestBody struct {
	Content map[string]struct {
		Schema map[string]any `yaml:"schema"`
	} `yaml:"content"`
}

var (
	loadOnce sync.Once
	loaded   []Operation
	loadErr  error
)

var httpMethods = map[string]bool{"get": true, "post": true, "put": true, "patch": true, "delete": true, "head": true, "options": true}

// Operations returns the parsed operation catalog from docs/openapi.yaml.
func Operations() ([]Operation, error) {
	loadOnce.Do(load)
	return append([]Operation(nil), loaded...), loadErr
}

func load() {
	loaded, loadErr = parseOpenAPI(docs.OpenAPI)
}

func parseOpenAPI(raw []byte) ([]Operation, error) {
	var doc document
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse OpenAPI contract: %w", err)
	}
	operations := make([]Operation, 0)
	for path, item := range doc.Paths {
		var pathFields map[string]yaml.Node
		if err := item.Decode(&pathFields); err != nil {
			return nil, fmt.Errorf("parse OpenAPI path %s: %w", path, err)
		}
		var pathParameters []parameter
		if node, ok := pathFields["parameters"]; ok {
			if err := node.Decode(&pathParameters); err != nil {
				return nil, fmt.Errorf("parse OpenAPI path parameters %s: %w", path, err)
			}
		}
		for method, node := range pathFields {
			if !httpMethods[method] {
				continue
			}
			var op rawOperation
			if err := node.Decode(&op); err != nil {
				return nil, fmt.Errorf("parse OpenAPI operation %s %s: %w", method, path, err)
			}
			parameters := append(append([]parameter(nil), pathParameters...), op.Parameters...)
			body, err := resolveSchema(op.RequestBody.Content["application/json"].Schema, doc.Components.Schemas)
			if err != nil {
				return nil, fmt.Errorf("resolve OpenAPI schema %s: %w", op.ID, err)
			}
			operations = append(operations, Operation{ID: op.ID, Tool: op.Tool, Method: strings.ToUpper(method), Path: path,
				Summary: op.Summary, Description: op.Description, Scope: op.Scope,
				Parameters: convertParameters(parameters, doc.Components.Schemas), BodySchema: body})
		}
	}
	return operations, nil
}

func convertParameters(parameters []parameter, schemas map[string]map[string]any) []Parameter {
	result := make([]Parameter, 0, len(parameters))
	for _, p := range parameters {
		if p.In == "path" || p.In == "query" {
			schema, _ := resolveSchema(p.Schema, schemas)
			result = append(result, Parameter{Name: p.Name, In: p.In, Required: p.Required, Schema: schema})
		}
	}
	return result
}

func resolveSchema(schema map[string]any, schemas map[string]map[string]any) (map[string]any, error) {
	resolved, err := resolveValue(schema, schemas, map[string]bool{})
	if err != nil {
		return nil, err
	}
	result, _ := resolved.(map[string]any)
	return result, nil
}

func resolveValue(value any, schemas map[string]map[string]any, active map[string]bool) (any, error) {
	switch item := value.(type) {
	case map[string]any:
		ref, _ := item["$ref"].(string)
		if ref != "" {
			const prefix = "#/components/schemas/"
			if !strings.HasPrefix(ref, prefix) {
				return nil, fmt.Errorf("unsupported schema reference %q", ref)
			}
			name, err := url.PathUnescape(strings.TrimPrefix(ref, prefix))
			if err != nil || schemas[name] == nil {
				return nil, fmt.Errorf("unknown schema reference %q", ref)
			}
			if active[name] {
				return nil, fmt.Errorf("cyclic schema reference %q", ref)
			}
			active[name] = true
			resolved, err := resolveValue(schemas[name], schemas, active)
			delete(active, name)
			if err != nil {
				return nil, err
			}
			base, _ := resolved.(map[string]any)
			item = mergeMaps(base, item)
		}
		out := make(map[string]any, len(item))
		for key, child := range item {
			if key == "$ref" {
				continue
			}
			resolved, err := resolveValue(child, schemas, active)
			if err != nil {
				return nil, err
			}
			out[key] = resolved
		}
		return out, nil
	case []any:
		out := make([]any, len(item))
		for i, child := range item {
			resolved, err := resolveValue(child, schemas, active)
			if err != nil {
				return nil, err
			}
			out[i] = resolved
		}
		return out, nil
	default:
		return value, nil
	}
}

func mergeMaps(base, overlay map[string]any) map[string]any {
	merged := cloneMap(base)
	for key, value := range overlay {
		if key != "$ref" {
			merged[key] = value
		}
	}
	return merged
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// InputSchema merges path/query parameters with the JSON request body schema for an MCP tool.
func (op Operation) InputSchema() (json.RawMessage, error) {
	properties := map[string]any{}
	required := []string{}
	for _, p := range op.Parameters {
		properties[p.Name] = p.Schema
		if p.Required {
			required = append(required, p.Name)
		}
	}
	if body := op.BodySchema; body != nil {
		bodyProperties, _ := body["properties"].(map[string]any)
		for key, value := range bodyProperties {
			properties[key] = value
		}
		if bodyRequired, _ := body["required"].([]any); bodyRequired != nil {
			for _, field := range bodyRequired {
				required = append(required, fmt.Sprint(field))
			}
		}
	}
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	raw, err := json.Marshal(schema)
	return raw, err
}

// Find matches an HTTP method and concrete URL path against an OpenAPI route template.
func Find(method, path string) (Operation, bool, error) {
	operations, err := Operations()
	if err != nil {
		return Operation{}, false, err
	}
	var found Operation
	bestScore := int(^uint(0) >> 1)
	for _, operation := range operations {
		if operation.Method != strings.ToUpper(method) || !pathMatches(operation.Path, path) {
			continue
		}
		score := strings.Count(operation.Path, "{")
		if score < bestScore {
			found, bestScore = operation, score
		}
	}
	return found, bestScore != int(^uint(0)>>1), nil
}

func pathMatches(template, path string) bool {
	a, b := strings.Split(strings.Trim(template, "/"), "/"), strings.Split(strings.Trim(path, "/"), "/")
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if strings.HasPrefix(a[i], "{") && strings.HasSuffix(a[i], "}") {
			if b[i] == "" {
				return false
			}
			continue
		}
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Allows reports whether grant is sufficient for the operation's required scope.
func Allows(grant, required Scope) bool {
	if required == ScopeRead {
		return grant == ScopeRead || grant == ScopeReadWrite || grant == ScopeFull
	}
	if required == ScopeReadWrite {
		return grant == ScopeReadWrite || grant == ScopeFull
	}
	return required == ScopeFull && grant == ScopeFull
}

// MCPTools returns only operations explicitly marked with x-mcp-tool in OpenAPI.
func MCPTools() ([]Operation, error) {
	operations, err := Operations()
	if err != nil {
		return nil, err
	}
	return markedTools(operations)
}

func markedTools(operations []Operation) ([]Operation, error) {
	tools := make([]Operation, 0)
	for _, operation := range operations {
		if operation.Tool != "" {
			tools = append(tools, operation)
		}
	}
	if len(tools) == 0 {
		return nil, errors.New("OpenAPI contract does not mark any MCP tools")
	}
	return tools, nil
}
