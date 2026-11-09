package ownerapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jooservices/speedtest/internal/center/schedules"
)

// ScheduleInput is the owner-editable part of one node's schedule.
type ScheduleInput struct {
	Engines  []EngineSchedule
	Interval int
	Offset   int
	Enabled  bool
}

// ScheduleRules are the center-wide coordination rules.
type ScheduleRules struct {
	Stagger       bool
	MaxConcurrent int
	Quiet         schedules.Quiet
}

// usageSamples is how many recent non-failed results estimate a node's data usage.
const usageSamples = 200

type scheduleRow struct {
	Engine       string
	NodeID       string
	NodeEnabled  bool
	Interval     int
	Offset       int
	Enabled      bool
	Revision     int64
	AverageBytes float64
}

type queryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// scheduleETag is the strong entity tag of one node schedule.
func scheduleETag(revision int64) string { return fmt.Sprintf(`"schedule-%d"`, revision) }

func (s *Store) scheduleRows(ctx context.Context, q queryer, lock bool, ids ...string) ([]scheduleRow, error) {
	query := `SELECT n.id::text, n.enabled, s.engine, s.interval_minutes, s.offset_minutes, s.enabled, s.revision,
		COALESCE((SELECT avg(COALESCE(t.bytes_down,0) + COALESCE(t.bytes_up,0)) FROM (
			SELECT r.bytes_down, r.bytes_up FROM results r WHERE r.node_id = n.id AND r.engine=s.engine AND r.status <> 'failed'
			ORDER BY r.ts DESC, r.id DESC LIMIT ` + fmt.Sprint(usageSamples) + `) t), 0)
		FROM nodes n JOIN engine_schedules s ON s.node_id = n.id`
	var args []any
	if len(ids) > 0 {
		query += ` WHERE n.id = ANY($1::uuid[])`
		args = append(args, ids)
	}
	query += ` ORDER BY n.id,s.engine`
	if lock {
		query += ` FOR UPDATE OF s`
	}
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []scheduleRow
	for rows.Next() {
		var r scheduleRow
		if err := rows.Scan(&r.NodeID, &r.NodeEnabled, &r.Engine, &r.Interval, &r.Offset, &r.Enabled, &r.Revision, &r.AverageBytes); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ruleKeys are the settings rows that hold the coordination rules.
var ruleKeys = []string{"stagger", "maxConcurrent", "quietStart", "quietEnd", "timezone"}

func (s *Store) loadRules(ctx context.Context, q queryer) (ScheduleRules, []int64, error) {
	rules := ScheduleRules{Stagger: true, MaxConcurrent: 1, Quiet: schedules.Quiet{Timezone: "UTC"}}
	rows, err := q.Query(ctx, `SELECT key, value, revision FROM settings WHERE key = ANY($1) AND value IS NOT NULL ORDER BY key`, ruleKeys)
	if err != nil {
		return rules, nil, err
	}
	defer rows.Close()
	var revisions []int64
	for rows.Next() {
		var key string
		var raw []byte
		var rev int64
		if err := rows.Scan(&key, &raw, &rev); err != nil {
			return rules, nil, err
		}
		revisions = append(revisions, rev)
		switch key {
		case "stagger":
			_ = json.Unmarshal(raw, &rules.Stagger)
		case "maxConcurrent":
			var n int
			if json.Unmarshal(raw, &n) == nil && n >= 1 && n <= 5 {
				rules.MaxConcurrent = n
			}
		case "quietStart":
			_ = json.Unmarshal(raw, &rules.Quiet.Start)
		case "quietEnd":
			_ = json.Unmarshal(raw, &rules.Quiet.End)
		case "timezone":
			var tz string
			if json.Unmarshal(raw, &tz) == nil && tz != "" {
				rules.Quiet.Timezone = tz
			}
		}
	}
	return rules, revisions, rows.Err()
}

func rulesView(r ScheduleRules) map[string]any {
	return map[string]any{"stagger": r.Stagger, "maxConcurrent": r.MaxConcurrent,
		"quiet": map[string]any{"quietStart": r.Quiet.Start, "quietEnd": r.Quiet.End, "timezone": r.Quiet.Timezone}}
}

func (s *Store) scheduleView(r scheduleRow, q schedules.Quiet, now time.Time) map[string]any {
	slot := schedules.Slot{Interval: r.Interval, Offset: r.Offset}
	view := map[string]any{"nodeId": r.NodeID, "engine": r.Engine, "config": map[string]any{"interval": r.Interval, "offset": r.Offset, "enabled": r.Enabled},
		"nextRun": nil, "upcoming": []string{}, "bytesPerDay": 0.0, "bytesPerMonth": 0.0}
	if !r.Enabled || !r.NodeEnabled {
		return view
	}
	runs := schedules.NextRuns(slot, q, now, schedules.MaxUpcoming)
	upcoming := make([]string, len(runs))
	for i, t := range runs {
		upcoming[i] = t.Format(time.RFC3339)
	}
	view["upcoming"] = upcoming
	if len(runs) > 0 {
		view["nextRun"] = upcoming[0]
	}
	perDay := r.AverageBytes * float64(schedules.RunsPerDay(slot, q, now))
	view["bytesPerDay"], view["bytesPerMonth"] = perDay, perDay*30
	return view
}

// collectionETag changes whenever any schedule or coordination rule changes, or a node is added or removed.
func collectionETag(rows []scheduleRow, ruleRevisions []int64) string {
	h := sha256.New()
	for _, r := range rows {
		_, _ = fmt.Fprintf(h, "%s:%s:%d;", r.NodeID, r.Engine, r.Revision)
	}
	_, _ = fmt.Fprint(h, "|")
	for _, rev := range ruleRevisions {
		_, _ = fmt.Fprintf(h, "%d;", rev)
	}
	return `"schedules-` + hex.EncodeToString(h.Sum(nil))[:16] + `"`
}

func (s *Store) collection(ctx context.Context, q queryer, lock bool) (map[string]any, string, []scheduleRow, ScheduleRules, error) {
	rows, err := s.scheduleRows(ctx, q, lock)
	if err != nil {
		return nil, "", nil, ScheduleRules{}, err
	}
	rules, revisions, err := s.loadRules(ctx, q)
	if err != nil {
		return nil, "", nil, rules, err
	}
	now := s.now()
	nodes := make([]map[string]any, len(rows))
	slots := make([]schedules.Node, len(rows))
	for i, r := range rows {
		nodes[i] = s.scheduleView(r, rules.Quiet, now)
		slots[i] = schedules.Node{ID: r.NodeID, Interval: r.Interval, Offset: r.Offset, Enabled: r.Enabled && r.NodeEnabled}
	}
	clashes := []map[string]any{}
	for _, c := range schedules.Clashes(slots, rules.MaxConcurrent) {
		if c.A == c.B {
			continue
		}
		clashes = append(clashes, map[string]any{"nodes": []string{c.A, c.B}, "detail": c.Detail})
	}
	data := map[string]any{"rules": rulesView(rules), "nodes": nodes, "clashes": clashes}
	return data, collectionETag(rows, revisions), rows, rules, nil
}

// Schedules returns the coordination rules, every node's schedule and the clashes between them.
func (s *Store) Schedules(ctx context.Context) (map[string]any, string, error) {
	data, etag, _, _, err := s.collection(ctx, s.DB, false)
	return data, etag, err
}

// Schedule returns one node's schedule with its upcoming runs and data-usage estimate.
func (s *Store) Schedule(ctx context.Context, id string) (map[string]any, string, error) {
	if !validUUID(id) {
		return nil, "", ErrNotFound
	}
	rows, err := s.scheduleRows(ctx, s.DB, false, id)
	if err != nil {
		return nil, "", err
	}
	if len(rows) == 0 {
		return nil, "", ErrNotFound
	}
	rules, _, err := s.loadRules(ctx, s.DB)
	if err != nil {
		return nil, "", err
	}
	views := make([]map[string]any, 0, len(rows))
	primary := 0
	for i, r := range rows {
		views = append(views, s.scheduleView(r, rules.Quiet, s.now()))
		if r.Engine == "ookla" {
			primary = i
		}
	}
	view := s.scheduleView(rows[primary], rules.Quiet, s.now())
	view["engines"] = views
	return view, nodeScheduleETag(rows), nil
}

// ReplaceSchedule sets one node's schedule under an If-Match precondition. The offset is clamped below the interval.
func (s *Store) ReplaceSchedule(ctx context.Context, id string, in ScheduleInput, ifMatch string) (map[string]any, string, error) {
	if !validUUID(id) {
		return nil, "", ErrNotFound
	}
	if in.Engines != nil {
		return s.replaceEngines(ctx, id, in.Engines, ifMatch)
	}
	if in.Interval < schedules.MinInterval || in.Interval > schedules.MaxInterval || in.Offset < 0 {
		return nil, "", ErrInvalid
	}
	slot := schedules.Clamp(in.Interval, in.Offset)
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := s.scheduleRows(ctx, tx, true, id)
	if err != nil {
		return nil, "", err
	}
	if len(rows) == 0 {
		return nil, "", ErrNotFound
	}
	if ifMatch != nodeScheduleETag(rows) {
		return nil, "", ErrStale
	}
	if _, err := tx.Exec(ctx, `UPDATE schedules SET interval_minutes=$2, offset_minutes=$3, enabled=$4, revision=revision+1, updated_at=now() WHERE node_id=$1`,
		id, slot.Interval, slot.Offset, in.Enabled); err != nil {
		return nil, "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE engine_schedules SET interval_minutes=$2,offset_minutes=$3,enabled=$4,revision=revision+1 WHERE node_id=$1 AND engine='ookla'`, id, slot.Interval, slot.Offset, in.Enabled); err != nil {
		return nil, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, "", err
	}
	return s.Schedule(ctx, id)
}

func upsertSetting(ctx context.Context, tx pgx.Tx, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO settings (key, value) VALUES ($1, $2::jsonb)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, secret_ciphertext = NULL, revision = settings.revision + 1, updated_at = now()`, key, string(raw))
	return err
}

// applyStagger rewrites the offsets of every node and reports how many changed.
func applyStagger(ctx context.Context, tx pgx.Tx, rows []scheduleRow) error {
	groups := map[string][]scheduleRow{}
	for _, r := range rows {
		groups[r.Engine] = append(groups[r.Engine], r)
	}
	for name, group := range groups {
		if err := staggerEngine(ctx, tx, group, name); err != nil {
			return err
		}
	}
	return nil
}
func staggerEngine(ctx context.Context, tx pgx.Tx, rows []scheduleRow, engine string) error {
	nodes := make([]schedules.Node, len(rows))
	for i, r := range rows {
		nodes[i] = schedules.Node{ID: r.NodeID, Interval: r.Interval, Offset: r.Offset, Enabled: r.Enabled}
	}
	offsets := schedules.Stagger(nodes)
	ids := make([]string, 0, len(offsets))
	for id := range offsets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if _, err := tx.Exec(ctx, `UPDATE engine_schedules SET offset_minutes=$2, revision=revision+1 WHERE node_id=$1 AND engine=$3 AND offset_minutes <> $2`, id, offsets[id], engine); err != nil {
			return err
		}
		if engine != "ookla" {
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE schedules SET offset_minutes=$2, revision=revision+1, updated_at=now() WHERE node_id=$1 AND offset_minutes <> $2`, id, offsets[id]); err != nil {
			return err
		}
	}
	return nil
}

