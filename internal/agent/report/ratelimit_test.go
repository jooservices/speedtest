package report_test

import (
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/agent/report"
	"github.com/jooservices/speedtest/internal/shared/failureclass"
)

func TestRateLimitGateRecordsOncePerWindow(t *testing.T) {
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	gate := report.NewRateLimitGate(report.RateLimitBackoffConfig{
		Initial: time.Minute, Max: 4 * time.Minute, Factor: 2, Jitter: 0,
	}, func() time.Time { return now })

	if !gate.ObserveFailure("ookla", failureclass.RateLimited) {
		t.Fatal("first rate-limited failure must be recorded")
	}
	if gate.ObserveFailure("ookla", failureclass.RateLimited) {
		t.Fatal("second failure in the same window must not be recorded")
	}
	if !gate.Blocked("ookla") {
		t.Fatal("engine must stay blocked during the backoff window")
	}
	if gate.Blocked("librespeed") {
		t.Fatal("backoff is per engine")
	}
	if !gate.ObserveFailure("librespeed", failureclass.Timeout) {
		t.Fatal("non-rate-limited failures always record")
	}

	now = now.Add(time.Minute)
	if !gate.ObserveFailure("ookla", failureclass.RateLimited) {
		t.Fatal("a new window after expiry records one failure again")
	}
	gate.ObserveSuccess("ookla")
	if gate.Blocked("ookla") {
		t.Fatal("success clears backoff")
	}
}

func TestRateLimitPlanLeavesOrdinaryFailuresUnchanged(t *testing.T) {
	gate := report.NewRateLimitGate(report.DefaultRateLimitBackoff(), nil)
	if record, state := gate.PlanFailure("ookla", failureclass.Timeout); !record || state != nil {
		t.Fatalf("ordinary failure plan = record %t, state %+v", record, state)
	}
	if blocked := gate.Blocked("ookla"); blocked {
		t.Fatal("ordinary failure opened a provider backoff")
	}
}

func TestRateLimitBackoffConfigFromEnv(t *testing.T) {
	t.Setenv("SPEEDTEST_RATE_LIMIT_BACKOFF_INITIAL", "30s")
	t.Setenv("SPEEDTEST_RATE_LIMIT_BACKOFF_MAX", "10m")
	t.Setenv("SPEEDTEST_RATE_LIMIT_BACKOFF_FACTOR", "3")
	t.Setenv("SPEEDTEST_RATE_LIMIT_BACKOFF_JITTER", "0.1")
	cfg, err := report.RateLimitBackoffConfigFromEnv()
	if err != nil || cfg.Initial != 30*time.Second || cfg.Max != 10*time.Minute || cfg.Factor != 3 || cfg.Jitter != 0.1 {
		t.Fatalf("cfg = %+v err = %v", cfg, err)
	}
	t.Setenv("SPEEDTEST_RATE_LIMIT_BACKOFF_FACTOR", "0.5")
	if _, err := report.RateLimitBackoffConfigFromEnv(); err == nil {
		t.Fatal("invalid factor accepted")
	}
	t.Setenv("SPEEDTEST_RATE_LIMIT_BACKOFF_FACTOR", "2")
	t.Setenv("SPEEDTEST_RATE_LIMIT_BACKOFF_INITIAL", "bad")
	if _, err := report.RateLimitBackoffConfigFromEnv(); err == nil {
		t.Fatal("invalid initial accepted")
	}
	t.Setenv("SPEEDTEST_RATE_LIMIT_BACKOFF_INITIAL", "30s")
	t.Setenv("SPEEDTEST_RATE_LIMIT_BACKOFF_MAX", "1s")
	if _, err := report.RateLimitBackoffConfigFromEnv(); err == nil {
		t.Fatal("max below initial accepted")
	}
	t.Setenv("SPEEDTEST_RATE_LIMIT_BACKOFF_MAX", "10m")
	t.Setenv("SPEEDTEST_RATE_LIMIT_BACKOFF_JITTER", "2")
	if _, err := report.RateLimitBackoffConfigFromEnv(); err == nil {
		t.Fatal("invalid jitter accepted")
	}
	t.Setenv("SPEEDTEST_RATE_LIMIT_BACKOFF_JITTER", "0.2")
	t.Setenv("SPEEDTEST_RATE_LIMIT_BACKOFF_MAX", "")
	t.Setenv("SPEEDTEST_RATE_LIMIT_BACKOFF_INITIAL", "2h")
	if _, err := report.RateLimitBackoffConfigFromEnv(); err == nil {
		t.Fatal("initial above default max accepted")
	}
	t.Setenv("SPEEDTEST_RATE_LIMIT_BACKOFF_MAX", "2h")
	if cfg, err := report.RateLimitBackoffConfigFromEnv(); err != nil || cfg.Initial != cfg.Max {
		t.Fatalf("equal configured bounds rejected: %+v %v", cfg, err)
	}
}

