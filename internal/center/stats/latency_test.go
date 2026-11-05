package stats

import (
	"errors"
	"slices"
	"testing"
	"time"
)

// bestStatisticsLatency keeps the complete store-call cost, including row decoding.
// Five samples tolerate transient contention without relaxing the latency target.
// Query errors stop measurement immediately; they are never retried.
func bestStatisticsLatency(run func() error, now func() time.Time) (time.Duration, []time.Duration, error) {
	const attempts = 5
	best := time.Duration(1<<63 - 1)
	samples := make([]time.Duration, 0, attempts)
	for range attempts {
		started := now()
		if err := run(); err != nil {
			return 0, samples, err
		}
		elapsed := now().Sub(started)
		samples = append(samples, elapsed)
		best = min(best, elapsed)
	}
	return best, samples, nil
}

func TestBestStatisticsLatency(t *testing.T) {
	for _, durations := range [][]time.Duration{
		{900 * time.Millisecond, 400 * time.Millisecond, 200 * time.Millisecond, 600 * time.Millisecond, 700 * time.Millisecond},
		{300 * time.Millisecond, 400 * time.Millisecond, 500 * time.Millisecond, 600 * time.Millisecond, 700 * time.Millisecond},
	} {
		clock := time.Unix(0, 0)
		calls := 0
		best, samples, err := bestStatisticsLatency(func() error {
			clock = clock.Add(durations[calls])
			calls++
			return nil
		}, func() time.Time { return clock })
		if err != nil || calls != 5 || best != slices.Min(durations) || !slices.Equal(samples, durations) {
			t.Fatalf("best=%v samples=%v calls=%d err=%v; want best=%v samples=%v calls=5", best, samples, calls, err, slices.Min(durations), durations)
		}
	}
}

func TestBestStatisticsLatencyStopsOnQueryError(t *testing.T) {
	want := errors.New("query failed")
	calls := 0
	best, samples, err := bestStatisticsLatency(func() error {
		calls++
		return want
	}, time.Now)
	if !errors.Is(err, want) || calls != 1 || best != 0 || len(samples) != 0 {
		t.Fatalf("best=%v samples=%v calls=%d err=%v; want original error after one call", best, samples, calls, err)
	}
}
