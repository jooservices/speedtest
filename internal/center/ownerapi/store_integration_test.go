//go:build integration

package ownerapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/shared/testenv"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/agentapi"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/shared/config"
)

func openPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	values, err := godotenv.Read(filepath.Join("..", "..", "..", ".env"))
	if err != nil {
		t.Fatal("a configured worktree .env is required")
	}
	testenv.Apply(t, values)
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
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	return pool, ctx
}

func uid(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

func seedNode(t *testing.T, ctx context.Context, pool *pgxpool.Pool, n int, name string, lastSeen *time.Time) string {
	t.Helper()
	id := uid(n)
	if _, err := pool.Exec(ctx, `INSERT INTO nodes (id,name,label,expected_down_mbps,expected_up_mbps,color_slot,last_seen_at,host,agent) VALUES ($1,$2,'lbl',300,100,$3,$4,'host','agent/1')`, id, name, n, lastSeen); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `WITH legacy AS (INSERT INTO schedules (node_id,interval_minutes,offset_minutes) VALUES ($1,30,$2) RETURNING *) INSERT INTO engine_schedules SELECT node_id,'ookla',interval_minutes,offset_minutes,enabled,revision FROM legacy`, id, n*3); err != nil {
		t.Fatal(err)
	}
	return id
}

func seedResult(t *testing.T, ctx context.Context, pool *pgxpool.Pool, n int, node string, ts time.Time, status, ip string) {
	t.Helper()
	if ip != "" {
		geo := `{"city":"Hanoi","country":"VN","latitude":21.0285,"longitude":105.8542,"time_zone":"Asia/Ho_Chi_Minh","accuracy_radius":20,"asn":7552,"organisation":"Viettel"}`
		if _, err := pool.Exec(ctx, `INSERT INTO ip_addresses (ip,geo,source) VALUES ($1,$2::jsonb,'GeoLite2') ON CONFLICT DO NOTHING`, ip, geo); err != nil {
			t.Fatal(err)
		}
	}
	var ipArg any
	if ip != "" {
		ipArg = ip
	}
	if status == "failed" {
		_, err := pool.Exec(ctx, `INSERT INTO results (id,node_id,ts,status,ip,threads,error,server) VALUES ($1,$2,$3,'failed',$4,4,'link down','{"id":1,"name":"S","city":"C","sponsor":"P"}')`, uid(1000+n), node, ts, ipArg)
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	_, err := pool.Exec(ctx, `INSERT INTO results (id,node_id,ts,status,down_mbps,up_mbps,ping_ms,jitter_ms,loss_percent,server_id,server,ip,threads,bytes_down,bytes_up,duration_seconds,manual,raw,warnings)
		VALUES ($1,$2,$3,$4,250.5,90.25,12.5,1.5,0,9,'{"id":9,"name":"S","city":"C","sponsor":"P"}',$5,4,1000,500,30,false,'{"k":"v"}','["w"]')`, uid(1000+n), node, ts, status, ipArg)
	if err != nil {
		t.Fatal(err)
	}
}

func TestStoreListGetAndStatus(t *testing.T) {
	pool, ctx := openPool(t)
	s := &Store{DB: pool, Now: func() time.Time { return time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC) }}
	now := s.Now()
	fresh := now.Add(-20 * time.Second)
	stale := now.Add(-time.Hour)
	b := seedNode(t, ctx, pool, 2, "bravo", &fresh)
	a := seedNode(t, ctx, pool, 1, "Alpha", &fresh)
	c := seedNode(t, ctx, pool, 3, "charlie", &stale)
	d := seedNode(t, ctx, pool, 4, "delta", nil)
	if _, err := pool.Exec(ctx, `UPDATE nodes SET enabled=false WHERE id=$1`, d); err != nil {
		t.Fatal(err)
	}
	seedResult(t, ctx, pool, 1, a, now.Add(-10*time.Minute), "ok", "203.0.113.5")
	seedResult(t, ctx, pool, 2, a, now.Add(-5*time.Minute), "failed", "203.0.113.7")
	seedResult(t, ctx, pool, 3, b, now.Add(-time.Minute), "degraded", "203.0.113.6")
	seedResult(t, ctx, pool, 4, b, now.Add(-2*time.Minute), "ok", "203.0.113.5")

	items, total, err := s.List(ctx, 1, 3)
	if err != nil || total != 4 || len(items) != 3 {
		t.Fatalf("page 1: %d items total %d err %v", len(items), total, err)
	}
	if items[0]["name"] != "Alpha" || items[1]["name"] != "bravo" || items[2]["name"] != "charlie" {
		t.Fatalf("order = %v %v %v (want case-insensitive by name)", items[0]["name"], items[1]["name"], items[2]["name"])
	}
	second, _, err := s.List(ctx, 2, 3)
	if err != nil || len(second) != 1 || second[0]["name"] != "delta" {
		t.Fatalf("page 2 = %v err %v", second, err)
	}
	if empty, total, err := s.List(ctx, 9, 3); err != nil || len(empty) != 0 || total != 4 {
		t.Fatalf("beyond the last page: %d items total %d err %v", len(empty), total, err)
	}
	alpha := items[0]
	if alpha["status"] != "failed" || alpha["currentIp"] != "203.0.113.7" || alpha["geo"] == nil {
		t.Fatalf("alpha: status %v ip %v geo %v (the newest result decides the current address)", alpha["status"], alpha["currentIp"], alpha["geo"])
	}
	if alpha["latest"].(map[string]any)["status"] != "failed" || alpha["latestGood"].(map[string]any)["status"] != "ok" {
		t.Fatalf("alpha latest/latestGood = %v / %v", alpha["latest"], alpha["latestGood"])
	}
	bravo := items[1]
	if bravo["status"] != "degraded" || bravo["currentIp"] != "203.0.113.6" || bravo["latestGood"].(map[string]any)["down"] != 250.5 {
		t.Fatalf("bravo = %v", bravo)
	}
	r := bravo["latest"].(map[string]any)
	if r["serverId"] != int64(9) || r["server"].(map[string]any)["name"] != "S" || r["raw"].(map[string]any)["k"] != "v" || len(r["warnings"].([]any)) != 1 || r["ip"] != "203.0.113.6" {
		t.Fatalf("result view = %v", r)
	}
	if items[2]["status"] != "offline" || second[0]["status"] != "paused" || items[2]["latest"] != nil || items[2]["currentIp"] != nil {
		t.Fatalf("charlie/delta = %v / %v", items[2]["status"], second[0]["status"])
	}
	one, etag, err := s.Get(ctx, c)
	if err != nil || etag != ETag(1) || one["name"] != "charlie" || one["offset"] != int64(9) {
		t.Fatalf("get = %v etag %q err %v", one, etag, err)
	}
	if _, _, err := s.Get(ctx, uid(99)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown node error = %v", err)
	}
}

