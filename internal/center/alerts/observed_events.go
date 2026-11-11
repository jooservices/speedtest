package alerts

import (
	"context"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jooservices/speedtest/internal/center/agentapi"
	"github.com/jooservices/speedtest/internal/center/catalog"
)

type observedBatchContextKey struct{}

type observedBatchState struct {
	lastISP        map[string]observedISP
	pinnedLoaded   bool
	pinnedServerID *int
	medianSettings map[string]effectiveEventSettings
}

type observedISP struct {
	ts    time.Time
	id    string
	value string
}

func (s *Store) prepareObservedBatch(ctx context.Context, node string, results []agentapi.Result) (context.Context, error) {
	state := &observedBatchState{lastISP: make(map[string]observedISP), medianSettings: make(map[string]effectiveEventSettings)}
	needsPinnedServer := false
	medianEngines := make(map[string]bool)
	needsCache := false
	for _, result := range results {
		if result.EngineISP != nil && *result.EngineISP != "" {
			needsCache = true
		}
		if result.ServerID != nil {
			needsPinnedServer, needsCache = true, true
		}
		if result.Status == "ok" && result.Down != nil && result.Up != nil {
			engine := measurementEngine(result.Engine)
			medianEngines[engine], needsCache = true, true
		}
	}
	if !needsCache {
		return ctx, nil
	}
	if needsPinnedServer {
		if err := s.DB.QueryRow(ctx, `SELECT server_id FROM nodes WHERE id=$1`, node).Scan(&state.pinnedServerID); err != nil {
			return ctx, err
		}
		state.pinnedLoaded = true
	}
	for engine := range medianEngines {
		config, err := resolveEventSettings(ctx, s.DB, "quality.drop_vs_median", node, "warn", []string{"telegram"})
		if err != nil {
			return ctx, err
		}
		state.medianSettings[engine] = config
	}
	return context.WithValue(ctx, observedBatchContextKey{}, state), nil
}

func (s *Store) emitObservedEvents(ctx context.Context, tx pgx.Tx, node string, result agentapi.Result, ts time.Time) error {
	if err := s.emitISPChange(ctx, tx, node, result, ts); err != nil {
		return err
	}
	if err := s.emitServerMismatch(ctx, tx, node, result, ts); err != nil {
		return err
	}
	return s.emitMedianDrop(ctx, tx, node, result, ts)
}

func (s *Store) emitISPChange(ctx context.Context, tx pgx.Tx, node string, result agentapi.Result, ts time.Time) error {
	if result.EngineISP == nil || *result.EngineISP == "" {
		return nil
	}
	engine := measurementEngine(result.Engine)
	state, _ := ctx.Value(observedBatchContextKey{}).(*observedBatchState)
	if state != nil {
		if previous, ok := state.lastISP[engine]; ok && resultBefore(previous.ts, previous.id, ts, result.ID) {
			state.lastISP[engine] = observedISP{ts: ts, id: result.ID, value: *result.EngineISP}
			if previous.value == *result.EngineISP {
				return nil
			}
			return s.emitObservedCatalogEvent(ctx, tx, "network.isp_changed", node, result, ts,
				map[string]string{"isp": *result.EngineISP, "old_isp": previous.value})
		}
	}
	var previous *string
	err := tx.QueryRow(ctx, `SELECT engine_isp FROM results WHERE node_id=$1 AND engine=$2 AND (ts,id)<($3,$4) ORDER BY ts DESC,id DESC LIMIT 1`, node, engine, ts, result.ID).Scan(&previous)
	if errors.Is(err, pgx.ErrNoRows) {
		rememberISP(state, engine, ts, result.ID, *result.EngineISP)
		return nil
	}
	if err != nil {
		return err
	}
	rememberISP(state, engine, ts, result.ID, *result.EngineISP)
	if previous == nil || *previous == *result.EngineISP {
		return nil
	}
	return s.emitObservedCatalogEvent(ctx, tx, "network.isp_changed", node, result, ts, map[string]string{"isp": *result.EngineISP, "old_isp": *previous})
}

func rememberISP(state *observedBatchState, engine string, ts time.Time, id, value string) {
	if state == nil {
		return
	}
	previous, ok := state.lastISP[engine]
	if !ok || resultBefore(previous.ts, previous.id, ts, id) {
		state.lastISP[engine] = observedISP{ts: ts, id: id, value: value}
	}
}

func resultBefore(leftTS time.Time, leftID string, rightTS time.Time, rightID string) bool {
	return leftTS.Before(rightTS) || (leftTS.Equal(rightTS) && leftID < rightID)
}

