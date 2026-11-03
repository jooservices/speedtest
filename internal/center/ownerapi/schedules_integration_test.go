//go:build integration

package ownerapi

import (
	"errors"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/schedules"
)

func scheduleStore(t *testing.T) (*Store, func(string) map[string]any) {
	t.Helper()
	pool, ctx := openPool(t)
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	s := &Store{DB: pool, Now: func() time.Time { return now }}
	fresh := now
	a := seedNode(t, ctx, pool, 1, "alpha", &fresh)
	b := seedNode(t, ctx, pool, 2, "bravo", &fresh)
	seedNode(t, ctx, pool, 3, "charlie", &fresh)
	// alpha has history: each run moved 1000 bytes down and 500 up; one failed run must not count.
	seedResult(t, ctx, pool, 1, a, now.Add(-time.Hour), "ok", "203.0.113.5")
	seedResult(t, ctx, pool, 2, a, now.Add(-2*time.Hour), "ok", "203.0.113.5")
	seedResult(t, ctx, pool, 3, a, now.Add(-3*time.Hour), "failed", "203.0.113.5")
	if _, err := pool.Exec(ctx, `UPDATE nodes SET enabled=false WHERE id=$1`, b); err != nil {
		t.Fatal(err)
	}
	return s, func(id string) map[string]any {
		view, _, err := s.Schedule(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return view
	}
}

func nodeViews(t *testing.T, data map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, n := range data["nodes"].([]map[string]any) {
		out[n["nodeId"].(string)] = n
	}
	return out
}

func TestSchedulesViewNextRunUsageAndClashes(t *testing.T) {
	s, _ := scheduleStore(t)
	ctx := t.Context()
	data, etag, err := s.Schedules(ctx)
	if err != nil || etag == "" {
		t.Fatalf("schedules: %v %q", err, etag)
	}
	rules := data["rules"].(map[string]any)
	if rules["stagger"] != true || rules["maxConcurrent"] != 1 || rules["quiet"].(map[string]any)["timezone"] != "UTC" {
		t.Fatalf("default rules = %v", rules)
	}
	nodes := nodeViews(t, data)
	alpha := nodes[uid(1)]
	// interval 30 offset 3 after 12:00 -> 12:03.
	if alpha["nextRun"] != "2026-11-03T12:03:00Z" || len(alpha["upcoming"].([]string)) != 10 {
		t.Fatalf("alpha next run = %v upcoming %d", alpha["nextRun"], len(alpha["upcoming"].([]string)))
	}
	if alpha["bytesPerDay"] != 1500.0*48 || alpha["bytesPerMonth"] != 1500.0*48*30 {
		t.Fatalf("alpha usage = %v / %v (failed runs must not count)", alpha["bytesPerDay"], alpha["bytesPerMonth"])
	}
	if charlie := nodes[uid(3)]; charlie["bytesPerDay"] != 0.0 || charlie["nextRun"] == nil {
		t.Fatalf("a node without history has no usage estimate: %v", charlie)
	}
	if bravo := nodes[uid(2)]; bravo["nextRun"] != nil || len(bravo["upcoming"].([]string)) != 0 || bravo["bytesPerDay"] != 0.0 {
		t.Fatalf("a paused node has no next run: %v", bravo)
	}
	// alpha (3) and charlie (9) start 6 minutes apart (no clash); bravo is paused, so no clash at all.
	if clashes := data["clashes"].([]map[string]any); len(clashes) != 0 {
		t.Fatalf("clashes = %v", clashes)
	}
	one, tag, err := s.Schedule(ctx, uid(1))
	if err != nil || tag != `"schedule-1"` || one["nodeId"] != uid(1) {
		t.Fatalf("one schedule: %v %q %v", one, tag, err)
	}
	if _, _, err := s.Schedule(ctx, uid(99)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown node = %v", err)
	}
	if _, _, err := s.Schedule(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("malformed id = %v", err)
	}
}

func TestReplaceScheduleClampsAndChecksPreconditions(t *testing.T) {
	s, _ := scheduleStore(t)
	ctx := t.Context()
	view, tag, err := s.ReplaceSchedule(ctx, uid(1), ScheduleInput{Interval: 10, Offset: 25, Enabled: true}, `"schedule-1"`)
	if err != nil || tag != `"schedule-2"` {
		t.Fatalf("replace: %v %q", err, tag)
	}
	if cfg := view["config"].(map[string]any); cfg["interval"] != 10 || cfg["offset"] != 9 || cfg["enabled"] != true {
		t.Fatalf("offset must clamp below the interval: %v", cfg)
	}
	if _, _, err := s.ReplaceSchedule(ctx, uid(1), ScheduleInput{Interval: 10, Offset: 0, Enabled: true}, `"schedule-1"`); !errors.Is(err, ErrStale) {
		t.Fatalf("stale tag = %v", err)
	}
	off, _, err := s.ReplaceSchedule(ctx, uid(1), ScheduleInput{Interval: 10, Offset: 0, Enabled: false}, tag)
	if err != nil || off["nextRun"] != nil || off["bytesPerDay"] != 0.0 {
		t.Fatalf("a disabled schedule has no next run or usage: %v %v", off, err)
	}
	for _, bad := range []ScheduleInput{{Interval: 4, Enabled: true}, {Interval: 1441, Enabled: true}, {Interval: 30, Offset: -1, Enabled: true}} {
		if _, _, err := s.ReplaceSchedule(ctx, uid(1), bad, `"schedule-3"`); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v = %v", bad, err)
		}
	}
	if _, _, err := s.ReplaceSchedule(ctx, uid(99), ScheduleInput{Interval: 30, Enabled: true}, `"schedule-1"`); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown node = %v", err)
	}
	if _, _, err := s.ReplaceSchedule(ctx, "x", ScheduleInput{Interval: 30, Enabled: true}, `"schedule-1"`); !errors.Is(err, ErrNotFound) {
		t.Fatalf("malformed id = %v", err)
	}
	// The collection tag moves when a single schedule changes.
	_, before, _ := s.Schedules(ctx)
	if _, _, err := s.ReplaceSchedule(ctx, uid(3), ScheduleInput{Interval: 30, Offset: 1, Enabled: true}, `"schedule-1"`); err != nil {
		t.Fatal(err)
	}
	if _, after, _ := s.Schedules(ctx); after == before {
		t.Fatal("the collection ETag must change with a schedule")
	}
}

