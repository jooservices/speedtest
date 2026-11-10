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
	ResponseHeaders                              []string
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
		Schemas    map[string]map[string]any `yaml:"schemas"`
		Parameters map[string]parameter      `yaml:"parameters"`
	} `yaml:"components"`
}

type rawOperation struct {
	ID          string                 `yaml:"operationId"`
	Summary     string                 `yaml:"summary"`
	Description string                 `yaml:"description"`
	Tool        string                 `yaml:"x-mcp-tool"`
	Scope       Scope                  `yaml:"x-access-token-scope"`
	Parameters  []parameter            `yaml:"parameters"`
	RequestBody requestBody            `yaml:"requestBody"`
	Responses   map[string]rawResponse `yaml:"responses"`
}

type parameter struct {
	Ref      string         `yaml:"$ref"`
	Name     string         `yaml:"name"`
	In       string         `yaml:"in"`
	Required bool           `yaml:"required"`
	Schema   map[string]any `yaml:"schema"`
}

type rawResponse struct {
	Headers map[string]struct {
		Ref string `yaml:"$ref"`
	} `yaml:"headers"`
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
			resolvedParameters, err := resolveParameters(pathParameters, doc.Components.Parameters)
			if err != nil {
				return nil, fmt.Errorf("resolve OpenAPI path parameters %s: %w", path, err)
			}
			pathParameters = resolvedParameters
		}
		for method, node := range pathFields {
			if !httpMethods[method] {
				continue
			}
			var op rawOperation
			if err := node.Decode(&op); err != nil {
				return nil, fmt.Errorf("parse OpenAPI operation %s %s: %w", method, path, err)
			}
			operationParameters, err := resolveParameters(op.Parameters, doc.Components.Parameters)
			if err != nil {
				return nil, fmt.Errorf("resolve OpenAPI parameters %s: %w", op.ID, err)
			}
			parameters := append(append([]parameter(nil), pathParameters...), operationParameters...)
			converted, err := convertParameters(parameters, doc.Components.Schemas)
			if err != nil {
				return nil, fmt.Errorf("convert OpenAPI parameters %s: %w", op.ID, err)
			}
			body, err := resolveSchema(op.RequestBody.Content["application/json"].Schema, doc.Components.Schemas)
			if err != nil {
				return nil, fmt.Errorf("resolve OpenAPI schema %s: %w", op.ID, err)
			}
			headers, err := successHeaders(op.Responses)
			if err != nil {
				return nil, fmt.Errorf("resolve OpenAPI response headers %s: %w", op.ID, err)
			}
			operations = append(operations, Operation{ID: op.ID, Tool: op.Tool, Method: strings.ToUpper(method), Path: path,
				Summary: op.Summary, Description: op.Description, Scope: op.Scope,
				Parameters: converted, BodySchema: body, ResponseHeaders: headers})
		}
	}
	return operations, nil
}

func resolveParameters(parameters []parameter, components map[string]parameter) ([]parameter, error) {
	result := make([]parameter, 0, len(parameters))
	for _, p := range parameters {
		if p.Ref == "" {
			result = append(result, p)
			continue
		}
		const prefix = "#/components/parameters/"
		if !strings.HasPrefix(p.Ref, prefix) {
			return nil, fmt.Errorf("unsupported parameter reference %q", p.Ref)
		}
		name, err := url.PathUnescape(strings.TrimPrefix(p.Ref, prefix))
		if err != nil || components[name].Name == "" {
			return nil, fmt.Errorf("unknown parameter reference %q", p.Ref)
		}
		result = append(result, components[name])
	}
	return result, nil
}

func successHeaders(responses map[string]rawResponse) ([]string, error) {
	var headers []string
	for status, response := range responses {
		if len(status) != 3 || status[0] != '2' {
			continue
		}
		for name, header := range response.Headers {
			if header.Ref != "" && !strings.HasPrefix(header.Ref, "#/components/headers/") {
				return nil, fmt.Errorf("unsupported response header reference %q", header.Ref)
			}
			if strings.EqualFold(name, "ETag") {
				headers = append(headers, "ETag")
			}
		}
	}
	return headers, nil
}

func convertParameters(parameters []parameter, schemas map[string]map[string]any) ([]Parameter, error) {
	result := make([]Parameter, 0, len(parameters))
	for _, p := range parameters {
		if p.In == "path" || p.In == "query" || p.In == "header" {
			if p.In == "header" && (strings.EqualFold(p.Name, "X-CSRF-Token") || strings.EqualFold(p.Name, "Origin")) {
				continue
			}
			schema, err := resolveSchema(p.Schema, schemas)
			if err != nil {
				return nil, fmt.Errorf("parameter %q: %w", p.Name, err)
			}
			result = append(result, Parameter{Name: p.Name, In: p.In, Required: p.Required, Schema: schema})
		}
	}
	return result, nil
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
		for key, value := range bodyFields(body) {
			properties[key] = value
		}
		required = append(required, stringFields(body["required"])...)
		if alternatives := bodyAlternatives(body); len(alternatives) > 0 {
			branches := make([]any, 0, len(alternatives))
			for _, alternative := range alternatives {
				branchRequired := append(append([]string(nil), required...), stringFields(alternative["required"])...)
				branches = append(branches, map[string]any{"required": uniqueStrings(branchRequired)})
			}
			schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false, "oneOf": branches}
			if len(required) > 0 {
				schema["required"] = uniqueStrings(required)
			}
			return json.Marshal(schema)
		}
	}
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	raw, err := json.Marshal(schema)
	return raw, err
}

func bodyFields(schema map[string]any) map[string]any {
	if fields, ok := schema["properties"].(map[string]any); ok {
		return fields
	}
	fields := map[string]any{}
	for _, alternative := range bodyAlternatives(schema) {
		for name, value := range bodyFields(alternative) {
			fields[name] = value
		}
	}
	return fields
}

func bodyAlternatives(schema map[string]any) []map[string]any {
	values, _ := schema["oneOf"].([]any)
	result := make([]map[string]any, 0, len(values))
	for _, value := range values {
		if alternative, ok := value.(map[string]any); ok {
			result = append(result, alternative)
		}
	}
	return result
}

func stringFields(value any) []string {
	values, _ := value.([]any)
	result := make([]string, 0, len(values))
	for _, field := range values {
		result = append(result, fmt.Sprint(field))
	}
	return result
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
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
	return toolsFromOperations(operations, err)
}

func toolsFromOperations(operations []Operation, err error) ([]Operation, error) {
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