func (s *Store) emitServerMismatch(ctx context.Context, tx pgx.Tx, node string, result agentapi.Result, ts time.Time) error {
	if result.ServerID == nil {
		return nil
	}
	state, _ := ctx.Value(observedBatchContextKey{}).(*observedBatchState)
	var pinned *int
	if state != nil && state.pinnedLoaded {
		pinned = state.pinnedServerID
	} else {
		if err := tx.QueryRow(ctx, `SELECT server_id FROM nodes WHERE id=$1`, node).Scan(&pinned); err != nil {
			return err
		}
	}
	if pinned == nil || *pinned == *result.ServerID {
		return nil
	}
	server := "unknown"
	if result.Server != nil {
		server = result.Server.Name
	}
	return s.emitObservedCatalogEvent(ctx, tx, "network.server_differs", node, result, ts,
		map[string]string{"server": server, "expected": strconv.Itoa(*pinned)})
}

func (s *Store) emitMedianDrop(ctx context.Context, tx pgx.Tx, node string, result agentapi.Result, ts time.Time) error {
	if result.Status != "ok" || result.Down == nil || result.Up == nil {
		return nil
	}
	engine := measurementEngine(result.Engine)
	state, _ := ctx.Value(observedBatchContextKey{}).(*observedBatchState)
	config, cached := effectiveEventSettings{}, false
	if state != nil {
		config, cached = state.medianSettings[engine]
	}
	var err error
	if !cached {
		config, err = resolveEventSettings(ctx, tx, "quality.drop_vs_median", node, "warn", []string{"telegram"})
	}
	if err != nil || !config.Enabled {
		return err
	}
	threshold := config.Thresholds["drop_pct"]
	if threshold <= 0 || threshold > 100 {
		return nil
	}
	var medianDown, medianUp *float64
	var samples int
	err = tx.QueryRow(ctx, `SELECT percentile_cont(0.5) WITHIN GROUP(ORDER BY down_mbps) FILTER(WHERE down_mbps IS NOT NULL),
		percentile_cont(0.5) WITHIN GROUP(ORDER BY up_mbps) FILTER(WHERE up_mbps IS NOT NULL),count(*)
		FROM results WHERE node_id=$1 AND engine=$2 AND status='ok' AND ts >= $3::timestamptz-interval '7 days'
		AND (ts,id)<($3::timestamptz,$4::uuid)`, node, engine, ts, result.ID).
		Scan(&medianDown, &medianUp, &samples)
	if err != nil || samples < 3 || medianDown == nil || medianUp == nil {
		return err
	}
	downPct, upPct := *result.Down / *medianDown * 100, *result.Up / *medianUp * 100
	if downPct >= 100-threshold && upPct >= 100-threshold {
		return nil
	}
	value := math.Min(downPct, upPct)
	extra := map[string]string{"value": strconv.FormatFloat(value, 'f', 1, 64), "threshold": strconv.FormatFloat(100-threshold, 'f', 1, 64), "expected": "7-day median", "pct": strconv.FormatFloat(value, 'f', 1, 64)}
	return s.emitObservedCatalogEvent(ctx, tx, "quality.drop_vs_median", node, result, ts, extra)
}

func (s *Store) emitObservedCatalogEvent(ctx context.Context, tx pgx.Tx, key, node string, result agentapi.Result, ts time.Time, extra map[string]string) error {
	event, ok := catalog.LookupEvent(key)
	if !ok {
		return nil
	}
	config, err := resolveEventSettings(ctx, tx, key, node, event.Severity, event.Channels)
	if err != nil {
		return err
	}
	var name string
	var expectedDown, expectedUp float64
	if err := tx.QueryRow(ctx, `SELECT name,expected_down_mbps,expected_up_mbps FROM nodes WHERE id=$1`, node).
		Scan(&name, &expectedDown, &expectedUp); err != nil {
		return err
	}
	limits, err := s.qualityLimits(ctx, tx, node, result.Engine)
	if err != nil {
		return err
	}
	zone, err := ownerTimezone(ctx, tx)
	if err != nil {
		return err
	}
	url, err := s.centerURL(ctx, tx)
	if err != nil {
		return err
	}
	extra["since"] = formatOwnerTime(ts, zone)
	payload := BuildPayload(key, name, url, config.Severity, result, expectedDown, expectedUp, limits, extra)
	_, err = s.insertCatalogEventWithSettings(ctx, tx, key, node, event.Title, event.Description, result.ID, payload, ts, config)
	return err
}
