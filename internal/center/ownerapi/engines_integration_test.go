//go:build integration

package ownerapi

import (
	"errors"
	"testing"

	"github.com/jooservices/speedtest/internal/center/agentapi"
)

func TestEngineSchedulesCapabilitiesDualWriteAndConfig(t *testing.T) {
	pool, ctx := openPool(t)
	id := seedNode(t, ctx, pool, 71, "multi", nil)
	s := &Store{DB: pool}
	_, tag, err := s.Schedule(ctx, id)
	if err != nil {
		t.Fatal(err)
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
	if err := pool.QueryRow(ctx, `SELECT interval_minutes FROM engine_schedules WHERE node_id=$1 AND engine='librespeed'`, id).Scan(&interval); err != nil || interval != 30 {
		t.Fatal("default interval", interval, err)
	}
	if _, _, err := s.ReplaceSchedule(ctx, id, input, tag); !errors.Is(err, ErrStale) {
		t.Fatal("stale overwrite", err)
	}
	t.Log("PASS: unknown capabilities rejected atomically; heartbeat enables LibreSpeed; independent schedules/default 30m; transactional Ookla mirror; stale write rejected; agent config revision changed")
}