func TestStoreUpdate(t *testing.T) {
	pool, ctx := openPool(t)
	s := &Store{DB: pool}
	id := seedNode(t, ctx, pool, 1, "alpha", nil)
	seedNode(t, ctx, pool, 2, "bravo", nil)
	patch := raw

	node, etag, err := s.Update(ctx, id, patch(map[string]string{"label": `"Home fiber"`, "expectedDown": `500`, "server": `33`, "enabled": `false`, "binding": `{"iface":"eth1","sourceIp":"192.0.2.7"}`}), ETag(1))
	if err != nil || etag != ETag(2) || node["label"] != "Home fiber" || node["expectedDown"] != float64(500) || node["server"] != int64(33) || node["enabled"] != false || node["status"] != "paused" ||
		node["binding"].(map[string]any)["iface"] != "eth1" || node["binding"].(map[string]any)["sourceIp"] != "192.0.2.7" {
		t.Fatalf("update = %v etag %q err %v", node, etag, err)
	}
	if _, _, err := s.Update(ctx, id, patch(map[string]string{"note": `"x"`}), ETag(1)); !errors.Is(err, ErrStale) {
		t.Fatalf("stale If-Match error = %v", err)
	}
	if _, _, err := s.Update(ctx, id, patch(map[string]string{"name": `"bravo"`}), ETag(2)); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate name error = %v", err)
	}
	for name, p := range map[string]map[string]string{"threads zero": {"threads": `0`}, "blank name": {"name": `"  "`}, "negative speed": {"expectedUp": `-5`}, "unknown type": {"enabled": `"no"`}} {
		if _, _, err := s.Update(ctx, id, patch(p), ETag(2)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: error = %v, want ErrInvalid", name, err)
		}
	}
	if _, _, err := s.Update(ctx, id, patch(map[string]string{"server": `"auto"`, "binding": `{"sourceIp":null}`, "enabled": `true`}), ETag(2)); err != nil {
		t.Fatalf("clear server and source IP: %v", err)
	}
	if _, _, err := s.Update(ctx, uid(99), patch(map[string]string{"note": `"x"`}), ETag(1)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown node error = %v", err)
	}
	var rev int64
	if err := pool.QueryRow(ctx, `SELECT config_revision FROM nodes WHERE id=$1`, id).Scan(&rev); err != nil || rev != 3 {
		t.Fatalf("revision = %d (a rejected update must not bump it) err %v", rev, err)
	}
}

