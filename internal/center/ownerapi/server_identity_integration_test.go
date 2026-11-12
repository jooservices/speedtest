//go:build integration

package ownerapi

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jooservices/speedtest/internal/center/agentapi"
)

func TestDelayedLibreSpeedResultKeepsMeasurementIdentityAfterCatalogRefreshIntegration(t *testing.T) {
	ctx, pool, fx := seedFixture(t)
	seedSharedHostCatalog(t, ctx, pool, fx.alpha)
	result := delayedLibreSpeedResult(fx.now.Add(-30*time.Minute), 52, 51)
	refreshSharedHostCatalog(t, ctx, pool, fx.alpha)
	if duplicate, err := (&agentapi.Postgres{Pool: pool}).SaveResult(ctx, fx.alpha, result); err != nil || duplicate {
		t.Fatalf("save delayed result duplicate=%t err=%v", duplicate, err)
	}
	filter := ResultFilter{Engine: "librespeed", From: fx.now.Add(-time.Hour), To: fx.now.Add(time.Hour), NodesSet: true, Nodes: []string{fx.alpha}, Status: "all"}
	items, _, err := fx.store.ListResults(ctx, filter, 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	assertMeasurementServer(t, findResult(t, items, result.ID))
	detail, err := fx.store.GetResult(ctx, result.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertMeasurementServer(t, detail)
}

func seedSharedHostCatalog(t *testing.T, ctx context.Context, pool *pgxpool.Pool, node string) {
	t.Helper()
	servers, err := json.Marshal([]agentapi.ServerOption{
		{ID: 51, Name: "Hanoi", Host: "shared.example.test", Endpoint: "https://shared.example.test/a"},
		{ID: 52, Name: "Singapore", Host: "shared.example.test", Endpoint: "https://shared.example.test/b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO node_engine_servers(node_id,engine,servers,pinned_server_id) VALUES($1,'librespeed',$2::jsonb,51)`, node, servers); err != nil {
		t.Fatal("could not seed server catalog", err)
	}
}

func refreshSharedHostCatalog(t *testing.T, ctx context.Context, pool *pgxpool.Pool, node string) {
	t.Helper()
	servers := `[{"id":51,"name":"Hanoi refreshed","host":"shared.example.test","endpoint":"https://shared.example.test/a-new"},{"id":52,"name":"Singapore refreshed","host":"shared.example.test","endpoint":"https://shared.example.test/b-new"}]`
	if _, err := pool.Exec(ctx, `UPDATE node_engine_servers SET servers=$2::jsonb,refreshed_at=clock_timestamp() WHERE node_id=$1 AND engine='librespeed'`, node, servers); err != nil {
		t.Fatal("could not refresh server catalog", err)
	}
}

func delayedLibreSpeedResult(ts time.Time, used, pinned int) agentapi.Result {
	down, up, ping, actual := 100.0, 50.0, 4.0, used
	ip := "203.0.113.50"
	return agentapi.Result{
		Engine: "librespeed", ID: uid(1005), TS: ts.UTC().Format(time.RFC3339Nano), Status: "ok",
		Down: &down, Up: &up, Ping: &ping, ServerID: &actual, IP: &ip,
		Threads: 4, BytesDown: 1000, BytesUp: 500, Duration: 1, Raw: json.RawMessage(`{}`),
		Server:               &agentapi.Server{ID: &actual, Name: "Singapore", City: "Singapore", Host: "shared.example.test", Endpoint: "https://shared.example.test/b"},
		Warnings:             []string{"Pinned server was unreachable; the engine selected a server automatically."},
		ServerSelectionKnown: true, PinnedServerID: int64Pointer(int64(pinned)),
	}
}

func int64Pointer(value int64) *int64 { return &value }

func findResult(t *testing.T, results []map[string]any, id string) map[string]any {
	t.Helper()
	for _, result := range results {
		if result["id"] == id {
			return result
		}
	}
	t.Fatalf("result %s not found in history", id)
	return nil
}

func assertMeasurementServer(t *testing.T, result map[string]any) {
	t.Helper()
	server, ok := result["server"].(map[string]any)
	if result["serverId"] != int64(52) || result["pinnedServerId"] != int64(51) || !ok || server["endpoint"] != "https://shared.example.test/b" {
		t.Fatalf("measurement-time server mismatch was lost: %#v", result)
	}
}
