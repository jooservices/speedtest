# Speedtest MCP

The center serves an authenticated Model Context Protocol (MCP) Streamable HTTP endpoint at `/mcp`. The `speedtest-mcp` stdio executable proxies the same tools for local MCP clients. Tool names, input parameters and permission scopes come from `docs/openapi.yaml`; only operations marked with `x-mcp-tool` are exposed. API and MCP responses contain product data, never integration credentials or access-token values.

## Create a token

In **Settings → Access**, create a named API token. Use `read` for dashboards and queries, `read-write` to include non-destructive actions, or `full` for direct REST operations that include destructive changes. The current MCP tool list contains no destructive operation. The center displays the token once. Put it in a private environment variable or secret store; do not paste it into a tracked MCP configuration file. Tokens may have an expiry and can be revoked from the same page.

Bearer calls use `Authorization: Bearer <token>`. They do not need a browser session cookie, Origin or CSRF header. Use HTTPS when the center is reached remotely. Each token is limited independently by `SPEEDTEST_API_TOKEN_REQUESTS_PER_MINUTE` (default 120 requests per minute, burst 20).

Successful writes through REST or MCP record the authentication method and the
non-secret token ID that authorized a bearer request. Token creation records the
new token ID as its audit target. MCP writes use the incoming client address and
the center's trusted-proxy rules; bearer values are never recorded.

## Claude Code stdio setup

Build the bridge binary in a location on the MCP client's PATH:

```sh
go build -o "$HOME/.local/bin/speedtest-mcp" ./cmd/speedtest-mcp
```

Add a project or user MCP server entry in Claude Code. Define `SPEEDTEST_API_TOKEN` in Claude Code's environment or shell environment before starting the client; the `${SPEEDTEST_API_TOKEN}` expansion keeps the credential out of the checked-in file. Set the endpoint to the configured center URL:

```json
{
  "mcpServers": {
    "speedtest": {
      "type": "stdio",
      "command": "speedtest-mcp",
      "env": {
        "SPEEDTEST_MCP_ENDPOINT": "https://speedtest.example.com/mcp",
        "SPEEDTEST_API_TOKEN": "${SPEEDTEST_API_TOKEN}"
      }
    }
  }
}
```

Restart Claude Code and run `/mcp` to confirm the server and tools are available. See the [Claude Code MCP documentation](https://code.claude.com/docs/en/mcp) for configuration locations and environment handling.

## Tools and permissions

| Tool | Operation | Required scope | Input |
|---|---|---|---|
| `list_nodes` | List nodes | `read` | Node-list filters and pagination |
| `get_node` | Read one node | `read` | Node ID |
| `list_results` | Filter result history | `read` | Node, time, engine and pagination filters |
| `get_statistics` | Read summary statistics | `read` | Range, timezone and node filters |
| `get_dashboard` | Read dashboard snapshot | `read` | Dashboard query filters |
| `list_alerts` | List alert events | `read` | State, node, time and pagination filters |
| `get_alert` | Read one alert event and its version | `read` | Alert ID |
| `acknowledge_alert` | Acknowledge or reopen one alert | `read-write` | Alert ID and acknowledgement state |
| `run_test` | Queue a node test | `read-write` | Node ID |
| `get_schedule` | Read a node schedule | `read` | Node ID |
| `update_schedule` | Replace a node schedule | `read-write` | Node ID and documented schedule fields |

A read token sees only read tools; read-write and full tokens also see read tools and non-destructive tools allowed by their scope. Destructive actions are not exposed as MCP tools. The ISP-list operation will be added to this catalog when its API operation from JOOST-74 is available in the OpenAPI contract.

Conditional write tools require the current ETag. Call `get_alert` before `acknowledge_alert`, or `get_schedule` before `update_schedule`; copy the ETag from the MCP result metadata at `_meta.http.headers.ETag` (also mirrored as `structuredContent.meta.etag`) into the write tool's `If-Match` argument. A stale value returns the API's precondition error and must be refreshed with the read tool. Bearer calls do not require the browser-only CSRF token or Origin header.

The `speedtest://docs/metrics` MCP resource contains metric units and aggregation definitions. These definitions match the [metric reference](mcp-metrics.md) and API contract.