func TestLegacyAndPerEnginePinsStayCompatibleIntegration(t *testing.T) {
	pool, ctx := openPool(t)
	nodeID := seedNode(t, ctx, pool, 1, "pin-compat", nil)
	if _, err := pool.Exec(ctx, `UPDATE nodes SET installed_engines=ARRAY['ookla']::text[] WHERE id=$1`, nodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO node_engine_servers(node_id,engine,servers) VALUES($1,'ookla',
		'[{"id":41,"name":"First","city":"Hanoi","isp":"ISP","host":"one.example"},{"id":42,"name":"Second","city":"Hue","isp":"ISP","host":"two.example"}]')`, nodeID); err != nil {
		t.Fatal(err)
	}
	owner := &Store{DB: pool}
	_, tag, err := owner.Update(ctx, nodeID, raw(map[string]string{"server": `41`}), ETag(1))
	if err != nil {
		t.Fatal(err)
	}
	assertAgentPinCompatible(t, ctx, pool, nodeID, "41")
	second := int64(42)
	_, tag, err = owner.SelectServer(ctx, nodeID, "ookla", &second, tag)
	if err != nil {
		t.Fatal(err)
	}
	assertAgentPinCompatible(t, ctx, pool, nodeID, "42")
	_, tag, err = owner.Update(ctx, nodeID, raw(map[string]string{"server": `"auto"`}), tag)
	if err != nil {
		t.Fatal(err)
	}
	assertAgentPinCompatible(t, ctx, pool, nodeID, "auto")
	_, _, err = owner.SelectServer(ctx, nodeID, "ookla", nil, tag)
	if err != nil {
		t.Fatal(err)
	}
	assertAgentPinCompatible(t, ctx, pool, nodeID, "auto")
}

func TestResultSelectionSnapshotsStayHistoricalAfterPinChangesIntegration(t *testing.T) {
	pool, ctx := openPool(t)
	nodeID := seedNode(t, ctx, pool, 1, "pin-history", nil)
	if _, err := pool.Exec(ctx, `INSERT INTO node_engine_servers(node_id,engine,pinned_server_id) VALUES($1,'ookla',10)`, nodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE node_engine_servers SET pinned_server_id=20 WHERE node_id=$1 AND engine='ookla'`, nodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE node_engine_servers SET pinned_server_id=NULL WHERE node_id=$1 AND engine='ookla'`, nodeID); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	seedResult(t, ctx, pool, 90, nodeID, now.Add(-3*time.Minute), "failed", "")
	seedResult(t, ctx, pool, 91, nodeID, now.Add(-2*time.Minute), "failed", "")
	seedResult(t, ctx, pool, 92, nodeID, now.Add(-time.Minute), "failed", "")
	for _, snapshot := range []struct {
		id    string
		pin   int
		known bool
	}{{uid(1091), 10, true}, {uid(1092), 20, true}} {
		if _, err := pool.Exec(ctx, `UPDATE results SET pinned_server_id_at_test=$2,server_selection_known=$3 WHERE id=$1`, snapshot.id, snapshot.pin, snapshot.known); err != nil {
			t.Fatal(err)
		}
	}
	items, _, err := (&Store{DB: pool}).ListResults(ctx, ResultFilter{Nodes: []string{nodeID}, NodesSet: true, AllEngines: true}, 1, 10)
	if err != nil || len(items) != 3 {
		t.Fatalf("historical results=%v err=%v", items, err)
	}
	byID := make(map[string]map[string]any, len(items))
	for _, item := range items {
		byID[item["id"].(string)] = item
	}
	if byID[uid(1090)]["serverSelectionKnown"] != false || byID[uid(1090)]["pinnedServerId"] != nil ||
		byID[uid(1091)]["serverSelectionKnown"] != true || byID[uid(1091)]["pinnedServerId"] != int64(10) ||
		byID[uid(1092)]["serverSelectionKnown"] != true || byID[uid(1092)]["pinnedServerId"] != int64(20) {
		t.Fatalf("current cleared pin rewrote historical selections: %v", byID)
	}
}

func TestLegacyPatchRollsBackWhenPerEnginePinSyncFailsIntegration(t *testing.T) {
	pool, ctx := openPool(t)
	nodeID := seedNode(t, ctx, pool, 1, "pin-sync-rollback", nil)
	if _, err := pool.Exec(ctx, `INSERT INTO node_engine_servers(node_id,engine,servers) VALUES($1,'ookla','[{"id":41,"name":"First"}]')`, nodeID); err != nil {
		t.Fatal(err)
	}
	createServerWriteFailureFunction(t, ctx, pool)
	deferServerWriteFailureCleanup(t, pool)
	createServerFailureTrigger(t, ctx, pool, `CREATE TRIGGER fail_pin BEFORE UPDATE ON node_engine_servers
		FOR EACH ROW WHEN (NEW.node_id='`+nodeID+`'::uuid) EXECUTE FUNCTION fail_server_write()`)
	if _, _, err := (&Store{DB: pool}).Update(ctx, nodeID, raw(map[string]string{"server": `41`}), ETag(1)); err == nil {
		t.Fatal("legacy patch succeeded despite per-engine pin storage failure")
	}
	var legacy *int64
	var pin *int64
	var revision int64
	if err := pool.QueryRow(ctx, `SELECT n.server_id,c.pinned_server_id,n.config_revision FROM nodes n JOIN node_engine_servers c ON c.node_id=n.id WHERE n.id=$1`, nodeID).
		Scan(&legacy, &pin, &revision); err != nil || legacy != nil || pin != nil || revision != 1 {
		t.Fatalf("failed legacy patch left split pins: legacy=%v per-engine=%v revision=%d err=%v", legacy, pin, revision, err)
	}
}

func assertAgentPinCompatible(t *testing.T, ctx context.Context, pool *pgxpool.Pool, nodeID, want string) {
	t.Helper()
	config, err := (&agentapi.Postgres{Pool: pool}).Config(ctx, nodeID)
	if err != nil || fmt.Sprint(config["server"]) != want {
		t.Fatalf("legacy agent pin=%v err=%v, want %s", config["server"], err, want)
	}
	var schedules []map[string]any
	if err := json.Unmarshal(config["schedules"].(json.RawMessage), &schedules); err != nil || len(schedules) != 1 || fmt.Sprint(schedules[0]["serverId"]) != want {
		t.Fatalf("current agent schedules=%v err=%v, want pin %s", schedules, err, want)
	}
}

func TestStoreDeleteCascadesAndIsRepeatable(t *testing.T) {
	pool, ctx := openPool(t)
	s := &Store{DB: pool}
	id := seedNode(t, ctx, pool, 1, "alpha", nil)
	keep := seedNode(t, ctx, pool, 2, "bravo", nil)
	seedResult(t, ctx, pool, 1, id, time.Now().UTC(), "ok", "203.0.113.5")
	seedResult(t, ctx, pool, 2, keep, time.Now().UTC(), "ok", "203.0.113.5")
	if _, err := s.QueueCommand(ctx, id, "run_now", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO node_credentials (id,node_id,kind,token_hash,expires_at) VALUES ($1,$2,'enrollment','h1',now()+interval '1 hour')`, uid(500), id); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, id, ETag(99)); !errors.Is(err, ErrStale) {
		t.Fatalf("stale delete error = %v", err)
	}
	if err := s.Delete(ctx, id, ETag(1)); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"results", "agent_commands", "node_credentials", "schedules"} {
		var n int
		col := "node_id"
		if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s=$1`, table, col), id).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s still has %d rows of the deleted node (err %v)", table, n, err)
		}
	}
	var others, ips int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM results WHERE node_id=$1), (SELECT count(*) FROM ip_addresses)`, keep).Scan(&others, &ips); err != nil || others != 1 || ips != 1 {
		t.Fatalf("other node's data and the shared address record must stay: results %d addresses %d err %v", others, ips, err)
	}
	if err := s.Delete(ctx, id, ETag(1)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("repeated delete error = %v, want ErrNotFound", err)
	}
}