func TestReplaceRulesStaggersAndAppliesQuietHours(t *testing.T) {
	s, _ := scheduleStore(t)
	ctx := t.Context()
	// Put alpha and charlie 2 minutes apart so a clash exists.
	if _, _, err := s.ReplaceSchedule(ctx, uid(3), ScheduleInput{Interval: 30, Offset: 4, Enabled: true}, `"schedule-1"`); err != nil {
		t.Fatal(err)
	}
	data, tag, err := s.Schedules(ctx)
	if err != nil || len(data["clashes"].([]map[string]any)) != 1 {
		t.Fatalf("expected one clash: %v %v", data["clashes"], err)
	}
	if _, _, err := s.ReplaceRules(ctx, ScheduleRules{Stagger: false, MaxConcurrent: 1}, `"old"`); !errors.Is(err, ErrStale) {
		t.Fatalf("stale rules = %v", err)
	}
	for _, bad := range []ScheduleRules{{MaxConcurrent: 0}, {MaxConcurrent: 6}, {MaxConcurrent: 1, Quiet: schedules.Quiet{Start: "22:00"}}, {MaxConcurrent: 1, Quiet: schedules.Quiet{Start: "22:00", End: "22:00"}}} {
		if _, _, err := s.ReplaceRules(ctx, bad, tag); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v = %v", bad, err)
		}
	}
	rules, newTag, err := s.ReplaceRules(ctx, ScheduleRules{Stagger: true, MaxConcurrent: 1,
		Quiet: schedules.Quiet{Start: "13:00", End: "14:00", Timezone: "UTC"}}, tag)
	if err != nil || newTag == tag || rules["maxConcurrent"] != 1 || rules["quiet"].(map[string]any)["quietStart"] != "13:00" {
		t.Fatalf("rules: %v %q %v", rules, newTag, err)
	}
	data, _, _ = s.Schedules(ctx)
	if len(data["clashes"].([]map[string]any)) != 0 {
		t.Fatalf("stagger must remove clashes: %v", data["clashes"])
	}
	nodes := nodeViews(t, data)
	// Three nodes at interval 30 -> offsets 0, 10, 20 by id order (bravo is paused but still spread).
	for id, want := range map[string]int{uid(1): 0, uid(2): 10, uid(3): 20} {
		if got := nodes[id]["config"].(map[string]any)["offset"]; got != want {
			t.Errorf("%s offset = %v, want %d", id, got, want)
		}
	}
	for _, run := range nodes[uid(1)]["upcoming"].([]string) {
		if run >= "2026-11-03T13:00:00Z" && run < "2026-11-03T14:00:00Z" {
			t.Fatalf("upcoming run %s falls in the quiet window", run)
		}
	}
	// 48 runs a day, 2 of them inside the quiet hour.
	if got := nodes[uid(1)]["bytesPerDay"]; got != 1500.0*46 {
		t.Fatalf("quiet hours reduce the daily estimate: %v", got)
	}
	// Agents learn about it through the schedule revision they are served.
	var rev int64
	if err := s.DB.QueryRow(ctx, `SELECT revision FROM schedules WHERE node_id=$1`, uid(1)).Scan(&rev); err != nil || rev < 3 {
		t.Fatalf("schedule revision = %d %v", rev, err)
	}
	var tz string
	if err := s.DB.QueryRow(ctx, `SELECT value #>> '{}' FROM settings WHERE key='quietEnd'`).Scan(&tz); err != nil || tz != "14:00" {
		t.Fatalf("persisted quiet end = %q %v", tz, err)
	}
}

func TestStaggerEndpoint(t *testing.T) {
	s, _ := scheduleStore(t)
	ctx := t.Context()
	_, tag, _ := s.Schedules(ctx)
	if _, _, err := s.Stagger(ctx, `"nope"`); !errors.Is(err, ErrStale) {
		t.Fatalf("stale stagger = %v", err)
	}
	data, newTag, err := s.Stagger(ctx, tag)
	if err != nil || newTag == tag {
		t.Fatalf("stagger: %v %q", err, newTag)
	}
	nodes := nodeViews(t, data)
	if nodes[uid(1)]["config"].(map[string]any)["offset"] != 0 || nodes[uid(2)]["config"].(map[string]any)["offset"] != 10 || nodes[uid(3)]["config"].(map[string]any)["offset"] != 20 {
		t.Fatalf("offsets = %v", nodes)
	}
	// Repeating it with the new tag changes nothing.
	again, sameTag, err := s.Stagger(ctx, newTag)
	if err != nil || sameTag != newTag || len(nodeViews(t, again)) != 3 {
		t.Fatalf("idempotent stagger: %v %q", err, sameTag)
	}
}
