//go:build integration

package isp_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/isp"
	"github.com/jooservices/speedtest/internal/shared/config"
)

func openISPStore(t *testing.T) (*pgxpool.Pool, context.Context, *isp.Store) {
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
		t.Fatal("invalid worktree test configuration")
	}
	if err := db.CheckTestDatabase(cfg.Database.Name); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal("cannot connect to configured worktree database")
	}
	t.Cleanup(pool.Close)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	return pool, ctx, &isp.Store{DB: pool}
}

func insertISPResult(t *testing.T, pool *pgxpool.Pool, ctx context.Context, id, node, ip, status, engine, ispID string, down, up, ping float64) {
	t.Helper()
	_, err := pool.Exec(ctx, `INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,ip,threads,server,engine,error,isp_id)
		VALUES($1,$2,now(),$3,
		 CASE WHEN $3='failed' THEN NULL::double precision ELSE $4::double precision END,
		 CASE WHEN $3='failed' THEN NULL::double precision ELSE $5::double precision END,
		 CASE WHEN $3='failed' THEN NULL::double precision ELSE $6::double precision END,
		 1,$7,4,'{"id":1,"name":"fixture","city":"Hanoi","host":"fixture"}'::jsonb,$8,
		 CASE WHEN $3='failed' THEN 'fixture failure' ELSE NULL END,$9)`, id, node, status, down, up, ping, ip, engine, ispID)
	if err != nil {
		t.Fatal("seed ISP result:", err)
	}
}

func assertISPMetric(t *testing.T, row map[string]any, key string, want float64) {
	t.Helper()
	got, ok := row[key].(*float64)
	if !ok || got == nil || *got != want {
		t.Fatalf("%s=%v, want %.4f", key, row[key], want)
	}
}

