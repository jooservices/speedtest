// Package mcpserver exposes read-only Speedtest tools over the Model Context Protocol.
package mcpserver

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jooservices/speedtest/internal/center/usage"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// UsageReader is the query-only capability exposed to MCP clients.
type UsageReader interface {
	Usage(context.Context, usage.Query) (usage.Response, error)
}

type usageInput struct {
	Node   string `json:"node,omitempty" jsonschema:"Optional node UUID filter."`
	Engine string `json:"engine,omitempty" jsonschema:"Optional speed-test engine filter: ookla or librespeed."`
	From   string `json:"from,omitempty" jsonschema:"Optional inclusive RFC 3339 timestamp; provide with to."`
	To     string `json:"to,omitempty" jsonschema:"Optional exclusive RFC 3339 timestamp; provide with from."`
	Group  string `json:"group,omitempty" jsonschema:"Calendar bucket size: day, week or month. Defaults to day."`
}

// New registers the read-only get_usage tool.
func New(store UsageReader) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "speedtest", Version: "1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_usage",
		Description: "Read measured Speedtest bandwidth usage by node, engine and calendar period. Engines remain separate.",
	}, getUsage(store))
	return server
}

func getUsage(store UsageReader) mcp.ToolHandlerFor[usageInput, usage.Response] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, input usageInput) (*mcp.CallToolResult, usage.Response, error) {
		query, err := input.query()
		if err != nil {
			return nil, usage.Response{}, err
		}
		result, err := store.Usage(ctx, query)
		if err != nil {
			if errors.Is(err, usage.ErrInvalid) {
				return nil, usage.Response{}, errors.New("invalid usage filters")
			}
			slog.Error("MCP get_usage query failed")
			return nil, usage.Response{}, errors.New("usage query failed")
		}
		return nil, result, nil
	}
}

func (input usageInput) query() (usage.Query, error) {
	if (input.From == "") != (input.To == "") {
		return usage.Query{}, errors.New("from and to must be provided together")
	}
	query := usage.Query{NodeID: input.Node, Engine: input.Engine, Group: input.Group}
	if input.From == "" {
		return query, nil
	}
	from, err := time.Parse(time.RFC3339, input.From)
	if err != nil {
		return usage.Query{}, errors.New("from must be an RFC 3339 timestamp")
	}
	to, err := time.Parse(time.RFC3339, input.To)
	if err != nil {
		return usage.Query{}, errors.New("to must be an RFC 3339 timestamp")
	}
	query.From, query.To = from, to
	return query, nil
}
