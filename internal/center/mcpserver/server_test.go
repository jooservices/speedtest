package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/usage"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type usageStub struct {
	query    usage.Query
	response usage.Response
	err      error
}

func (s *usageStub) Usage(_ context.Context, query usage.Query) (usage.Response, error) {
	s.query = query
	return s.response, s.err
}

func connect(t *testing.T, store UsageReader) *mcp.ClientSession {
	t.Helper()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := New(store).Connect(context.Background(), serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestGetUsageToolListsAndReturnsFilteredUsage(t *testing.T) {
	response := usage.Response{Group: "month", Totals: usage.Bytes{Down: 40, Up: 2, Total: 42}}
	store := &usageStub{response: response}
	session := connect(t, store)
	tools, err := session.ListTools(t.Context(), nil)
	if err != nil || len(tools.Tools) != 1 || tools.Tools[0].Name != "get_usage" {
		t.Fatalf("tools = %+v, err = %v", tools, err)
	}
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "get_usage", Arguments: map[string]any{
		"node": "10000000-0000-4000-8000-000000000001", "engine": "ookla",
		"from": "2026-11-01T00:00:00Z", "to": "2026-11-09T00:00:00+07:00", "group": "month",
	}})
	if err != nil || result.IsError {
		t.Fatalf("tool result = %+v, err = %v", result, err)
	}
	_, offset := store.query.To.Zone()
	if store.query.NodeID != "10000000-0000-4000-8000-000000000001" || store.query.Engine != "ookla" || store.query.Group != "month" || !store.query.From.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) || offset != 7*60*60 {
		t.Fatalf("query = %+v", store.query)
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var got usage.Response
	if err := json.Unmarshal(raw, &got); err != nil || got.Totals.Total != 42 {
		t.Fatalf("structured result = %s, err = %v", raw, err)
	}
}

func TestGetUsageToolRejectsInvalidFilters(t *testing.T) {
	store := &usageStub{}
	session := connect(t, store)
	for _, arguments := range []map[string]any{
		{"from": "2026-11-01T00:00:00Z"},
		{"from": "yesterday", "to": "2026-11-09T00:00:00Z"},
	} {
		result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "get_usage", Arguments: arguments})
		if err != nil || !result.IsError {
			t.Fatalf("arguments %v: result = %+v, err = %v", arguments, result, err)
		}
	}
	if store.query != (usage.Query{}) {
		t.Fatalf("invalid filters reached store: %+v", store.query)
	}
}

func TestGetUsageToolHidesDatabaseErrors(t *testing.T) {
	store := &usageStub{err: errors.New("database password leaked")}
	session := connect(t, store)
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "get_usage", Arguments: map[string]any{}})
	if err != nil || !result.IsError || len(result.Content) == 0 {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	if text, ok := result.Content[0].(*mcp.TextContent); !ok || text.Text != "usage query failed" {
		t.Fatalf("error content = %#v", result.Content)
	}
}