func TestISPMigrationAndQueriesIntegration(t *testing.T) {
	pool, ctx, store := openISPStore(t)
	node := "00000000-0000-4000-8000-0000000000a1"
	node2 := "00000000-0000-4000-8000-0000000000a2"
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES ($1,'isp-node',300,100)`, node); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES ($1,'isp-node-two',200,100)`, node2); err != nil {
		t.Fatal(err)
	}
	highASN := uint64(4294967295)
	e, err := isp.Ensure(ctx, pool, uint(highASN), "Viettel Group")
	if err != nil || e == nil || e.Name != "Viettel" {
		t.Fatalf("ensure: %#v %v", e, err)
	}
	if _, err = isp.LinkAddress(ctx, pool, "203.0.113.80", uint(highASN), "Viettel Group"); err != nil {
		t.Fatal(err)
	}
	e2, err := isp.Ensure(ctx, pool, 1, "Local Fiber")
	if err != nil || e2 == nil {
		t.Fatal("lower-bound ASN:", err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO ip_addresses(ip,isp_id) VALUES('203.0.113.80',$1),('203.0.113.81',$2)`, e.ID, e2.ID); err != nil {
		t.Fatal(err)
	}
	insertISPResult(t, pool, ctx, "00000000-0000-4000-8000-0000000000b1", node, "203.0.113.80", "ok", "ookla", e.ID, 250, 80, 12)
	insertISPResult(t, pool, ctx, "00000000-0000-4000-8000-0000000000b2", node, "203.0.113.80", "degraded", "ookla", e.ID, 200, 60, 20)
	insertISPResult(t, pool, ctx, "00000000-0000-4000-8000-0000000000b3", node, "203.0.113.80", "failed", "ookla", e.ID, 999, 99, 999)
	insertISPResult(t, pool, ctx, "00000000-0000-4000-8000-0000000000b4", node, "203.0.113.80", "ok", "librespeed", e.ID, 900, 500, 2)
	insertISPResult(t, pool, ctx, "00000000-0000-4000-8000-0000000000b5", node2, "203.0.113.81", "ok", "ookla", e2.ID, 300, 150, 8)
	q := isp.Query{From: time.Now().UTC().Add(-24 * time.Hour), To: time.Now().UTC().Add(time.Hour)}
	list, err := store.List(ctx, q)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %v %#v", err, list)
	}
	assertISPMetric(t, list[0], "avgDown", 225)
	assertISPMetric(t, list[0], "p95Down", 247.5)
	assertISPMetric(t, list[0], "pctExpected", 75)
	assertISPMetric(t, list[0], "successRate", 2.0/3)
	assertISPMetric(t, list[0], "avgPing", 16)
	if list[0]["nodeCount"] != 1 {
		t.Fatalf("ISP node count=%v, want 1", list[0]["nodeCount"])
	}
	got, err := store.Get(ctx, e.ID, q)
	if err != nil || got["name"] != "Viettel" {
		t.Fatalf("get: %#v %v", got, err)
	}
	trend, err := store.Trend(ctx, e.ID, q)
	if err != nil || len(trend) != 1 || trend[0]["n"] != 3 {
		t.Fatalf("trend rows: %#v %v", trend, err)
	}
	assertISPMetric(t, trend[0], "down", 225)
	assertISPMetric(t, trend[0], "up", 70)
	assertISPMetric(t, trend[0], "ping", 16)
	cmp, err := store.Compare(ctx, isp.Query{From: q.From, To: q.To, ISPIDs: []string{e.ID, e2.ID}})
	if err != nil || len(cmp) != 2 {
		t.Fatalf("compare: %v %#v", err, cmp)
	}
	cmpByID := map[string]map[string]any{cmp[0]["id"].(string): cmp[0], cmp[1]["id"].(string): cmp[1]}
	assertISPMetric(t, cmpByID[e2.ID], "avgDown", 300)
	assertISPMetric(t, cmpByID[e2.ID], "pctExpected", 150)
	libre, err := store.Get(ctx, e.ID, isp.Query{Engine: "librespeed", From: q.From, To: q.To})
	if err != nil {
		t.Fatal("isolated engine query:", err)
	}
	assertISPMetric(t, libre, "avgDown", 900)
	plan, err := store.CreatePlan(ctx, e.ID, "Fast 50", 300, 100)
	if err != nil {
		t.Fatal(err)
	}
	planID, _ := plan["id"].(string)
	if _, err := pool.Exec(ctx, `UPDATE nodes SET isp_plan_id=$2::uuid,expected_down_isp_default=true,expected_up_isp_default=true WHERE id=$1`, node, planID); err != nil {
		t.Fatal("assign node ISP plan:", err)
	}
	if _, err = store.UpdatePlan(ctx, e.ID, planID, "Fast 50", 350, 100); err != nil {
		t.Fatal(err)
	}
	var down, up float64
	if err := pool.QueryRow(ctx, `SELECT expected_down_mbps,expected_up_mbps FROM nodes WHERE id=$1`, node).Scan(&down, &up); err != nil || down != 350 || up != 100 {
		t.Fatalf("plan default speeds=%v/%v: %v", down, up, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE nodes SET expected_down_mbps=500,expected_down_isp_default=false WHERE id=$1`, node); err != nil {
		t.Fatal("set explicit speed override:", err)
	}
	if _, err = store.UpdatePlan(ctx, e.ID, planID, "Fast 50", 400, 200); err != nil {
		t.Fatal("change advertised speeds:", err)
	}
	if err := pool.QueryRow(ctx, `SELECT expected_down_mbps,expected_up_mbps FROM nodes WHERE id=$1`, node).Scan(&down, &up); err != nil || down != 500 || up != 200 {
		t.Fatalf("plan defaults/override=%v/%v: %v", down, up, err)
	}
	plans, err := store.ListPlans(ctx, e.ID)
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans: %v %#v", err, plans)
	}
	if err = store.DeletePlan(ctx, e.ID, planID); err != nil {
		t.Fatal(err)
	}
	var planCleared bool
	if err := pool.QueryRow(ctx, `SELECT isp_plan_id IS NULL FROM nodes WHERE id=$1`, node).Scan(&planCleared); err != nil || !planCleared {
		t.Fatalf("plan deletion did not clear node assignment: %v", err)
	}
	if err = db.Migrate(ctx, pool, "down"); err != nil {
		t.Fatal("down", err)
	}
	if err = db.Migrate(ctx, pool, "up"); err != nil {
		t.Fatal("up again", err)
	}
}

