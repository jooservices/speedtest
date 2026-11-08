//go:build integration

package isp_test

import (
	"context"
	"path/filepath"
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

func TestISPMigrationAndQueriesIntegration(t *testing.T) {
	pool, ctx, store := openISPStore(t)
	node := "00000000-0000-4000-8000-0000000000a1"
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES ($1,'isp-node',300,100)`, node); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ip_addresses(ip) VALUES ('203.0.113.80')`); err != nil {
		t.Fatal(err)
	}
	e, err := isp.Ensure(ctx, pool, 7552, "Viettel Group")
	if err != nil || e == nil || e.Name != "Viettel" {
		t.Fatalf("ensure: %#v %v", e, err)
	}
	if _, err = isp.LinkAddress(ctx, pool, "203.0.113.80", 7552, "Viettel Group"); err != nil {
		t.Fatal(err)
	}
	rid := "00000000-0000-4000-8000-0000000000b1"
	if _, err = pool.Exec(ctx, `INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,ip,threads,server,engine,isp_id)
		VALUES ($1,$2,now(),'ok',250,80,12,1,'203.0.113.80',4,'{"id":1,"name":"x","city":"y","host":"h"}'::jsonb,'ookla',$3)`, rid, node, e.ID); err != nil {
		t.Fatal(err)
	}
	q := isp.Query{From: time.Now().UTC().Add(-24 * time.Hour), To: time.Now().UTC().Add(time.Hour)}
	list, err := store.List(ctx, q)
	if err != nil || len(list) == 0 {
		t.Fatalf("list: %v %#v", err, list)
	}
	got, err := store.Get(ctx, e.ID, q)
	if err != nil || got["name"] != "Viettel" {
		t.Fatalf("get: %#v %v", got, err)
	}
	if _, err = store.Trend(ctx, e.ID, q); err != nil {
		t.Fatal(err)
	}
	cmp, err := store.Compare(ctx, isp.Query{From: q.From, To: q.To, ISPIDs: []string{e.ID}})
	if err != nil || len(cmp) != 1 {
		t.Fatalf("compare: %v %#v", err, cmp)
	}
	plan, err := store.CreatePlan(ctx, e.ID, "Fast 50", 300, 100)
	if err != nil {
		t.Fatal(err)
	}
	planID, _ := plan["id"].(string)
	if _, err = store.UpdatePlan(ctx, e.ID, planID, "Fast 50", 350, 100); err != nil {
		t.Fatal(err)
	}
	plans, err := store.ListPlans(ctx, e.ID)
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans: %v %#v", err, plans)
	}
	if err = store.DeletePlan(ctx, e.ID, planID); err != nil {
		t.Fatal(err)
	}
	if err = db.Migrate(ctx, pool, "down"); err != nil {
		t.Fatal("down", err)
	}
	if err = db.Migrate(ctx, pool, "up"); err != nil {
		t.Fatal("up again", err)
	}
}
