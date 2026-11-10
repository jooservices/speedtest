// Package mcpserver exposes the OpenAPI-marked operations through MCP transports.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/jooservices/speedtest/docs"
	"github.com/jooservices/speedtest/internal/center/apicontract"
	"github.com/jooservices/speedtest/internal/center/apitoken"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// NewHandler serves an authenticated MCP endpoint over Streamable HTTP.
func NewHandler(api http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := apitoken.PrincipalFromContext(r.Context())
		if !ok {
			http.Error(w, "missing or invalid bearer credential", http.StatusUnauthorized)
			return
		}
		stream := mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
			return buildServer(api, principal)
		}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
		stream.ServeHTTP(w, r)
	})
}

func buildServer(api http.Handler, principal apitoken.Principal) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "speedtest", Version: "1.0.0"}, nil)
	operations, err := apicontract.MCPTools()
	if err != nil {
		return server
	}
	for _, operation := range operations {
		if !apicontract.Allows(apicontract.Scope(principal.Scope), operation.Scope) {
			continue
		}
		addTool(server, api, principal, operation)
	}
	server.AddResource(&mcp.Resource{URI: "speedtest://docs/metrics", Name: "Speedtest metrics", Description: "Metric definitions returned by Speedtest tools.", MIMEType: "text/markdown"},
		func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: "speedtest://docs/metrics", MIMEType: "text/markdown", Text: string(docs.MCPMetrics)}}}, nil
		})
	return server
}

func addTool(server *mcp.Server, api http.Handler, principal apitoken.Principal, operation apicontract.Operation) {
	schema, err := operation.InputSchema()
	if err != nil {
		return
	}
	readOnly := operation.Scope == apicontract.ScopeRead
	server.AddTool(&mcp.Tool{Name: operation.Tool, Title: operation.Summary, Description: operation.Summary + "\n\n" + operation.Description,
		InputSchema: schema, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly, DestructiveHint: boolPointer(false)}},
		func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if request.Params == nil {
				return toolError("Tool arguments are required."), nil
			}
			return callOperation(ctx, api, principal, operation, request.Params.Arguments)
		})
}

func boolPointer(value bool) *bool { return &value }

func callOperation(ctx context.Context, api http.Handler, principal apitoken.Principal, operation apicontract.Operation, raw json.RawMessage) (*mcp.CallToolResult, error) {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	var arguments map[string]any
	if err := json.Unmarshal(raw, &arguments); err != nil || arguments == nil {
		return toolError("Tool arguments must be a JSON object."), nil
	}
	req, err := operationRequest(ctx, operation, arguments)
	if err != nil {
		return toolError(err.Error()), nil
	}
	recorder := newResponseRecorder()
	api.ServeHTTP(recorder, req.WithContext(apitoken.WithPrincipal(req.Context(), principal)))
	return responseResult(recorder, operation.ResponseHeaders), nil
}

func operationRequest(ctx context.Context, operation apicontract.Operation, arguments map[string]any) (*http.Request, error) {
	allowed := make(map[string]bool)
	for _, parameter := range operation.Parameters {
		allowed[parameter.Name] = true
	}
	bodyFields := requestFields(operation.BodySchema)
	for name := range bodyFields {
		allowed[name] = true
	}
	for name := range arguments {
		if !allowed[name] {
			return nil, fmt.Errorf("unknown tool argument %q", name)
		}
	}
	path := operation.Path
	query := url.Values{}
	for _, parameter := range operation.Parameters {
		value, exists := arguments[parameter.Name]
		if parameter.Required && !exists {
			return nil, fmt.Errorf("tool argument %q is required", parameter.Name)
		}
		if !exists {
			continue
		}
		switch parameter.In {
		case "path":
			path = strings.ReplaceAll(path, "{"+parameter.Name+"}", url.PathEscape(fmt.Sprint(value)))
		case "query":
			query.Set(parameter.Name, queryValue(value))
		}
	}
	path = addQuery(path, query)
	body, err := requestBody(operation.BodySchema, arguments)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, operation.Method, path, bytes.NewReader(body))
	if err == nil && len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if err == nil {
		for _, parameter := range operation.Parameters {
			if parameter.In == "header" {
				if value, exists := arguments[parameter.Name]; exists {
					req.Header.Set(parameter.Name, fmt.Sprint(value))
				}
			}
		}
	}
	return req, err
}