func TestPlanUpdatesPreserveExplicitEqualAndDifferentSpeedsIntegration(t *testing.T) {
	pool, ctx, store := openISPStore(t)
	entity, err := isp.Ensure(ctx, pool, 64501, "Plan Provenance ISP")
	if err != nil || entity == nil {
		t.Fatal("create ISP:", err)
	}
	plan, err := store.CreatePlan(ctx, entity.ID, "Fiber", 300, 100)
	if err != nil {
		t.Fatal("create plan:", err)
	}
	planID := plan["id"].(string)
	seedPlanProvenanceNodes(t, ctx, pool, planID)
	if _, err := store.UpdatePlan(ctx, entity.ID, planID, "Fiber", 350, 120); err != nil {
		t.Fatal("update plan:", err)
	}
	assertPlanProvenance(t, ctx, pool)
}

func seedPlanProvenanceNodes(t *testing.T, ctx context.Context, pool *pgxpool.Pool, planID string) {
	t.Helper()
	_, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,isp_plan_id,expected_down_mbps,expected_up_mbps,expected_down_isp_default,expected_up_isp_default) VALUES
	('00000000-0000-4000-8000-0000000000d1','default speeds',$1,300,100,true,true),
	('00000000-0000-4000-8000-0000000000d2','explicit equal speeds',$1,300,100,false,false),
	('00000000-0000-4000-8000-0000000000d3','explicit different speeds',$1,500,150,false,false)`, planID)
	if err != nil {
		t.Fatal("seed plan provenance nodes:", err)
	}
}

func assertPlanProvenance(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT name,expected_down_mbps,expected_up_mbps FROM nodes WHERE id IN (
	'00000000-0000-4000-8000-0000000000d1','00000000-0000-4000-8000-0000000000d2','00000000-0000-4000-8000-0000000000d3') ORDER BY id`)
	if err != nil {
		t.Fatal("read expected plan values:", err)
	}
	defer rows.Close()
	want := [][3]any{{"default speeds", 350.0, 120.0}, {"explicit equal speeds", 300.0, 100.0}, {"explicit different speeds", 500.0, 150.0}}
	for i := range want {
		var name string
		var down, up float64
		if !rows.Next() || rows.Scan(&name, &down, &up) != nil || name != want[i][0] || down != want[i][1] || up != want[i][2] {
			t.Fatalf("plan value[%d]=%q %.0f/%.0f want %v", i, name, down, up, want[i])
		}
	}
	if rows.Next() || rows.Err() != nil {
		t.Fatalf("unexpected plan provenance rows: %v", rows.Err())
	}
}

func TestSixHourTrendUsesInRangeHourlyPointsAcrossUTCDateIntegration(t *testing.T) {
	pool, ctx, store := openISPStore(t)
	const node = "00000000-0000-4000-8000-0000000000f1"
	const ispID = "00000000-0000-4000-8000-0000000000f2"
	from := time.Date(2026, 11, 10, 21, 0, 0, 0, time.UTC)
	to := from.Add(6 * time.Hour)
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES ($1,'trend-node',100,50)`, node); err != nil {
		t.Fatal("seed trend node:", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO isps(id,asn,organisation,name) VALUES ($1,64520,'Trend Network','Trend Network')`, ispID); err != nil {
		t.Fatal("seed trend ISP:", err)
	}
	seedSixHourTrendSamples(t, ctx, pool, node, ispID, from)
	points, err := store.Trend(ctx, ispID, isp.Query{From: from, To: to})
	if err != nil || len(points) != 4 {
		t.Fatalf("hourly trend points=%v error=%v", points, err)
	}
	assertTrendPointsInsideWindow(t, points, from, to)
}

