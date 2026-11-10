//go:build integration

package mcpserver

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/usage"
	"github.com/jooservices/speedtest/internal/shared/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func openTestPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	values, err := godotenv.Read(filepath.Join("..", "..", "..", ".env"))
	if err != nil {
		t.Fatal("a configured worktree .env is required")
	}
	for key, value := range values {
		t.Setenv(key, value)
	}
	cfg, err := config.LoadCenter()
	if err != nil {
		t.Fatal("invalid test configuration")
	}
	if err := db.CheckTestDatabase(cfg.Database.Name); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var name, marker string
	if err := pool.QueryRow(ctx, `SELECT current_database(),shobj_description(oid,'pg_database') FROM pg_database WHERE datname=current_database()`).Scan(&name, &marker); err != nil || name != cfg.Database.Name || marker != "workspaces-project:speedtest" {
		t.Fatal("worktree database identity could not be verified")
	}
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	return pool, ctx
}

func TestGetUsageToolReadsWorktreeDatabase(t *testing.T) {
	pool, ctx := openTestPool(t)
	now := time.Now().UTC()
	const node = "10000000-0000-4000-8000-000000000001"
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES ($1,'MCP node',100,50)`, node); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ip_addresses(ip) VALUES ('203.0.113.80')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,server,ip,threads,bytes_down,bytes_up,engine)
	 VALUES ('80000000-0000-4000-8000-000000000001',$1,$2,'ok',10,5,2,1,'{"name":"MCP fixture"}','203.0.113.80',4,700,300,'ookla')`, node, now); err != nil {
		t.Fatal(err)
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := New(&usage.Store{DB: pool}).Connect(ctx, serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "integration-test", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Errorf("close MCP client session: %v", err)
		}
	})
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_usage", Arguments: map[string]any{
		"node": node, "engine": "ookla", "from": usage.MonthStart(now).Format(time.RFC3339),
		"to": now.Add(time.Minute).Format(time.RFC3339), "group": "day",
	}})
	if err != nil || result.IsError {
		t.Fatalf("MCP call result = %+v, err = %v", result, err)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var got usage.Response
	if err := json.Unmarshal(encoded, &got); err != nil || got.Totals.Total != 1000 || got.MonthToDate.Total != 1000 || len(got.Series) != 1 {
		t.Fatalf("usage response = %s, err = %v", encoded, err)
	}
	t.Log("PASS: MCP tools/list and tools/call returned byte totals from the guarded worktree PostgreSQL database")
}
