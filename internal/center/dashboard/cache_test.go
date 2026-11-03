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