func TestRateLimitGateDefaultsAndCap(t *testing.T) {
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	gate := report.NewRateLimitGate(report.RateLimitBackoffConfig{Jitter: 1.5}, func() time.Time { return now })
	if !gate.ObserveFailure("", failureclass.RateLimited) {
		t.Fatal("empty engine key must still record")
	}
	if !gate.Blocked("") {
		t.Fatal("empty engine must share the ookla key")
	}
	live := report.NewRateLimitGate(report.RateLimitBackoffConfig{Initial: time.Second, Max: time.Second, Factor: 2, Jitter: -1}, nil)
	if !live.ObserveFailure("ookla", failureclass.RateLimited) || !live.Blocked("ookla") {
		t.Fatal("nil clock and repaired jitter must still open a window")
	}
	def := report.DefaultRateLimitBackoff()
	if def.Initial != time.Minute || def.Max != time.Hour {
		t.Fatalf("defaults = %+v", def)
	}
	// Drive attempts until the delay hits the cap.
	for i := 0; i < 20; i++ {
		now = now.Add(2 * time.Hour)
		_ = gate.ObserveFailure("ookla", failureclass.RateLimited)
	}
}

func TestRateLimitGatePlansJitterGrowthCapAndRestore(t *testing.T) {
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	gate := report.NewRateLimitGate(report.RateLimitBackoffConfig{Initial: time.Second, Max: 4 * time.Second, Factor: 2, Jitter: 0.2}, func() time.Time { return now })
	for attempt, base := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second} {
		record, state := gate.PlanFailure("ookla", failureclass.RateLimited)
		if !record || state == nil {
			t.Fatalf("attempt %d was not planned", attempt)
		}
		delay := state.Deadline.Sub(now)
		if delay < base*4/5 || delay > base || state.Attempt != attempt+1 || !state.Recorded {
			t.Fatalf("attempt %d state = %+v, delay %s, base %s", attempt, state, delay, base)
		}
		gate.CommitFailure("ookla", *state)
		deadline, exists := gate.Deadline("ookla")
		if !exists || !deadline.Equal(state.Deadline) {
			t.Fatalf("deadline = %s, exists = %t; want %s", deadline, exists, state.Deadline)
		}
		if record, next := gate.PlanFailure("ookla", failureclass.RateLimited); record || next != nil {
			t.Fatal("active window planned a second recorded failure")
		}
		now = state.Deadline
	}
	stateMap := map[string]report.RateLimitState{}
	_, saved := gate.PlanFailure("ookla", failureclass.RateLimited)
	if saved == nil {
		t.Fatal("expired window did not plan its next attempt")
	}
	stateMap["ookla"] = *saved
	restarted := report.NewRateLimitGate(report.RateLimitBackoffConfig{Initial: time.Second, Max: 4 * time.Second, Factor: 2}, func() time.Time { return now })
	restarted.Restore(stateMap)
	if !restarted.Blocked("ookla") {
		t.Fatal("restored engine deadline was not enforced")
	}
	restarted.ObserveSuccess("ookla")
	if restarted.Blocked("ookla") {
		t.Fatal("success did not clear restored state")
	}
}
