//go:build integration

package agentapi

import (
	"encoding/json"
	"strconv"
	"testing"
)

func TestLibreSpeedResultResolvesExactEndpointWhenHostsMatchIntegration(t *testing.T) {
	pool, ctx := openAgentAPIIntegrationDB(t)
	const node = "00000000-0000-4000-8000-000000000076"
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES($1,'Endpoint identity',100,50)`, node); err != nil {
		t.Fatal("could not create result node")
	}
	servers, err := json.Marshal([]ServerOption{
		{ID: 51, Name: "First path", Host: "shared.example.test", Endpoint: "https://shared.example.test/a"},
		{ID: 52, Name: "Second path", Host: "shared.example.test", Endpoint: "https://shared.example.test/b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO node_engine_servers(node_id,engine,servers,pinned_server_id) VALUES($1,'librespeed',$2::jsonb,51)`, node, servers); err != nil {
		t.Fatal("could not create shared-host catalog")
	}
	result, err := ParseResult(libreSpeedResultWithEndpoint("00000000-0000-4000-8000-000000000077", "https://shared.example.test/b", 51))
	if err != nil {
		t.Fatal("could not parse result endpoint", err)
	}
	if duplicate, err := (&Postgres{Pool: pool}).SaveResult(ctx, node, result); err != nil || duplicate {
		t.Fatalf("save result duplicate=%t err=%v", duplicate, err)
	}
	var actual *int64
	var endpoint string
	if err := pool.QueryRow(ctx, `SELECT server_id,server->>'endpoint' FROM results WHERE id=$1`, result.ID).Scan(&actual, &endpoint); err != nil || actual == nil || *actual != 52 || endpoint != "https://shared.example.test/b" {
		t.Fatalf("stored identity id=%v endpoint=%q err=%v", actual, endpoint, err)
	}
}

func libreSpeedResultWithEndpoint(id, endpoint string, pinned int64) []byte {
	return []byte(`{"engine":"librespeed","id":"` + id + `","ts":"2026-11-11T12:00:00Z","status":"ok","down":100,"up":50,"ping":3,"jitter":null,"dLat":null,"uLat":null,"loss":null,"serverId":null,"ip":"203.0.113.5","threads":4,"bytesDown":1000,"bytesUp":500,"duration":1,"error":null,"manual":false,"server":{"name":"Second path","city":"","sponsor":"","host":"shared.example.test","endpoint":"` + endpoint + `"},"serverSelectionKnown":true,"pinnedServerId":` + jsonNumber(pinned) + `}`)
}

func jsonNumber(value int64) string { return strconv.FormatInt(value, 10) }
