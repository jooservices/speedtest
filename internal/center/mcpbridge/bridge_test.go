package mcpbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func closeSession(t *testing.T, session *mcp.ClientSession) {
	t.Helper()
	if err := session.Close(); err != nil {
		t.Errorf("close MCP session: %v", err)
	}
}

func TestValidateConfigRejectsUnsafeOrMissingValues(t *testing.T) {
	valid := []struct{ endpoint, token string }{
		{"http://127.0.0.1:8080/mcp", "st_at_example"},
		{"https://speedtest.example.test/mcp", "token"},
	}
	for _, test := range valid {
		if err := ValidateConfig(test.endpoint, test.token); err != nil {
			t.Errorf("ValidateConfig(%q) error = %v", test.endpoint, err)
		}
	}
	invalid := []struct{ endpoint, token string }{
		{"", "token"}, {"ftp://center.test/mcp", "token"}, {"https://user:pass@center.test/mcp", "token"},
		{"https://center.test/mcp#fragment", "token"}, {"https://center.test/mcp", "  "},
		{"https://center.test/mcp", strings.Repeat("x", 8193)},
	}
	for _, test := range invalid {
		if err := ValidateConfig(test.endpoint, test.token); err == nil {
			t.Errorf("ValidateConfig(%q, token length %d) unexpectedly succeeded", test.endpoint, len(test.token))
		} else if strings.Contains(err.Error(), test.token) || test.endpoint != "" && strings.Contains(err.Error(), test.endpoint) {
			t.Errorf("configuration error disclosed input: %v", err)
		}
	}
}

func TestRunRejectsInvalidConfigurationBeforeConnecting(t *testing.T) {
	if err := Run(context.Background(), "", "secret-value"); err == nil || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("Run invalid config error = %v", err)
	}
}

type failingTransport struct{ err error }

func (t failingTransport) Connect(context.Context) (mcp.Connection, error) { return nil, t.err }

func TestRunConnectsRemoteAndStartsLocalTransport(t *testing.T) {
	remote := mcp.NewServer(&mcp.Implementation{Name: "remote", Version: "1"}, nil)
	endpoint := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remote }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	defer endpoint.Close()
	want := errors.New("local transport failed")
	if err := run(context.Background(), endpoint.URL, "token-value", failingTransport{err: want}); !errors.Is(err, want) {
		t.Fatalf("run() error = %v, want local transport error", err)
	}
}

func TestRunHidesRemoteConnectionDetails(t *testing.T) {
	endpoint := httptest.NewServer(http.NotFoundHandler())
	url := endpoint.URL
	endpoint.Close()
	err := Run(context.Background(), url, "token-value")
	if err == nil || strings.Contains(err.Error(), "token-value") || strings.Contains(err.Error(), url) {
		t.Fatalf("Run connection error disclosed configuration: %v", err)
	}
}

func TestRunReturnsRemoteToolDiscoveryFailure(t *testing.T) {
	remoteServer := mcp.NewServer(&mcp.Implementation{Name: "remote", Version: "1"}, nil)
	stream := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remoteServer }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var request struct {
			Method string `json:"method"`
		}
		if json.Unmarshal(body, &request) == nil && request.Method == "tools/list" {
			http.Error(w, "tool listing failed", http.StatusInternalServerError)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		stream.ServeHTTP(w, r)
	}))
	defer endpoint.Close()
	err := run(context.Background(), endpoint.URL, "token", failingTransport{err: errors.New("local transport must not start")})
	if err == nil || !strings.Contains(err.Error(), "could not list Speedtest MCP tools") {
		t.Fatalf("run tool discovery error = %v", err)
	}
}

func TestBearerTransportSetsCredentialWithoutMutatingRequest(t *testing.T) {
	seen := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	request, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Test", "preserved")
	origin, _ := url.Parse(server.URL)
	if _, err := (bearerTransport{next: server.Client().Transport, token: "secret-value", origin: origin}).RoundTrip(request); err != nil {
		t.Fatal(err)
	}
	if got := <-seen; got != "Bearer secret-value" {
		t.Fatalf("Authorization = %q", got)
	}
	if request.Header.Get("Authorization") != "" || request.Header.Get("X-Test") != "preserved" {
		t.Fatalf("original request was mutated: %+v", request.Header)
	}
}

