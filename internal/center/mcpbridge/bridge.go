// Package mcpbridge provides a local stdio MCP server backed by the center's Streamable HTTP MCP endpoint.
package mcpbridge

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Run connects an authenticated local stdio server to the remote center MCP endpoint.
func Run(ctx context.Context, endpoint, token string) error {
	return run(ctx, endpoint, token, &mcp.StdioTransport{})
}

func run(ctx context.Context, endpoint, token string, localTransport mcp.Transport) (runErr error) {
	if err := ValidateConfig(endpoint, token); err != nil {
		return err
	}
	target, _ := url.Parse(endpoint)
	httpClient := &http.Client{Transport: bearerTransport{next: http.DefaultTransport, token: token, origin: target}}
	client := mcp.NewClient(&mcp.Implementation{Name: "speedtest-stdio", Version: "1.0.0"}, nil)
	remote, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: httpClient, DisableStandaloneSSE: true}, nil)
	if err != nil {
		return errors.New("could not connect to the Speedtest MCP endpoint")
	}
	defer func() {
		if closeErr := remote.Close(); runErr == nil && closeErr != nil {
			runErr = errors.New("could not close the Speedtest MCP connection")
		}
	}()
	local, err := proxyServer(ctx, remote)
	if err != nil {
		return err
	}
	return local.Run(ctx, localTransport)
}

// ValidateConfig checks the endpoint and token without including either value in errors.
func ValidateConfig(endpoint, token string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.User != nil || parsed.Fragment != "" {
		return errors.New("SPEEDTEST_MCP_ENDPOINT must be an HTTP(S) URL without user info or a fragment")
	}
	if strings.TrimSpace(token) == "" || len(token) > 8192 {
		return errors.New("SPEEDTEST_API_TOKEN must contain a bearer token")
	}
	return nil
}

func proxyServer(ctx context.Context, remote *mcp.ClientSession) (*mcp.Server, error) {
	server := mcp.NewServer(&mcp.Implementation{Name: "speedtest-stdio", Version: "1.0.0"}, nil)
	tools, err := remote.ListTools(ctx, nil)
	if err != nil {
		return nil, errors.New("could not list Speedtest MCP tools")
	}
	for _, tool := range tools.Tools {
		copy := *tool
		server.AddTool(&copy, proxyTool(remote, copy.Name))
	}
	resources, err := remote.ListResources(ctx, nil)
	if err != nil {
		return nil, errors.New("could not list Speedtest MCP resources")
	}
	for _, resource := range resources.Resources {
		copy := *resource
		server.AddResource(&copy, proxyResource(remote, copy.URI))
	}
	return server, nil
}

func proxyTool(remote *mcp.ClientSession, name string) mcp.ToolHandler {
	return func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, errors.New("tool arguments are required")
		}
		return remote.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: request.Params.Arguments})
	}
}

func proxyResource(remote *mcp.ClientSession, uri string) mcp.ResourceHandler {
	return func(ctx context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return remote.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
	}
}

type bearerTransport struct {
	next   http.RoundTripper
	token  string
	origin *url.URL
}

func (t bearerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if t.origin == nil || !sameOrigin(t.origin, request.URL) {
		return nil, errors.New("refusing a cross-origin MCP request")
	}
	copy := request.Clone(request.Context())
	copy.Header = request.Header.Clone()
	copy.Header.Set("Authorization", "Bearer "+t.token)
	return t.next.RoundTrip(copy)
}

func sameOrigin(first, second *url.URL) bool {
	return strings.EqualFold(first.Scheme, second.Scheme) && strings.EqualFold(first.Host, second.Host)
}