// ReplaceRules stores the coordination rules under an If-Match precondition. Every node's configuration
// revision moves so agents pick up quiet hours; with stagger on, offsets are respread at the same time.
func (s *Store) ReplaceRules(ctx context.Context, in ScheduleRules, ifMatch string) (map[string]any, string, error) {
	if in.MaxConcurrent < 1 || in.MaxConcurrent > 5 || in.Quiet.Validate() != nil {
		return nil, "", ErrInvalid
	}
	if in.Quiet.Timezone == "" {
		in.Quiet.Timezone = "UTC"
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, etag, rows, _, err := s.collection(ctx, tx, true)
	if err != nil {
		return nil, "", err
	}
	if ifMatch != etag {
		return nil, "", ErrStale
	}
	for key, value := range map[string]any{"stagger": in.Stagger, "maxConcurrent": in.MaxConcurrent, "quietStart": in.Quiet.Start,
		"quietEnd": in.Quiet.End, "timezone": in.Quiet.Timezone} {
		if err := upsertSetting(ctx, tx, key, value); err != nil {
			return nil, "", err
		}
	}
	if in.Stagger {
		if err := applyStagger(ctx, tx, rows); err != nil {
			return nil, "", err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE engine_schedules SET revision=revision+1`); err != nil {
		return nil, "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE schedules SET revision=revision+1, updated_at=now()`); err != nil {
		return nil, "", err
	}
	data, newTag, _, _, err := s.collection(ctx, tx, false)
	if err != nil {
		return nil, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, "", err
	}
	return data["rules"].(map[string]any), newTag, nil
}

// Stagger spreads the offsets of all nodes evenly under an If-Match precondition and returns the new schedules.
func (s *Store) Stagger(ctx context.Context, ifMatch string) (map[string]any, string, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, etag, rows, _, err := s.collection(ctx, tx, true)
	if err != nil {
		return nil, "", err
	}
	if ifMatch != etag {
		return nil, "", ErrStale
	}
	if err := applyStagger(ctx, tx, rows); err != nil {
		return nil, "", err
	}
	data, newTag, _, _, err := s.collection(ctx, tx, false)
	if err != nil {
		return nil, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, "", err
	}
	return data, newTag, nil
}

func nodeScheduleETag(rows []scheduleRow) string {
	if len(rows) == 1 {
		return scheduleETag(rows[0].Revision)
	}
	return collectionETag(rows, nil)
}
