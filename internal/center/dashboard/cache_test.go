package dashboard

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/stats"
)

type countingSource struct {
	calls atomic.Int32
	err   error
	gate  chan struct{}
}

func (s *countingSource) Snapshot(context.Context, stats.Query) (Snapshot, error) {
	s.calls.Add(1)
	if s.gate != nil {
		<-s.gate
	}
	return Snapshot{OpenAlertCount: 7}, s.err
}

func (*countingSource) Series(context.Context, stats.Query, bool, int) ([]stats.Series, error) {
	return nil, nil
}

func TestCachedSharesConcurrentAndRepeatedRequests(t *testing.T) {
	src := &countingSource{gate: make(chan struct{})}
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	c := &Cached{Source: src, TTL: 5 * time.Second, Now: func() time.Time { return now }}
	q := stats.Query{From: now.Add(-time.Hour), To: now}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s, err := c.Snapshot(t.Context(), q); err != nil || s.OpenAlertCount != 7 {
				t.Error("bad snapshot", err)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(src.gate)
	wg.Wait()
	if _, err := c.Snapshot(t.Context(), q); err != nil {
		t.Fatal(err)
	}
	if n := src.calls.Load(); n != 1 {
		t.Fatalf("computed %d times, want 1", n)
	}
	now = now.Add(6 * time.Second)
	if _, err := c.Snapshot(t.Context(), stats.Query{From: now.Add(-time.Hour), To: now}); err != nil {
		t.Fatal(err)
	}
	if n := src.calls.Load(); n != 2 {
		t.Fatalf("expected a refresh after the TTL, got %d", n)
	}
}

func TestCachedDoesNotKeepFailures(t *testing.T) {
	src := &countingSource{err: errors.New("db down")}
	c := &Cached{Source: src, TTL: time.Minute}
	q := stats.Query{}
	for range 2 {
		if _, err := c.Snapshot(t.Context(), q); err == nil {
			t.Fatal("want error")
		}
	}
	if src.calls.Load() != 2 {
		t.Fatal("failures must not be cached")
	}
}

func TestCachedWithoutTTLPassesThrough(t *testing.T) {
	src := &countingSource{}
	c := &Cached{Source: src}
	for range 2 {
		_, _ = c.Snapshot(t.Context(), stats.Query{})
	}
	if src.calls.Load() != 2 {
		t.Fatal("no caching expected")
	}
}

type seriesSource struct {
	countingSource
	ctx     context.Context
	query   stats.Query
	percent bool
	limit   int
}

func (s *seriesSource) Series(ctx context.Context, q stats.Query, percent bool, limit int) ([]stats.Series, error) {
	s.ctx, s.query, s.percent, s.limit = ctx, q, percent, limit
	return []stats.Series{{Name: "fixture series"}}, s.err
}
func TestCachedSeriesPassesThrough(t *testing.T) {
	expected := errors.New("series unavailable")
	src := &seriesSource{countingSource: countingSource{err: expected}}
	c := &Cached{Source: src, TTL: time.Hour}
	q := stats.Query{Timezone: "UTC", Bucket: "hour"}
	ctx := t.Context()
	got, err := c.Series(ctx, q, true, 17)
	if !errors.Is(err, expected) || len(got) != 1 || got[0].Name != "fixture series" || src.ctx != ctx || src.query.Timezone != q.Timezone || src.query.Bucket != q.Bucket || !src.percent || src.limit != 17 {
		t.Fatalf("series forwarding result=%v err=%v source=%+v", got, err, src)
	}
}

func TestCachedCancelledWaiterDoesNotCancelComputation(t *testing.T) {
	src := &countingSource{gate: make(chan struct{})}
	c := &Cached{Source: src, TTL: time.Minute}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.Snapshot(ctx, stats.Query{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter: %v", err)
	}
	close(src.gate)
	got, err := c.Snapshot(t.Context(), stats.Query{})
	if err != nil || got.OpenAlertCount != 7 || src.calls.Load() != 1 {
		t.Fatalf("shared computation: %+v %v calls=%d", got, err, src.calls.Load())
	}
}

func TestCachePrunesExpiredWindowsAndRefreshesSameKey(t *testing.T) {
	now := time.Now()
	src := &countingSource{}
	c := &Cached{Source: src, TTL: time.Minute, Now: func() time.Time { return now }, entries: map[string]*entry{}}
	for i := range 65 {
		c.entries[time.Unix(int64(i), 0).String()] = &entry{expires: now.Add(-time.Second)}
	}
	q := stats.Query{}
	if _, err := c.Snapshot(t.Context(), q); err != nil {
		t.Fatal(err)
	}
	if len(c.entries) != 1 {
		t.Fatalf("expired entries retained: %d", len(c.entries))
	}
	now = now.Add(2 * time.Minute)
	if _, err := c.Snapshot(t.Context(), q); err != nil {
		t.Fatal(err)
	}
	if src.calls.Load() != 2 {
		t.Fatal("same-key expiration did not refresh")
	}
}

func TestCacheKeysSeparateEngines(t *testing.T) {
	c := &Cached{TTL: time.Minute}
	q := stats.Query{}
	primary := c.key(q)
	q.Engine = "ookla"
	ookla := c.key(q)
	q.Engine = "librespeed"
	if libre := c.key(q); libre == primary || libre == ookla || primary == ookla {
		t.Fatal("engine filters share a cache key")
	}
}
