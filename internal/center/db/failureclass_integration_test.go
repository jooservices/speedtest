//go:build integration

package db_test

import (
	"testing"

	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
)

func TestFailureClassMigrationDefaultAndRollback(t *testing.T) {
	pool, ctx := testPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	if err := db.Migrate(ctx, pool, "down"); err != nil {
		t.Fatal(err)
	}
	exec(t, ctx, pool, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES ('00000000-0000-4000-8000-000000000081','fc',300,100);
 INSERT INTO engine_schedules(node_id,engine) SELECT id,'ookla' FROM nodes;
 INSERT INTO results(id,node_id,ts,status,threads,error,engine) SELECT '00000000-0000-4000-8000-000000000082',id,now(),'failed',4,'Too many requests','ookla' FROM nodes;`)
	dbtest.MigrateUp(t, pool)
	var class string
	if err := pool.QueryRow(ctx, `SELECT failure_class FROM results WHERE id='00000000-0000-4000-8000-000000000082'`).Scan(&class); err != nil || class != "other" {
		t.Fatalf("existing rows must default to other, got %q err=%v", class, err)
	}
	exec(t, ctx, pool, `INSERT INTO results(id,node_id,ts,status,threads,error,engine,failure_class)
 SELECT '00000000-0000-4000-8000-000000000083',id,now(),'failed',4,'Too many requests','ookla','rate_limited' FROM nodes;`)
	var stored string
	if err := pool.QueryRow(ctx, `SELECT failure_class FROM results WHERE id='00000000-0000-4000-8000-000000000083'`).Scan(&stored); err != nil || stored != "rate_limited" {
		t.Fatalf("new class not stored: %q %v", stored, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO results(id,node_id,ts,status,threads,error,engine,failure_class)
 SELECT '00000000-0000-4000-8000-000000000084',id,now(),'failed',4,'x','ookla','not_a_class' FROM nodes`); err == nil {
		t.Fatal("invalid failure_class accepted")
	}
	if err := db.Migrate(ctx, pool, "down"); err != nil {
		t.Fatal(err)
	}
	var hasColumn bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='results' AND column_name='failure_class')`).Scan(&hasColumn); err != nil || hasColumn {
		t.Fatal("column survived rollback", hasColumn, err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM results`).Scan(&count); err != nil || count != 2 {
		t.Fatal("rollback lost results", count, err)
	}
	dbtest.MigrateUp(t, pool)
	t.Log("PASS: failure_class defaults existing rows to other, accepts vocabulary, rejects unknowns, rolls back safely")
}