func TestBearerTransportRefusesCrossOriginRedirectTarget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("cross-origin request reached destination") }))
	defer server.Close()
	origin, _ := url.Parse("https://center.example.test/mcp")
	request, err := http.NewRequest(http.MethodPost, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (bearerTransport{next: server.Client().Transport, token: "secret-value", origin: origin}).RoundTrip(request); err == nil || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("cross-origin request error = %v", err)
	}
}

func TestProxyServerForwardsToolCalls(t *testing.T) {
	remoteServer := mcp.NewServer(&mcp.Implementation{Name: "remote", Version: "1"}, nil)
	remoteServer.AddTool(&mcp.Tool{Name: "echo", InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}}}`)},
		func(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args map[string]string
			if err := json.Unmarshal(request.Params.Arguments, &args); err != nil {
				return nil, err
			}
			value := args["value"]
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: value}}}, nil
		})
	remoteServer.AddResource(&mcp.Resource{URI: "speedtest://fixture/docs", Name: "Fixture docs", MIMEType: "text/plain"},
		func(_ context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: "speedtest://fixture/docs", MIMEType: "text/plain", Text: "resource-ok"}}}, nil
		})
	remoteHTTP := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remoteServer }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	remoteEndpoint := httptest.NewServer(remoteHTTP)
	defer remoteEndpoint.Close()
	ctx := context.Background()
	remoteClient := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil)
	remote, err := remoteClient.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: remoteEndpoint.URL, HTTPClient: remoteEndpoint.Client()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeSession(t, remote)
	proxy, err := proxyServer(ctx, remote)
	if err != nil {
		t.Fatal(err)
	}
	localHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return proxy }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	defer localHTTP.Close()
	localClient := mcp.NewClient(&mcp.Implementation{Name: "local-test-client", Version: "1"}, nil)
	local, err := localClient.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: localHTTP.URL, HTTPClient: localHTTP.Client()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeSession(t, local)
	tools, err := local.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 1 || tools.Tools[0].Name != "echo" {
		t.Fatalf("proxied tool list = %+v, error = %v", tools, err)
	}
	result, err := local.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"value": "hello"}})
	if err != nil || result.IsError || result.Content[0].(*mcp.TextContent).Text != "hello" {
		t.Fatalf("proxied call = %+v, error = %v", result, err)
	}
	resource, err := local.ReadResource(ctx, &mcp.ReadResourceParams{URI: "speedtest://fixture/docs"})
	if err != nil || len(resource.Contents) != 1 || resource.Contents[0].Text != "resource-ok" {
		t.Fatalf("proxied resource = %+v, error = %v", resource, err)
	}
}

func TestProxyServerHandlesRemoteToolListFailure(t *testing.T) {
	remoteServer := mcp.NewServer(&mcp.Implementation{Name: "remote", Version: "1"}, nil)
	endpoint := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remoteServer }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	defer endpoint.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil)
	remote, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: endpoint.URL, HTTPClient: endpoint.Client()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeSession(t, remote)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := proxyServer(ctx, remote); err == nil || !strings.Contains(err.Error(), "could not list Speedtest MCP tools") {
		t.Fatalf("proxyServer canceled list error = %v", err)
	}
}

func TestProxyServerHandlesRemoteResourceListFailure(t *testing.T) {
	remoteServer := mcp.NewServer(&mcp.Implementation{Name: "remote", Version: "1"}, nil)
	stream := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remoteServer }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var request struct {
			Method string `json:"method"`
		}
		if json.Unmarshal(body, &request) == nil && request.Method == "resources/list" {
			http.Error(w, "resource listing failed", http.StatusInternalServerError)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		stream.ServeHTTP(w, r)
	}))
	defer endpoint.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil)
	remote, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: endpoint.URL, HTTPClient: endpoint.Client()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeSession(t, remote)
	if _, err := proxyServer(context.Background(), remote); err == nil || !strings.Contains(err.Error(), "could not list Speedtest MCP resources") {
		t.Fatalf("proxyServer resource list error = %v", err)
	}
}

func TestProxyToolRejectsMissingArguments(t *testing.T) {
	if _, err := proxyTool(nil, "example")(context.Background(), nil); err == nil || err.Error() != "tool arguments are required" {
		t.Fatalf("missing tool request error = %v", err)
	}
}
