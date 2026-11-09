//go:build integration

package db_test

import (
	"testing"

	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
)

func TestEngineMigrationBackfillLegacyWritesAndRollback(t *testing.T) {
	pool, ctx := testPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	// Roll back the monthly report runs (00021), ISP entities (00014) then engine schedules (00010) to rehearse the pre-engine schema.
	for range 3 {
		if err := db.Migrate(ctx, pool, "down"); err != nil {
			t.Fatal(err)
		}
	}
	exec(t, ctx, pool, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES ('00000000-0000-4000-8000-000000000071','legacy',300,100);
 INSERT INTO schedules(node_id,interval_minutes,offset_minutes,enabled,revision) SELECT id,60,7,false,4 FROM nodes;
 INSERT INTO results(id,node_id,ts,status,threads,error) SELECT '00000000-0000-4000-8000-000000000072',id,now(),'failed',4,'legacy fixture' FROM nodes;`)
	dbtest.MigrateUp(t, pool)
	var matches bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM engine_schedules e JOIN schedules s USING(node_id)
 WHERE e.engine='ookla' AND (e.interval_minutes,e.offset_minutes,e.enabled,e.revision)=(s.interval_minutes,s.offset_minutes,s.enabled,s.revision))
 AND (SELECT engine='ookla' FROM results LIMIT 1)`).Scan(&matches); err != nil || !matches {
		t.Fatal("backfill differs", err)
	}
	// A v1.0.0 result insert has no engine; legacy schedule queries still address exactly one row.
	exec(t, ctx, pool, `INSERT INTO results(id,node_id,ts,status,threads,error) SELECT '00000000-0000-4000-8000-000000000073',id,now(),'failed',4,'old binary insert' FROM nodes;
 INSERT INTO engine_schedules(node_id,engine) SELECT id,'librespeed' FROM nodes;`)
	for range 3 {
		if err := db.Migrate(ctx, pool, "down"); err != nil {
			t.Fatal(err)
		}
	}
	var results, interval, offset int
	var enabled bool
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM results),interval_minutes,offset_minutes,enabled FROM schedules`).Scan(&results, &interval, &offset, &enabled); err != nil || results != 2 || interval != 60 || offset != 7 || enabled {
		t.Fatal("rollback lost legacy data", results, interval, offset, enabled, err)
	}
	dbtest.MigrateUp(t, pool)
	t.Log("PASS: existing schedule/results backfilled; old result inserts succeed; down preserves both result rows and unchanged legacy schedule; up reapplies")
}
