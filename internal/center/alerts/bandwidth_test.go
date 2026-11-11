package alerts

import (
	"context"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/agentapi"
)

func TestBandwidthThresholdCrossings(t *testing.T) {
	ts := time.Date(2026, time.January, 15, 12, 0, 0, 0, time.UTC)
	if !bandwidthThresholdReached("bandwidth.budget_80", 79, 80, ts, 100) ||
		bandwidthThresholdReached("bandwidth.budget_80", 80, 81, ts, 100) {
		t.Fatal("80 percent threshold must fire only when usage crosses it")
	}
	if !bandwidthThresholdReached("bandwidth.budget_100", 99, 100, ts, 100) ||
		bandwidthThresholdReached("bandwidth.budget_100", 100, 101, ts, 100) {
		t.Fatal("100 percent threshold must fire only when usage crosses it")
	}
	if !bandwidthThresholdReached("bandwidth.projection_over", 45, 50, ts, 100) ||
		bandwidthThresholdReached("bandwidth.projection_over", 50, 60, ts, 100) {
		t.Fatal("projection threshold must fire only when projected usage crosses budget")
	}
	if bandwidthThresholdReached("unknown", 0, 100, ts, 1) || monthlyProjection(0, ts) != 0 {
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
