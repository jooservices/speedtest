package sched

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"
)

type engineRunner struct {
	clock       *fakeClock
	started     chan string
	release     chan struct{}
	mu          sync.Mutex
	active, max int
}

func (r *engineRunner) Run(ctx context.Context) error { return r.RunEngine(ctx, "manual") }
func (r *engineRunner) RunEngine(ctx context.Context, name string) error {
	r.mu.Lock()
	r.active++
	if r.active > r.max {
		r.max = r.active
	}
	r.mu.Unlock()
	r.started <- name
	select {
	case <-ctx.Done():
	case <-r.release:
	}
	r.mu.Lock()
	r.active--
	r.mu.Unlock()
	return nil
}

func TestEngineSchedulesSerializeCoincidentSlotsAndManualRequests(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	r := &engineRunner{clock: clock, started: make(chan string, 10), release: make(chan struct{}, 10)}
	s := New(r, nil, WithClock(clock), WithJitter(func(time.Duration) time.Duration { return 0 }))
	s.SetEngines([]Schedule{{Engine: "ookla", Interval: 30 * time.Minute, Enabled: true}, {Engine: "librespeed", Interval: 30 * time.Minute, Enabled: true}})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	next := func(want string) {
		t.Helper()
		select {
		case name := <-r.started:
			if name != want {
				t.Fatalf("got %s want %s", name, want)
			}
		case <-time.After(time.Second):
			t.Fatal("engine never started", want)
		}
	}
	next("ookla")
	clock.advance(time.Second)
	r.release <- struct{}{}
	next("librespeed")
	s.RunNow()
	select {
	case name := <-r.started:
		t.Fatal("concurrent run", name)
	case <-time.After(10 * time.Millisecond):
	}
	r.release <- struct{}{}
	next("manual")
	r.release <- struct{}{}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if r.max != 1 {
		t.Fatalf("maximum concurrency=%d", r.max)
	}
}

func TestEngineScheduleSelectionPauseReplaceAndDisabled(t *testing.T) {
	epoch := time.Unix(0, 0)
	clock := &fakeClock{now: epoch}
	s := New(nil, nil, WithClock(clock))
	main := Schedule{Interval: 30 * time.Minute, Enabled: true}
	s.SetSchedule(main)
	consumed := map[string]time.Time{}
	if got := s.nextSchedule(consumed); got != main || !s.hasSchedule(main) {
		t.Fatal(got)
	}
	a := Schedule{Engine: "ookla", Interval: 30 * time.Minute, Enabled: true}
	b := Schedule{Engine: "librespeed", Interval: 10 * time.Minute, Offset: time.Minute, Enabled: true}
	s.SetEngines([]Schedule{a, b})
	if got := s.nextSchedule(consumed); got != a {
		t.Fatal(got)
	}
	consumed[a.Engine] = epoch
	if got := s.nextSchedule(consumed); got != b {
		t.Fatal(got)
	}
	s.Pause()
	if s.nextSchedule(consumed).Enabled || s.hasSchedule(a) {
		t.Fatal("paused schedule active")
	}
	s.Resume()
	if !s.hasSchedule(a) || s.hasSchedule(main) {
		t.Fatal("replacement validation failed")
	}
	if got := s.engineSlot(a, epoch.Add(time.Minute), epoch); got != epoch.Add(30*time.Minute) {
		t.Fatal(got)
	}
	a.Enabled = false
	b.Enabled = false
	s.SetEngines([]Schedule{a, b})
	if s.nextSchedule(consumed).Enabled {
		t.Fatal("disabled schedule selected")
	}
	s.SetEngines(nil)
	if !reflect.DeepEqual(s.nextSchedule(consumed), main) {
		t.Fatal("legacy fallback")
	}
}