func seedSixHourTrendSamples(t *testing.T, ctx context.Context, pool *pgxpool.Pool, node, ispID string, from time.Time) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO ip_addresses(ip,isp_id) VALUES ('203.0.113.220',$1)`, ispID); err != nil {
		t.Fatal("seed trend IP:", err)
	}
	for i, offset := range []time.Duration{10 * time.Minute, 40 * time.Minute, time.Hour + 20*time.Minute, 3*time.Hour + 10*time.Minute, 5*time.Hour + 45*time.Minute} {
		_, err := pool.Exec(ctx, `INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,server,ip,threads,isp_id)
			VALUES ($1,$2,$3,'ok',100,50,10,1,'{"id":1,"name":"fixture"}'::jsonb,'203.0.113.220',4,$4)`, fmt.Sprintf("00000000-0000-4000-8000-%012x", 0xf3+i), node, from.Add(offset), ispID)
		if err != nil {
			t.Fatal("seed trend result:", err)
		}
	}
}

func assertTrendPointsInsideWindow(t *testing.T, points []map[string]any, from, to time.Time) {
	t.Helper()
	for _, point := range points {
		raw, ok := point["ts"].(string)
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if !ok || err != nil || parsed.Before(from) || !parsed.Before(to) {
			t.Fatalf("trend timestamp %v is outside [%s,%s): %v", raw, from, to, err)
		}
	}
}

func TestConcurrentISPAddressLinksIntegration(t *testing.T) {
	pool, ctx, _ := openISPStore(t)
	const workers = 32
	if _, err := pool.Exec(ctx, `INSERT INTO ip_addresses(ip)
SELECT ('203.0.113.' || value)::inet FROM generate_series(100,131) value`); err != nil {
		t.Fatal("seed concurrent ISP addresses:", err)
	}
	ids, errs := make(chan string, workers), make(chan error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		ip := fmt.Sprintf("203.0.113.%d", i+100)
		wg.Go(func() {
			id, err := linkAddressTransaction(ctx, pool, ip)
			if err != nil {
				errs <- err
				return
			}
			ids <- id
		})
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatal("concurrent ISP upsert/link:", err)
	}
	assertSingleISPLink(t, ctx, pool, ids)
}

func linkAddressTransaction(ctx context.Context, pool *pgxpool.Pool, ip string) (string, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	e, err := isp.LinkAddress(ctx, tx, ip, 4294967295, "Boundary Network Ltd.")
	if err != nil {
		return "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", err
	}
	return e.ID, nil
}

func assertSingleISPLink(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids <-chan string) {
	t.Helper()
	wantID := ""
	for id := range ids {
		if wantID == "" {
			wantID = id
		} else if id != wantID {
			t.Fatalf("concurrent upsert returned ISP IDs %q and %q", wantID, id)
		}
	}
	var isps, linked int
	err := pool.QueryRow(ctx, `SELECT count(DISTINCT isp_id), count(*) FILTER (WHERE isp_id IS NOT NULL)
