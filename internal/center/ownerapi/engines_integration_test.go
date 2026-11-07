//go:build integration

package ownerapi

import (
	"errors"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/agentapi"
)

func TestEngineSchedulesCapabilitiesDualWriteAndConfig(t *testing.T) {
	pool, ctx := openPool(t)
	id := seedNode(t, ctx, pool, 7, "multi", nil)
	s := &Store{DB: pool}
	_, tag, err := s.Schedule(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReplaceSchedule(ctx, id, ScheduleInput{Engines: []EngineSchedule{{Engine: "librespeed", Enabled: true}}}, tag); !errors.Is(err, ErrInvalid) {
		t.Fatal("ookla-less schedules accepted", err)
	}
	input := ScheduleInput{Engines: []EngineSchedule{{Engine: "ookla", Interval: 60, Offset: 7, Enabled: true}, {Engine: "librespeed", Enabled: true}}}
	if _, _, err := s.ReplaceSchedule(ctx, id, input, tag); !errors.Is(err, ErrInvalid) {
		t.Fatal("unreported engine accepted", err)
	}
	agent := &agentapi.Postgres{Pool: pool}
	if _, err := agent.Heartbeat(ctx, id, agentapi.Heartbeat{Agent: "multi", Engines: []string{"ookla", "librespeed"}}); err != nil {
		t.Fatal(err)
	}
	before, err := agent.Config(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	view, next, err := s.ReplaceSchedule(ctx, id, input, tag)
	if err != nil {
		t.Fatal(err)
	}
	if next == tag || len(view["engines"].([]map[string]any)) != 2 {
		t.Fatal("ETag/list", view, next)
	}
	var same bool
	if err := pool.QueryRow(ctx, `SELECT (e.interval_minutes,e.offset_minutes,e.enabled)=(s.interval_minutes,s.offset_minutes,s.enabled) FROM engine_schedules e JOIN schedules s USING(node_id) WHERE e.node_id=$1 AND e.engine='ookla'`, id).Scan(&same); err != nil || !same {
		t.Fatal("dual write", err)
	}
	cfg, err := agent.Config(ctx, id)
	if err != nil || cfg["revision"] == before["revision"] {
		t.Fatal("config revision did not change", cfg, err)
	}
	var interval int
	if err := pool.QueryRow(ctx, `SELECT interval_minutes FROM engine_schedules WHERE node_id=$1 AND engine='librespeed'`, id).Scan(&interval); err != nil || interval != 60 {
		t.Fatal("default interval", interval, err)
	}
	if _, _, err := s.ReplaceSchedule(ctx, id, input, tag); !errors.Is(err, ErrStale) {
		t.Fatal("stale overwrite", err)
	}
	ooklaOnly := ScheduleInput{Engines: []EngineSchedule{{Engine: "ookla", Interval: 45, Offset: 3, Enabled: true}}}
	view, next, err = s.ReplaceSchedule(ctx, id, ooklaOnly, next)
	if err != nil || len(view["engines"].([]map[string]any)) != 1 {
		t.Fatal("drop second engine", view, err)
	}
	var leftover int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM engine_schedules WHERE node_id=$1 AND engine='librespeed'`, id).Scan(&leftover); err != nil || leftover != 0 {
		t.Fatal("librespeed schedule retained", leftover, err)
	}
	if _, _, err := s.ReplaceSchedule(ctx, "00000000-0000-4000-8000-000000000099", ooklaOnly, next); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing node", err)
	}
	t.Log("PASS: unknown capabilities rejected atomically; heartbeat enables LibreSpeed; independent schedules/default 60m; transactional Ookla mirror; stale write rejected; drop removes engine row; missing node; agent config revision changed")
}

func TestResultListAndExportKeepEngineIdentity(t *testing.T) {
	pool, ctx := openPool(t)
	id := seedNode(t, ctx, pool, 7, "engines", nil)
	seedResult(t, ctx, pool, 1, id, time.Now(), "ok", "203.0.113.5")
	seedResult(t, ctx, pool, 2, id, time.Now(), "ok", "203.0.113.5")
	if _, err := pool.Exec(ctx, `UPDATE results SET engine='librespeed' WHERE id=$1`, uid(1002)); err != nil {
		t.Fatal(err)
	}
	store := &Store{DB: pool}
	for _, name := range []string{"", "ookla", "librespeed"} {
		want := name
		if want == "" {
			want = "ookla"
		}
		filter := ResultFilter{Engine: name}
		rows, total, err := store.ListResults(ctx, filter, 1, 100)
		if err != nil || total != 1 || len(rows) != 1 || rows[0]["engine"] != want {
			t.Fatal(name, rows, total, err)
		}
		count := 0
		if err = store.ExportResults(ctx, filter, func(row ResultRow) error {
			count++
			if row.Result["engine"] != want {
				t.Fatal(row)
			}
			return nil
		}); err != nil || count != 1 {
			t.Fatal(name, count, err)
		}
	}
	count := 0
	if err := store.ExportResults(ctx, ResultFilter{AllEngines: true}, func(ResultRow) error { count++; return nil }); err != nil || count != 2 {
		t.Fatal(count, err)
	}
}