func TestStoreCommands(t *testing.T) {
	pool, ctx := openPool(t)
	s := &Store{DB: pool}
	id := seedNode(t, ctx, pool, 1, "alpha", nil)
	run, err := s.QueueCommand(ctx, id, "run_now", "")
	if err != nil || run["type"] != "run_now" || run["status"] != "pending" || run["nodeId"] != id || run["resultId"] != nil {
		t.Fatalf("run_now = %v err %v", run, err)
	}
	if _, err := s.QueueCommand(ctx, id, "run_now", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("second run_now while one is pending: %v, want ErrConflict", err)
	}
	got, err := s.Command(ctx, id, run["id"].(string))
	if err != nil || got["id"] != run["id"] {
		t.Fatalf("command = %v err %v", got, err)
	}
	if _, err := s.Command(ctx, uid(98), run["id"].(string)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("command of another node: %v", err)
	}
	if _, err := s.Command(ctx, id, uid(97)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown command: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_commands SET status='succeeded' WHERE id=$1`, run["id"]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueCommand(ctx, id, "run_now", ""); err != nil {
		t.Fatalf("run_now after the previous one finished: %v", err)
	}
	var rev int64
	pause, err := s.QueueCommand(ctx, id, "pause", "")
	if err != nil || pause["type"] != "pause" {
		t.Fatal(err)
	}
	var enabled bool
	if err := pool.QueryRow(ctx, `SELECT enabled, config_revision FROM nodes WHERE id=$1`, id).Scan(&enabled, &rev); err != nil || enabled || rev != 2 {
		t.Fatalf("after pause: enabled=%v revision=%d err %v", enabled, rev, err)
	}
	if _, err := s.QueueCommand(ctx, id, "run_now", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("run_now on a paused node: %v, want ErrConflict", err)
	}
	if _, err := s.QueueCommand(ctx, id, "resume", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueCommand(ctx, id, "update_config", ""); err != nil {
		t.Fatal(err)
	}
	upd, err := s.QueueCommand(ctx, id, "update_agent", "1.4.3")
	if err != nil || upd["version"] != "1.4.3" {
		t.Fatalf("update_agent = %v err %v", upd, err)
	}
	if err := pool.QueryRow(ctx, `SELECT enabled, config_revision FROM nodes WHERE id=$1`, id).Scan(&enabled, &rev); err != nil || !enabled || rev != 4 {
		t.Fatalf("after resume and update_config: enabled=%v revision=%d err %v", enabled, rev, err)
	}
	if _, err := s.QueueCommand(ctx, uid(96), "run_now", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("command for an unknown node: %v", err)
	}
}

func TestCanceledNodeRequestsDoNotMutateIntegration(t *testing.T) {
	pool, ctx := openPool(t)
	id := seedNode(t, ctx, pool, 1, "Cancellation fixture", nil)
	store := &Store{DB: pool}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if rows, total, err := store.List(canceled, 1, 20); !errors.Is(err, context.Canceled) || rows != nil || total != 0 {
		t.Fatalf("canceled list: %v %d %v", rows, total, err)
	}
	if row, tag, err := store.Update(canceled, id, raw(map[string]string{"name": `"Must not persist"`}), ETag(1)); !errors.Is(err, context.Canceled) || row != nil || tag != "" {
		t.Fatalf("canceled update: %v %q %v", row, tag, err)
	}
	if err := store.Delete(canceled, id, ETag(1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled delete: %v", err)
	}
	if command, err := store.QueueCommand(canceled, id, "pause", ""); !errors.Is(err, context.Canceled) || command != nil {
		t.Fatalf("canceled command: %v %v", command, err)
	}
	var name string
	var enabled bool
	var commands int
	if err := pool.QueryRow(ctx, `SELECT name,enabled,(SELECT count(*) FROM agent_commands WHERE node_id=$1) FROM nodes WHERE id=$1`, id).Scan(&name, &enabled, &commands); err != nil {
		t.Fatal(err)
	}
	if name != "Cancellation fixture" || !enabled || commands != 0 {
		t.Fatalf("cancellation changed persisted state: %q %t commands=%d", name, enabled, commands)
	}
}