FROM ip_addresses WHERE ip BETWEEN '203.0.113.100'::inet AND '203.0.113.131'::inet`).Scan(&isps, &linked)
	if err != nil || isps != 1 || linked != 32 || wantID == "" {
		t.Fatalf("concurrent ISP rows=%d linked addresses=%d returned ID=%q: %v", isps, linked, wantID, err)
	}
}

func TestISPMigrationBackfillsASNAndSurvivesRollbackIntegration(t *testing.T) {
	pool, ctx, _ := openISPStore(t)
	node, result := seedLegacyISPData(t, ctx, pool)
	rollbackThroughISPEntities(t, ctx, pool)
	if err := db.Migrate(ctx, pool, "up"); err != nil {
		t.Fatal("up", err)
	}
	assertISPBackfill(t, ctx, pool, node, result)
	rollbackThroughISPEntities(t, ctx, pool)
	assertISPRollbackPreservesData(t, ctx, pool, result)
	if err := db.Migrate(ctx, pool, "up"); err != nil {
		t.Fatal("reapply", err)
	}
	assertISPBackfill(t, ctx, pool, node, result)
}

func rollbackThroughISPEntities(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, name := range []string{"audit authentication source", "API access tokens", "ISP plan provenance", "ISP entities"} {
		if err := db.Migrate(ctx, pool, "down"); err != nil {
			t.Fatalf("down %s: %v", name, err)
		}
	}
}

func seedLegacyISPData(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (string, string) {
	t.Helper()
	node, result := "00000000-0000-4000-8000-0000000000c1", "00000000-0000-4000-8000-0000000000c2"
	_, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES ($1,'asn-backfill',100,25)`, node)
	if err != nil {
		t.Fatal("legacy node", err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO ip_addresses(ip,geo) VALUES
 ('203.0.113.81','{"asn":64500,"organisation":"Origin ISP"}'),
 ('203.0.113.82','{"asn":4294967295,"organisation":"High ASN Network Ltd."}'),
 ('203.0.113.83','{"asn":1}'),
 ('203.0.113.84','{"asn":0,"organisation":"Invalid zero"}'),
 ('203.0.113.85','{"asn":4294967296,"organisation":"Invalid high"}')`)
	if err != nil {
		t.Fatal("legacy ASN addresses", err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,ip,threads,server,engine) VALUES
	 ($1,$2,'2026-11-03T00:00:00Z','ok',50,10,4,1,'203.0.113.82',2,'{"id":1,"name":"fixture"}'::jsonb,'ookla'),
	 ('00000000-0000-4000-8000-0000000000c3',$2,'2026-11-03T00:01:00Z','ok',10,5,4,1,'203.0.113.83',2,'{"id":1,"name":"fixture"}'::jsonb,'ookla')`, result, node)
	if err != nil {
		t.Fatal("legacy result", err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO node_ip_history(id,node_id,ts,from_ip,to_ip)
	 VALUES ($1,$2,'2026-11-03T00:00:00Z','203.0.113.81','203.0.113.82')`, result, node)
	if err != nil {
		t.Fatal("legacy history", err)
	}
	return node, result
}

func assertISPBackfill(t *testing.T, ctx context.Context, pool *pgxpool.Pool, node, result string) {
	t.Helper()
	var resultASN, addressASN, fromASN, toASN uint64
	err := pool.QueryRow(ctx, `SELECT result_isp.asn,address_isp.asn,from_isp.asn,to_isp.asn
FROM results r JOIN ip_addresses a ON a.ip=r.ip JOIN node_ip_history h ON h.id=r.id
JOIN isps result_isp ON result_isp.id=r.isp_id JOIN isps address_isp ON address_isp.id=a.isp_id
JOIN isps from_isp ON from_isp.id=h.from_isp_id JOIN isps to_isp ON to_isp.id=h.to_isp_id
WHERE r.id=$1 AND r.node_id=$2`, result, node).Scan(&resultASN, &addressASN, &fromASN, &toASN)
	if err != nil || resultASN != 4294967295 || addressASN != 4294967295 || fromASN != 64500 || toASN != 4294967295 {
		t.Fatalf("ASN backfill result=%d address=%d from=%d to=%d: %v", resultASN, addressASN, fromASN, toASN, err)
	}
	var lowASN uint64
	var organisation, name string
	if err := pool.QueryRow(ctx, `SELECT i.asn,i.organisation,i.name FROM ip_addresses a JOIN isps i ON i.id=a.isp_id WHERE a.ip='203.0.113.83'`).Scan(&lowASN, &organisation, &name); err != nil || lowASN != 1 || organisation != "AS1" || name != "AS1" {
		t.Fatalf("lower-bound ASN fallback=%d %q %q: %v", lowASN, organisation, name, err)
	}
	var invalid int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM isps WHERE asn=0 OR asn=4294967296`).Scan(&invalid); err != nil || invalid != 0 {
		t.Fatalf("invalid ASN rows created=%d: %v", invalid, err)
	}
}

func assertISPRollbackPreservesData(t *testing.T, ctx context.Context, pool *pgxpool.Pool, result string) {
	t.Helper()
	var gone bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.isps') IS NULL`).Scan(&gone); err != nil || !gone {
		t.Fatal("ISP table remains after migration down", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM results`).Scan(&count); err != nil || count != 2 {
		t.Fatal("down removed pre-existing results", err)
	}
}
