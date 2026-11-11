package alerts

import (
	"context"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/agentapi"
)

func TestBandwidthThresholdCrossings(t *testing.T) {
	ts := time.Date(2026, time.January, 15, 12, 0, 0, 0, time.UTC)
	if !bandwidthThresholdReached("bandwidth.budget_80", 79, 80, ts, 100, 80) ||
		bandwidthThresholdReached("bandwidth.budget_80", 80, 81, ts, 100, 80) {
		t.Fatal("80 percent threshold must fire only when usage crosses it")
	}
	if !bandwidthThresholdReached("bandwidth.budget_100", 99, 100, ts, 100, 100) ||
		bandwidthThresholdReached("bandwidth.budget_100", 100, 101, ts, 100, 100) {
		t.Fatal("100 percent threshold must fire only when usage crosses it")
	}
	if !bandwidthThresholdReached("bandwidth.projection_over", 45, 50, ts, 100, 100) ||
		bandwidthThresholdReached("bandwidth.projection_over", 50, 60, ts, 100, 100) {
		t.Fatal("projection threshold must fire only when projected usage crosses budget")
	}
	if bandwidthThresholdReached("unknown", 0, 100, ts, 1, 100) || monthlyProjection(0, ts) != 0 {
		t.Fatal("unknown event or empty usage produced a bandwidth alert")
	}
}

func TestBandwidthEventsSkipUsageScanWithoutBudget(t *testing.T) {
	tx := &fakeTx{}
	store := &Store{}
	err := store.emitBandwidthEvents(context.Background(), tx, "node", agentapi.Result{ID: "result"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if tx.bandwidthConfigReads != 1 || tx.bandwidthUsageReads != 0 {
		t.Fatalf("bandwidth queries = config %d, usage %d; want config 1 and usage 0", tx.bandwidthConfigReads, tx.bandwidthUsageReads)
	}
}

func TestBandwidthPercentIsConfigurable(t *testing.T) {
	ts := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	if bandwidthPercent("bandwidth.budget_80", nil) != 80 || bandwidthPercent("bandwidth.budget_100", map[string]float64{"percent": 0}) != 100 || bandwidthPercent("bandwidth.budget_80", map[string]float64{"percent": 65}) != 65 {
		t.Fatal("the percent threshold defaults to 80 and 100 and is read from the event setting")
	}
	// A 50% threshold on a 100 GB budget fires when usage crosses 50 GB, not 80 GB.
	if !bandwidthThresholdReached("bandwidth.budget_80", 49, 51, ts, 100, 50) || bandwidthThresholdReached("bandwidth.budget_80", 79, 81, ts, 100, 50) {
		t.Fatal("custom percent")
	}
	payload := bandwidthPayload("bandwidth.budget_80", "Home", "warn", agentapi.Result{}, 51, 100, 50, ts)
	if payload["threshold"] != "50 GB" {
		t.Fatalf("threshold text follows the configured percent: %q", payload["threshold"])
	}
}