func requestBody(schema map[string]any, args map[string]any) ([]byte, error) {
	if schema == nil {
		return nil, nil
	}
	fields := requestFields(schema)
	if alternatives := requestAlternatives(schema); len(alternatives) > 0 {
		var matches []map[string]any
		for _, alternative := range alternatives {
			if matchesBody(alternative, fields, args) {
				matches = append(matches, alternative)
			}
		}
		if len(matches) != 1 {
			return nil, fmt.Errorf("tool arguments must match exactly one request-body format")
		}
		fields = requestFields(matches[0])
	}
	body := make(map[string]any, len(fields))
	for name := range fields {
		if value, ok := args[name]; ok {
			body[name] = value
		}
	}
	return json.Marshal(body)
}

func matchesBody(schema map[string]any, fields map[string]any, args map[string]any) bool {
	allowed := requestFields(schema)
	for name := range fields {
		if _, provided := args[name]; provided {
			if _, accepted := allowed[name]; !accepted {
				return false
			}
		}
	}
	for _, name := range requiredFields(schema) {
		if _, provided := args[name]; !provided {
			return false
		}
	}
	return true
}

func requestFields(schema map[string]any) map[string]any {
	if fields, ok := schema["properties"].(map[string]any); ok {
		return fields
	}
	fields := map[string]any{}
	for _, alternative := range requestAlternatives(schema) {
		for name, value := range requestFields(alternative) {
			fields[name] = value
		}
	}
	return fields
}

func requestAlternatives(schema map[string]any) []map[string]any {
	values, _ := schema["oneOf"].([]any)
	result := make([]map[string]any, 0, len(values))
	for _, value := range values {
		if alternative, ok := value.(map[string]any); ok {
			result = append(result, alternative)
		}
	}
	return result
}

func requiredFields(schema map[string]any) []string {
	values, _ := schema["required"].([]any)
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, fmt.Sprint(value))
	}
	return result
}

func queryValue(value any) string {
	if items, ok := value.([]any); ok {
		parts := make([]string, 0, len(items))
		for _, item := range items {
			parts = append(parts, fmt.Sprint(item))
		}
		return strings.Join(parts, ",")
	}
	return fmt.Sprint(value)
}

func addQuery(path string, query url.Values) string {
	if len(query) == 0 {
		return path
	}
	return path + "?" + query.Encode()
}

type responseRecorder struct {
	code   int
	header http.Header
	body   bytes.Buffer
}

func newResponseRecorder() *responseRecorder {
	return &responseRecorder{code: http.StatusOK, header: make(http.Header)}
}
func (r *responseRecorder) Header() http.Header            { return r.header }
func (r *responseRecorder) WriteHeader(code int)           { r.code = code }
func (r *responseRecorder) Write(data []byte) (int, error) { return r.body.Write(data) }

func responseResult(response *responseRecorder, exposedHeaders []string) *mcp.CallToolResult {
	var payload any
	if err := json.Unmarshal(response.body.Bytes(), &payload); err != nil {
		payload = map[string]any{"status": response.code, "detail": "The API returned an invalid response."}
	}
	meta := mcp.Meta{}
	headers := map[string]any{}
	for _, name := range exposedHeaders {
		if value := response.header.Get(name); value != "" {
			headers[name] = value
		}
	}
	if len(headers) > 0 {
		meta["http"] = map[string]any{"headers": headers}
		if envelope, ok := payload.(map[string]any); ok {
			responseMeta, ok := envelope["meta"].(map[string]any)
			if !ok {
				responseMeta = map[string]any{}
				envelope["meta"] = responseMeta
			}
			responseMeta["etag"] = headers["ETag"]
		}
	}
	encoded, _ := json.Marshal(payload)
	return &mcp.CallToolResult{Meta: meta, Content: []mcp.Content{&mcp.TextContent{Text: string(encoded)}}, StructuredContent: payload, IsError: response.code < 200 || response.code >= 300}
}

func toolError(message string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: message}}, IsError: true}
}
