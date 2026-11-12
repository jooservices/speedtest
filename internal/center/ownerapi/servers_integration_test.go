//go:build integration

package ownerapi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jooservices/speedtest/internal/center/agentapi"
)

func TestServerCatalogSelectionCommandsAndAgentConfig(t *testing.T) {
	pool, ctx := openPool(t)
	id := seedNode(t, ctx, pool, 7, "server-catalog", nil)
	if _, err := pool.Exec(ctx, `UPDATE nodes SET installed_engines=ARRAY['ookla','librespeed']::text[] WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO engine_schedules(node_id,engine,interval_minutes,enabled) VALUES($1,'librespeed',60,true)`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO node_engine_servers(node_id,engine,servers,refreshed_at)
VALUES($1,'ookla','[{"id":54321,"name":"Hanoi","city":"Hanoi","isp":"Fiber","host":"hn.example","distanceKm":22.8}]',now()),
($1,'librespeed','[{"id":7,"name":"Singapore","city":"Singapore","isp":"Community","host":"sg.example"}]',now())`, id); err != nil {
		t.Fatal(err)
	}
	store := &Store{DB: pool}
	catalog, tag, err := store.ServerCatalog(ctx, id, "ookla")
	refreshed, hasTimestamp := catalog["refreshedAt"].(string)
	if err != nil || tag != ETag(1) || len(catalog["servers"].([]agentapi.ServerOption)) != 1 || !hasTimestamp || !strings.HasSuffix(refreshed, "Z") {
		t.Fatalf("Ookla catalog=%v tag=%q err=%v", catalog, tag, err)
	}
	pin := int64(54321)
	if _, _, err := store.SelectServer(ctx, id, "ookla", &pin, ETag(99)); !errors.Is(err, ErrStale) {
		t.Fatalf("stale selection error=%v", err)
	}
	unknownID := int64(99)
	if _, _, err := store.SelectServer(ctx, id, "ookla", &unknownID, tag); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown catalog selection error=%v", err)
	}
	selected, next, err := store.SelectServer(ctx, id, "ookla", &pin, tag)
	if err != nil || next != ETag(2) || selected["serverId"] != pin {
		t.Fatalf("Ookla selection=%v tag=%q err=%v", selected, next, err)
	}
	libreID := int64(7)
	if _, next, err = store.SelectServer(ctx, id, "librespeed", &libreID, next); err != nil || next != ETag(3) {
		t.Fatalf("LibreSpeed selection ETag=%q err=%v", next, err)
	}
	var legacy *int64
	if err := pool.QueryRow(ctx, `SELECT server_id::bigint FROM nodes WHERE id=$1`, id).Scan(&legacy); err != nil || legacy == nil || *legacy != pin {
		t.Fatalf("legacy primary pin=%v err=%v", legacy, err)
	}
	command, err := store.QueueServerList(ctx, id, "librespeed")
	if err != nil || command["type"] != "list_servers" || command["engine"] != "librespeed" {
		t.Fatalf("queued discovery=%v err=%v", command, err)
	}
	if _, err := store.QueueServerList(ctx, id, "librespeed"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate discovery error=%v", err)
	}
	center := &agentapi.Postgres{Pool: pool}
	config, err := center.Config(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	scheduleBytes, err := json.Marshal(config["schedules"])
	if err != nil || !strings.Contains(string(scheduleBytes), `"serverId":54321`) || !strings.Contains(string(scheduleBytes), `"serverId":7`) {
		t.Fatalf("agent config schedules=%s err=%v", scheduleBytes, err)
	}
	t.Log("PASS: catalog read and ETag; stale and unknown pin refused; per-engine pin saved without mixing; primary legacy field stays compatible; discovery command carries engine; duplicate refresh refused; both pins appear in agent config")
}

func TestAutomaticSelectionCreatesEmptyCatalogIntegration(t *testing.T) {
	pool, ctx := openPool(t)
	id := seedNode(t, ctx, pool, 9, "automatic-server", nil)
	store := &Store{DB: pool}
	if _, tag, err := store.ServerCatalog(ctx, id, "ookla"); err != nil || tag != ETag(1) {
		t.Fatalf("empty catalog ETag=%q err=%v", tag, err)
	}
	selected, tag, err := store.SelectServer(ctx, id, "ookla", nil, ETag(1))
	if err != nil || tag != ETag(2) || selected["serverId"] != "auto" {
		t.Fatalf("automatic selection=%v tag=%q err=%v", selected, tag, err)
	}
	t.Log("PASS: automatic selection works before a server list has been discovered")
}

func TestServerCatalogRejectsInvalidStateIntegration(t *testing.T) {
	pool, ctx := openPool(t)
	id := seedNode(t, ctx, pool, 1, "server-catalog-invalid", nil)
	store := &Store{DB: pool}
	if _, _, err := store.ServerCatalog(ctx, id, "unknown"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid engine error = %v", err)
	}
	if _, _, err := store.ServerCatalog(ctx, "00000000-0000-4000-8000-000000000000", "ookla"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing node error = %v", err)
	}
	if _, _, err := store.ServerCatalog(ctx, id, "librespeed"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("uninstalled engine error = %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO node_engine_servers(node_id,engine,servers) VALUES($1,'ookla','["invalid"]')`, id); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ServerCatalog(ctx, id, "ookla"); err == nil {
		t.Fatal("catalog with an invalid JSON shape was accepted")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := store.ServerCatalog(canceled, id, "ookla"); err == nil {
		t.Fatal("canceled catalog query succeeded")
	}
	t.Log("PASS: invalid engine, missing node, uninstalled engine, malformed stored catalog, and canceled read are rejected")
}

func TestServerSelectionRejectsInvalidStateIntegration(t *testing.T) {
	pool, ctx := openPool(t)
	id := seedNode(t, ctx, pool, 2, "server-selection-invalid", nil)
	store := &Store{DB: pool}
	zero := int64(0)
	if _, _, err := store.SelectServer(ctx, id, "unknown", nil, ETag(1)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid engine error = %v", err)
	}
	if _, _, err := store.SelectServer(ctx, id, "ookla", &zero, ETag(1)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid server ID error = %v", err)
	}
	if _, _, err := store.SelectServer(ctx, "00000000-0000-4000-8000-000000000000", "ookla", nil, ETag(1)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing node error = %v", err)
	}
	if _, _, err := store.SelectServer(ctx, id, "librespeed", nil, ETag(1)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("uninstalled engine error = %v", err)
	}
	unknown := int64(1)
	if _, _, err := store.SelectServer(ctx, id, "ookla", &unknown, ETag(1)); !errors.Is(err, ErrConflict) {
		t.Fatalf("pin before discovery error = %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := store.SelectServer(canceled, id, "ookla", nil, ETag(1)); err == nil {
		t.Fatal("canceled selection succeeded")
	}
	t.Log("PASS: invalid engine and ID, missing node, uninstalled engine, undiscovered pin, and canceled selection are rejected")
}

func TestQueueServerListRejectsInvalidStateIntegration(t *testing.T) {
	pool, ctx := openPool(t)
	id := seedNode(t, ctx, pool, 3, "server-list-invalid", nil)
	store := &Store{DB: pool}
	if _, err := store.QueueServerList(ctx, id, "unknown"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid engine error = %v", err)
	}
	if _, err := store.QueueServerList(ctx, "00000000-0000-4000-8000-000000000000", "ookla"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing node error = %v", err)
	}
	if _, err := store.QueueServerList(ctx, id, "librespeed"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("uninstalled engine error = %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.QueueServerList(canceled, id, "ookla"); err == nil {
		t.Fatal("canceled discovery queue succeeded")
	}
	t.Log("PASS: invalid engine, missing node, uninstalled engine, and canceled discovery queue are rejected")
}

func TestServerSelectionRollsBackStorageFailuresIntegration(t *testing.T) {
	pool, ctx := openPool(t)
	primary := seedNode(t, ctx, pool, 4, "server-selection-primary-error", nil)
	secondary := seedNode(t, ctx, pool, 5, "server-selection-secondary-error", nil)
	if _, err := pool.Exec(ctx, `UPDATE nodes SET installed_engines=ARRAY['ookla','librespeed'] WHERE id=ANY($1::uuid[])`, []string{primary, secondary}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO node_engine_servers(node_id,engine,servers) VALUES
($1,'ookla','[{"id":1,"name":"Primary"}]'),($2,'librespeed','[{"id":1,"name":"Secondary"}]')`, primary, secondary); err != nil {
		t.Fatal(err)
	}
	createServerWriteFailureFunction(t, ctx, pool)
	deferServerWriteFailureCleanup(t, pool)
	createServerFailureTrigger(t, ctx, pool, `CREATE TRIGGER fail_pin BEFORE UPDATE OF pinned_server_id ON node_engine_servers
FOR EACH ROW WHEN (NEW.node_id='`+primary+`'::uuid) EXECUTE FUNCTION fail_server_write()`)
	if err := selectServerFailure(ctx, &Store{DB: pool}, primary, "ookla"); err == nil {
		t.Fatal("server catalog update failure was ignored")
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER fail_pin ON node_engine_servers`); err != nil {
		t.Fatal(err)
	}
	createServerFailureTrigger(t, ctx, pool, `CREATE TRIGGER fail_node BEFORE UPDATE ON nodes
FOR EACH ROW WHEN (NEW.id IN ('`+primary+`'::uuid,'`+secondary+`'::uuid)) EXECUTE FUNCTION fail_server_write()`)
	if err := selectServerFailure(ctx, &Store{DB: pool}, primary, "ookla"); err == nil {
		t.Fatal("primary node update failure was ignored")
	}
	if err := selectServerFailure(ctx, &Store{DB: pool}, secondary, "librespeed"); err == nil {
		t.Fatal("secondary node update failure was ignored")
	}
	t.Log("PASS: failed catalog and node updates return errors and roll back pinned-server writes")
}

func TestQueueServerListRollsBackStorageFailuresIntegration(t *testing.T) {
	pool, ctx := openPool(t)
	id := seedNode(t, ctx, pool, 6, "server-list-storage-error", nil)
	createServerWriteFailureFunction(t, ctx, pool)
	deferServerWriteFailureCleanup(t, pool)
	createServerFailureTrigger(t, ctx, pool, `CREATE TRIGGER fail_list_insert BEFORE INSERT ON agent_commands
FOR EACH ROW WHEN (NEW.node_id='`+id+`'::uuid AND NEW.type='list_servers') EXECUTE FUNCTION fail_server_write()`)
	if _, err := (&Store{DB: pool}).QueueServerList(ctx, id, "ookla"); err == nil {
		t.Fatal("server list insert failure was ignored")
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER fail_list_insert ON agent_commands`); err != nil {
		t.Fatal(err)
	}
	createServerFailureTrigger(t, ctx, pool, `CREATE CONSTRAINT TRIGGER fail_list_commit AFTER INSERT ON agent_commands
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.node_id='`+id+`'::uuid AND NEW.type='list_servers')
EXECUTE FUNCTION fail_server_write()`)
	if _, err := (&Store{DB: pool}).QueueServerList(ctx, id, "ookla"); err == nil {
		t.Fatal("server list commit failure was ignored")
	}
	t.Log("PASS: failed discovery insert and deferred commit return errors")
}

func selectServerFailure(ctx context.Context, store *Store, node, engine string) error {
	id := int64(1)
	_, _, err := store.SelectServer(ctx, node, engine, &id, ETag(1))
	return err
}

func createServerWriteFailureFunction(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `CREATE OR REPLACE FUNCTION fail_server_write() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'injected server storage failure'; END $$`); err != nil {
		t.Fatal(err)
	}
}

func createServerFailureTrigger(t *testing.T, ctx context.Context, pool *pgxpool.Pool, statement string) {
	t.Helper()
	if _, err := pool.Exec(ctx, statement); err != nil {
		t.Fatal(err)
	}
}

func deferServerWriteFailureCleanup(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, statement := range []string{
			`DROP TRIGGER IF EXISTS fail_pin ON node_engine_servers`,
			`DROP TRIGGER IF EXISTS fail_node ON nodes`,
			`DROP TRIGGER IF EXISTS fail_list_insert ON agent_commands`,
			`DROP TRIGGER IF EXISTS fail_list_commit ON agent_commands`,
			`DROP FUNCTION IF EXISTS fail_server_write()`,
		} {
			if _, err := pool.Exec(ctx, statement); err != nil {
				t.Errorf("cleanup %q: %v", statement, err)
			}
		}
	})
}
