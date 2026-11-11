// Package docs embeds the API contract used by runtime API integrations.
package docs

import _ "embed"

// OpenAPI is the product API contract used by the MCP operation registry.
//
//go:embed openapi.yaml
var OpenAPI []byte

// MCPMetrics contains the static metrics reference served as an MCP resource.
//
//go:embed mcp-metrics.md
var MCPMetrics []byte
