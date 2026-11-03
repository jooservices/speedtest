package sched

import (
	"context"
	"testing"
	"time"
)

func TestOffsetJitterAndSlotConsumedOnce(t *testing.T) {
	clock := &fakeClock{now: epoch0000}
	runner := newRecordingRunner(clock, false)
	s := New(runner, nil, WithClock(clock), WithJitter(func(max time.Duration) time.Duration {
		if max != 5*time.Second {
			t.Errorf("jitter bound = %v", max)
		}
		return 2 * time.Second
	}))
	s.SetSchedule(Schedule{Interval: 5 * time.Minute, Offset: time.Minute, Enabled: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Run(ctx) }()
	waitFor(t, func() bool { return clock.pending() > 0 })
	clock.advance(31 * time.Second)
	select {
	case <-runner.started:
		t.Fatal("ran before slot plus jitter")
	default:
	}
	clock.advance(time.Second)
	if at := awaitRun(t, runner.started); !at.Equal(time.Unix(62, 0)) {
		t.Fatal(at)
	}
	waitFor(t, func() bool { return clock.pending() > 0 })
	select {
	case <-runner.started:
		t.Fatal("slot repeated")
	default:
	}
	clock.advance(5 * time.Minute)
	if at := awaitRun(t, runner.started); !at.Equal(time.Unix(362, 0)) {
		t.Fatal(at)
	}
}

func TestExactSlotDoesNotRepeat(t *testing.T) {
	clock := &fakeClock{now: time.Unix(60, 0)}
	runner := newRecordingRunner(clock, false)
	_, _, cancel := startScheduler(t, Schedule{Interval: time.Minute, Enabled: true}, clock, runner)
	defer cancel()
	awaitRun(t, runner.started)
	waitFor(t, func() bool { return clock.pending() > 0 })
	select {
	case <-runner.started:
		t.Fatal("same slot executed twice")
	default:
	}
	clock.advance(time.Minute)
	if at := awaitRun(t, runner.started); !at.Equal(time.Unix(120, 0)) {
		t.Fatal(at)
	}
}

func TestConfigChangeAppliesWithoutRestart(t *testing.T) {
	clock := &fakeClock{now: epoch0000}
	runner := newRecordingRunner(clock, false)
	s, _, cancel := startScheduler(t, Schedule{Interval: time.Hour, Enabled: true}, clock, runner)
	defer cancel()
	waitFor(t, func() bool { return clock.pending() > 0 })
	s.SetSchedule(Schedule{Interval: 5 * time.Minute, Offset: time.Minute, Enabled: true})
	waitFor(t, func() bool { return clock.pending() > 1 })
	clock.advance(30 * time.Second)
	if at := awaitRun(t, runner.started); !at.Equal(time.Unix(60, 0)) {
		t.Fatal(at)
	}
}

func TestExpiredSlotSkipped(t *testing.T) {
	clock := &fakeClock{now: epoch0000}
	runner := newRecordingRunner(clock, false)
	_, logs, cancel := startScheduler(t, Schedule{Interval: time.Minute, Enabled: true}, clock, runner)
	defer cancel()
	waitFor(t, func() bool { return clock.pending() > 0 })
	clock.advance(3 * time.Minute)
	waitFor(t, func() bool { return logs.String() != "" })
	select {
	case <-runner.started:
		t.Fatal("expired slot ran")
	default:
	}
	waitFor(t, func() bool { return clock.pending() > 0 })
	clock.advance(30 * time.Second)
	if at := awaitRun(t, runner.started); !at.Equal(time.Unix(240, 0)) {
		t.Fatal(at)
	}
}

func TestNoAllowedSlotStillAcceptsManualAndCancel(t *testing.T) {
	sch := Schedule{Interval: 24 * time.Hour, Enabled: true, Quiet: QuietHours{Start: "00:00", End: "00:01"}}
	if !sch.nextAllowed(epoch0000).IsZero() {
		t.Fatal("quiet daily slot allowed")
	}
	clock := &fakeClock{now: epoch0000}
	runner := newRecordingRunner(clock, false)
	s, _, cancel := startScheduler(t, sch, clock, runner)
	s.RunNow()
	awaitRun(t, runner.started)
	cancel()
}

func TestSystemClockAndDefaultJitter(t *testing.T) {
	c := SystemClock{}
	before := time.Now()
	if c.Now().Before(before) {
		t.Fatal("clock moved backwards")
	}
	select {
	case <-c.After(time.Nanosecond):
	case <-time.After(time.Second):
		t.Fatal("clock timer failed")
	}
	if defaultJitterFunc(0) != 0 {
		t.Fatal("zero jitter")
	}
	for range 100 {
		if d := defaultJitterFunc(maxJitter); d < 0 || d >= maxJitter {
			t.Fatal(d)
		}
	}
	for _, q := range []QuietHours{{Start: "bad", End: "02:00"}, {Start: "01:00", End: "01:00"}} {
		if q.inQuiet(time.Now()) {
			t.Fatal("invalid quiet hours")
		}
	}
	if _, ok := parseHM("+1:00"); ok {
		t.Fatal("signed hour accepted")
	}
}

type hookClock struct {
	now   time.Time
	after func(time.Duration) <-chan time.Time
}

func (c *hookClock) Now() time.Time                         { return c.now }
func (c *hookClock) After(d time.Duration) <-chan time.Time { return c.after(d) }

func TestWakeRechecksConfigurationAndActualQuietHours(t *testing.T) {
	for _, mode := range []string{"config", "quiet"} {
		t.Run(mode, func(t *testing.T) {
			clock := &hookClock{now: epoch0000}
			runner := newRecordingRunner(&fakeClock{now: epoch0000}, false)
			s := New(runner, nil, WithClock(clock), WithJitter(func(time.Duration) time.Duration { return 0 }))
			s.SetSchedule(Schedule{Interval: 5 * time.Minute, Offset: time.Minute, Enabled: true, Quiet: QuietHours{Start: "00:02", End: "00:04"}})
			secondWait := make(chan struct{})
			calls := 0
			clock.after = func(time.Duration) <-chan time.Time {
				calls++
				if calls == 1 {
					clock.now = time.Unix(180, 0)
					if mode == "config" {
						s.SetSchedule(Schedule{Interval: 10 * time.Minute, Enabled: true})
						<-s.wake // Leave only the timer ready to test the stale snapshot path.
					}
					ch := make(chan time.Time, 1)
					ch <- clock.now
					return ch
				}
				close(secondWait)
				return make(chan time.Time)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- s.Run(ctx) }()
			select {
			case <-secondWait:
			case <-time.After(time.Second):
				t.Fatal("did not skip stale/quiet slot")
			}
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			select {
			case <-runner.started:
				t.Fatal("stale/quiet slot ran")
			default:
			}
		})
	}
}
