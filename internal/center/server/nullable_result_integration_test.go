//go:build integration

package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/jooservices/speedtest/internal/center/agentapi"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/nodes"
)

func TestNullableResultIdentityIntegration(t *testing.T) {
	pool, ctx, key := openServerIntegrationPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	svc := nodes.NewService(pool, key)
	node, token := enrollIngestionNode(t, ctx, svc, "nullable-edge")
	hook := &recordingHook{}
	store := &agentapi.Postgres{Pool: pool, Hook: hook}
	handler := NewHandler(Options{NodeCredentials: svc, AgentAPI: store, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil))})
	var fields map[string]any
	_ = json.Unmarshal([]byte(ingestionResult), &fields)
	fields["ip"] = nil
	fields["status"] = "failed"
	fields["error"] = "speedtest: no_route"
	failed, _ := json.Marshal(fields)
	outcomes(t, ingestRequest(t, handler, token, "POST", "/api/agent/results", `{"results":[`+string(failed)+`]}`), 201)
	outcomes(t, ingestRequest(t, handler, token, "POST", "/api/agent/results", `{"results":[`+string(failed)+`]}`), 200)
	if hook.calls != 1 {
		t.Fatal("new failed NULL-IP result did not invoke the transactional alert hook exactly once")
	}
	var null bool
	if err := pool.QueryRow(ctx, `SELECT ip IS NULL FROM results WHERE id=$1`, fields["id"]).Scan(&null); err != nil || !null {
		t.Fatal("unknown IP not stored as NULL", err)
	}
	fields["id"] = "00000000-0000-4000-8000-000000000002"
	fields["status"] = "ok"
	fields["error"] = nil
	invalid, _ := json.Marshal(fields)
	outcomes(t, ingestRequest(t, handler, token, "POST", "/api/agent/results", `{"results":[`+string(invalid)+`]}`), 422)
	fields["ip"] = "203.0.113.4"
	fields["serverId"] = nil
	fields["server"] = map[string]any{"name": "LibreSpeed", "host": "speed.example"}
	automatic, _ := json.Marshal(fields)
	outcomes(t, ingestRequest(t, handler, token, "POST", "/api/agent/results", `{"results":[`+string(automatic)+`]}`), 201)
	if err := pool.QueryRow(ctx, `SELECT server_id IS NULL AND server->>'host'='speed.example' FROM results WHERE id=$1`, fields["id"]).Scan(&null); err != nil || !null {
		t.Fatal("server object not persisted", err)
	}
	if hook.calls != 2 {
		t.Fatal("known IP hook not invoked")
	}
	// Roll back later migrations, then refuse to lose the nullable-IP contract.
	for _, name := range []string{"geo provider usage", "Audit authentication source", "API access tokens", "failure class", "60-minute schedule default", "engine schedules", "two-factor", "audit log", "SSH installer", "notification deliveries", "alert engine", "history"} {
		if err := db.Migrate(ctx, pool, "down"); err != nil {
			t.Fatal(name+" migration rollback failed", err)
		}
	}
	// New-format rows cannot be represented under version 2. Refuse rollback atomically.
	if err := db.Migrate(ctx, pool, "down"); err == nil {
		t.Fatal("lossy rollback accepted")
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM results`).Scan(&count); err != nil || count != 2 {
		t.Fatal("failed rollback lost data", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM results WHERE node_id=$1`, node); err != nil {
		t.Fatal(err)
	}
	// A version-3 compatible row (no results.engine yet) survives both down and up; no data is discarded.
	const compatibleID = "00000000-0000-4000-8000-000000000001"
	if _, err := pool.Exec(ctx, `INSERT INTO ip_addresses(ip) VALUES ('203.0.113.1') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,ip,threads,server)
		VALUES ($1,$2,'2026-11-03T00:00:00Z','ok',123.4,56.7,3,1,'203.0.113.1',4,'{"id":1,"name":"edge","city":"city","sponsor":"isp","host":"test.example"}'::jsonb)`, compatibleID, node); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool, "down"); err != nil {
		t.Fatal(err)
	}
	// Exercise upgrade of legacy successful rows without a typed server object.
	if _, err := pool.Exec(ctx, `UPDATE results SET server=NULL WHERE id=$1`, compatibleID); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool, "up"); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT server_id=1 AND ip='203.0.113.1' AND server->>'name'='Server 1' FROM results WHERE id=$1`, compatibleID).Scan(&null); err != nil || !null {
		t.Fatal("compatible data changed or backfill missing", err)
	}
	t.Log("PASS: failed NULL IP accepted/stored/idempotent and invokes alert hook; ok NULL IP rejected; LibreSpeed server identity accepted/stored; null IP skips IP history work; populated down/up; lossy down refuses atomically")
}
