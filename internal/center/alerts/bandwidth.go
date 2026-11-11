package alerts

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jooservices/speedtest/internal/center/agentapi"
	"github.com/jooservices/speedtest/internal/center/catalog"
)

type bandwidthBudgetBatchKey struct{}

func (s *Store) emitBandwidthEvents(ctx context.Context, tx pgx.Tx, node string, result agentapi.Result, ts time.Time) error {
	configured, prepared := ctx.Value(bandwidthBudgetBatchKey{}).(bool)
	if !prepared {
		var err error
		configured, err = configuredBandwidthBudget(ctx, tx)
		if err != nil {
			return err
		}
	}
	if !configured {
		return nil
	}
	var name string
	var current, previous float64
	err := tx.QueryRow(ctx, `SELECT n.name,
		COALESCE((SELECT sum(COALESCE(x.bytes_down,0)+COALESCE(x.bytes_up,0)) FROM results x
			WHERE x.node_id=r.node_id AND x.engine=r.engine AND x.status<>'failed'
			AND x.ts>=date_trunc('month',r.ts) AND x.ts<=r.ts),0),
		COALESCE((SELECT sum(COALESCE(x.bytes_down,0)+COALESCE(x.bytes_up,0)) FROM results x
			WHERE x.node_id=r.node_id AND x.engine=r.engine AND x.status<>'failed'
			AND x.ts>=date_trunc('month',r.ts) AND (x.ts<r.ts OR (x.ts=r.ts AND x.id<r.id))),0)
		FROM results r JOIN nodes n ON n.id=r.node_id WHERE r.id=$1 AND r.node_id=$2`, result.ID, node).
		Scan(&name, &current, &previous)
	if err != nil {
		return err
	}
	currentGB, previousGB := current/1e9, previous/1e9
	for _, key := range bandwidthEventKeys() {
		event, _ := catalog.LookupEvent(key)
		config, err := resolveEventSettings(ctx, tx, key, node, event.Severity, event.Channels)
		if err != nil {
			return err
		}
		budget := config.Thresholds["monthly_gb"]
		if !config.Enabled || budget <= 0 || !bandwidthThresholdReached(key, previousGB, currentGB, ts, budget) {
			continue
		}
		payload := bandwidthPayload(key, name, config.Severity, result, currentGB, budget, ts)
		if _, err := s.insertCatalogEventWithSettings(ctx, tx, key, node, event.Title, event.Description, result.ID, payload, ts, config); err != nil {
			return err
		}
	}
	return nil
}

func configuredBandwidthBudget(ctx context.Context, q notificationQuerier) (bool, error) {
	var configured bool
	err := q.QueryRow(ctx, `/* bandwidth_budget_configured */
		SELECT EXISTS (SELECT 1 FROM notification_event_settings
		WHERE event_key=ANY($1::text[]) AND enabled AND cardinality(channels)>0
		AND COALESCE((thresholds->>'monthly_gb')::float8,0)>0)`, bandwidthEventKeys()).Scan(&configured)
	return configured, err
}

func bandwidthEventKeys() []string {
	return []string{"bandwidth.budget_80", "bandwidth.budget_100", "bandwidth.projection_over"}
}

func bandwidthThresholdReached(key string, previous, current float64, ts time.Time, budget float64) bool {
	switch key {
	case "bandwidth.budget_80":
		return previous < budget*0.8 && current >= budget*0.8
	case "bandwidth.budget_100":
		return previous < budget && current >= budget
	case "bandwidth.projection_over":
		return monthlyProjection(previous, ts) <= budget && monthlyProjection(current, ts) > budget
	default:
		return false
	}
}

func monthlyProjection(usageGB float64, ts time.Time) float64 {
	start := time.Date(ts.Year(), ts.Month(), 1, 0, 0, 0, 0, ts.Location())
	monthDays := float64(time.Date(ts.Year(), ts.Month()+1, 0, 0, 0, 0, 0, ts.Location()).Day())
	elapsedDays := ts.Sub(start).Hours() / 24
	if elapsedDays < 1 {
		elapsedDays = 1
	}
	return usageGB / elapsedDays * monthDays
}

func bandwidthPayload(key, node string, severity string, result agentapi.Result, usage, budget float64, ts time.Time) map[string]string {
	threshold := budget
	if key == "bandwidth.budget_80" {
		threshold *= 0.8
	}
	return BuildPayload(key, node, "", severity, result, 0, 0, DefaultQualityLimits(), map[string]string{
		"value": formatNum(usage), "expected": formatNum(budget), "pct": formatNum(usage / budget * 100),
		"threshold": fmt.Sprintf("%s GB", formatNum(threshold)), "since": ts.UTC().Format(time.RFC3339),
	})
}
