package stats

import (
	"errors"
	"slices"
	"testing"
	"time"
)

// statisticsLatency keeps the complete store-call cost, including row decoding.
// Query errors stop measurement immediately; they are never retried.
func statisticsLatency(run func() error, now func() time.Time) (time.Duration, time.Duration, []time.Duration, error) {
	const attempts = 5
	samples := make([]time.Duration, 0, attempts)
	for range attempts {
		started := now()
		if err := run(); err != nil {
			return 0, 0, samples, err
		}
		elapsed := now().Sub(started)
		samples = append(samples, elapsed)
	}
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	return sorted[len(sorted)/2], sorted[len(sorted)-1], samples, nil
}

func TestStatisticsLatency(t *testing.T) {
	tests := []struct {
		durations []time.Duration
		p50, p95  time.Duration
	}{
		{[]time.Duration{900 * time.Millisecond, 400 * time.Millisecond, 200 * time.Millisecond, 600 * time.Millisecond, 700 * time.Millisecond}, 600 * time.Millisecond, 900 * time.Millisecond},
		{[]time.Duration{300 * time.Millisecond, 400 * time.Millisecond, 500 * time.Millisecond, 600 * time.Millisecond, 700 * time.Millisecond}, 500 * time.Millisecond, 700 * time.Millisecond},
	}
	for _, tc := range tests {
		clock := time.Unix(0, 0)
		calls := 0
		p50, p95, samples, err := statisticsLatency(func() error {
			clock = clock.Add(tc.durations[calls])
			calls++
			return nil
		}, func() time.Time { return clock })
		if err != nil || calls != 5 || p50 != tc.p50 || p95 != tc.p95 || !slices.Equal(samples, tc.durations) {
			t.Fatalf("p50=%v p95=%v samples=%v calls=%d err=%v; want p50=%v p95=%v samples=%v calls=5", p50, p95, samples, calls, err, tc.p50, tc.p95, tc.durations)
		}
	}
}

func TestStatisticsLatencyStopsOnQueryError(t *testing.T) {
	want := errors.New("query failed")
	calls := 0
	p50, p95, samples, err := statisticsLatency(func() error {
		calls++
		return want
	}, time.Now)
	if !errors.Is(err, want) || calls != 1 || p50 != 0 || p95 != 0 || len(samples) != 0 {
		t.Fatalf("p50=%v p95=%v samples=%v calls=%d err=%v; want original error after one call", p50, p95, samples, calls, err)
	}
}
