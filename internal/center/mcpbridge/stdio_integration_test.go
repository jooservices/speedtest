//go:build integration

package mcpbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type stdioFixture struct {
	endpoint          string
	seenAuthorization chan string
}

func TestStdioBinaryProxiesAuthenticatedMCPToolsIntegration(t *testing.T) {
	fixture := newStdioFixture(t)
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	binary := buildStdioBinary(t, root)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	session := connectStdio(t, ctx, binary, fixture.endpoint)
	defer closeSession(t, session)
	verifyStdioToolCall(t, ctx, session)
	if len(fixture.seenAuthorization) == 0 {
		t.Fatal("stdio bridge did not send an authenticated remote request")
	}
}

func newStdioFixture(t *testing.T) stdioFixture {
	t.Helper()
	seenAuthorization := make(chan string, 8)
	remoteServer := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	remoteServer.AddTool(&mcp.Tool{Name: "fixture_echo", InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}}}`)},
		func(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args map[string]string
			if err := json.Unmarshal(request.Params.Arguments, &args); err != nil {
				return nil, err
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: args["value"]}}}, nil
		})
	stream := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remoteServer }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuthorization <- r.Header.Get("Authorization")
		if r.Header.Get("Authorization") != "Bearer stdio-fixture-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		stream.ServeHTTP(w, r)
	}))
	t.Cleanup(endpoint.Close)
	return stdioFixture{endpoint: endpoint.URL + "/mcp", seenAuthorization: seenAuthorization}
}

func buildStdioBinary(t *testing.T, root string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".tmp"), 0o700); err != nil {
		t.Fatal("create worktree temporary directory:", err)
	}
	tempDir, err := os.MkdirTemp(filepath.Join(root, ".tmp"), "mcp-stdio-")
	if err != nil {
		t.Fatal("create stdio fixture directory:", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tempDir) })
	binary := filepath.Join(tempDir, "speedtest-mcp")
	build := exec.Command("go", "build", "-o", binary, "./cmd/speedtest-mcp")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build stdio binary failed: %v (%d output bytes)", err, len(output))
	}
	return binary
}

func connectStdio(t *testing.T, ctx context.Context, binary, endpoint string) *mcp.ClientSession {
	t.Helper()
	command := exec.Command(binary)
	command.Env = append(os.Environ(), "SPEEDTEST_MCP_ENDPOINT="+endpoint, "SPEEDTEST_API_TOKEN=stdio-fixture-token")
	client := mcp.NewClient(&mcp.Implementation{Name: "stdio-fixture-client", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: command}, nil)
	if err != nil {
		t.Fatal("connect MCP client to stdio binary:", err)
	}
	return session
}

func verifyStdioToolCall(t *testing.T, ctx context.Context, session *mcp.ClientSession) {
	t.Helper()
	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 1 || tools.Tools[0].Name != "fixture_echo" {
		t.Fatalf("stdio tool discovery returned %d tools, error=%v", len(tools.Tools), err)
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "fixture_echo", Arguments: map[string]any{"value": "stdio-ok"}})
	if err != nil || result.IsError || result.Content[0].(*mcp.TextContent).Text != "stdio-ok" {
		t.Fatalf("stdio tool call error=%v tool-error=%t", err, result != nil && result.IsError)
	}
}
