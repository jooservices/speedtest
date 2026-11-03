package sched

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		sched   Schedule
		wantErr string
	}{
		{name: "ok", sched: Schedule{Interval: time.Minute, Enabled: true}},
		{name: "offset within interval", sched: Schedule{Interval: 30 * time.Minute, Offset: 29 * time.Minute}},
		{name: "zero interval", sched: Schedule{Offset: 0}, wantErr: "interval must be positive"},
		{name: "negative interval", sched: Schedule{Interval: -time.Minute}, wantErr: "interval must be positive"},
		{name: "negative offset", sched: Schedule{Interval: time.Minute, Offset: -time.Second}, wantErr: "offset must be >= 0 and < interval"},
		{name: "offset equals interval", sched: Schedule{Interval: time.Minute, Offset: time.Minute}, wantErr: "offset must be >= 0 and < interval"},
		{
			name:    "quiet only start",
			sched:   Schedule{Interval: time.Minute, Quiet: QuietHours{Start: "01:00"}},
			wantErr: "quiet start and end must both be set or both empty",
		},
		{
			name:    "quiet only end",
			sched:   Schedule{Interval: time.Minute, Quiet: QuietHours{End: "01:00"}},
			wantErr: "quiet start and end must both be set or both empty",
		},
		{
			name:    "quiet equal endpoints",
			sched:   Schedule{Interval: time.Minute, Quiet: QuietHours{Start: "01:00", End: "01:00"}},
			wantErr: "quiet start and end must differ",
		},
		{
			name:    "quiet malformed",
			sched:   Schedule{Interval: time.Minute, Quiet: QuietHours{Start: "1:00", End: "02:00"}},
			wantErr: "quiet hours must be HH:mm",
		},
		{name: "quiet crossing midnight", sched: Schedule{Interval: time.Minute, Quiet: QuietHours{Start: "23:00", End: "01:00"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.sched.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestNextSlot(t *testing.T) {
	epoch := time.Unix(0, 0)
	offset := 7 * time.Minute
	interval := 30 * time.Minute

	// Before the first slot, the first slot is epoch + offset.
	if got := nextSlot(epoch.Add(offset-time.Minute), offset, interval); !got.Equal(epoch.Add(offset)) {
		t.Fatalf("nextSlot(before first) = %v, want %v", got, epoch.Add(offset))
	}
	// Exactly on a slot returns that slot.
	if got := nextSlot(epoch.Add(offset), offset, interval); !got.Equal(epoch.Add(offset)) {
		t.Fatalf("nextSlot(on slot) = %v, want %v", got, epoch.Add(offset))
	}
	// Just after a slot returns the next slot.
	if got := nextSlot(epoch.Add(offset+time.Second), offset, interval); !got.Equal(epoch.Add(offset + interval)) {
		t.Fatalf("nextSlot(after slot) = %v, want %v", got, epoch.Add(offset+interval))
	}
	// Far in the future honours k*interval.
	want := epoch.Add(offset + 4*interval)
	if got := nextSlot(epoch.Add(offset+3*interval+time.Second), offset, interval); !got.Equal(want) {
		t.Fatalf("nextSlot(far) = %v, want %v", got, want)
	}
}

func TestQuietHoursInQuiet(t *testing.T) {
	utc := time.UTC
	loc := time.FixedZone("UTC+7", 7*60*60)

	tests := []struct {
		name string
		q    QuietHours
		in   string // RFC3339 UTC
		want bool
	}{
		{name: "no window", q: QuietHours{}, in: "2026-11-03T12:00:00Z", want: false},
		{name: "inside", q: QuietHours{Start: "01:00", End: "02:00", Loc: utc}, in: "2026-11-03T01:30:00Z", want: true},
		{name: "before", q: QuietHours{Start: "01:00", End: "02:00", Loc: utc}, in: "2026-11-03T00:30:00Z", want: false},
		{name: "after", q: QuietHours{Start: "01:00", End: "02:00", Loc: utc}, in: "2026-11-03T02:30:00Z", want: false},
		{name: "start boundary included", q: QuietHours{Start: "01:00", End: "02:00", Loc: utc}, in: "2026-11-03T01:00:00Z", want: true},
		{name: "end boundary excluded", q: QuietHours{Start: "01:00", End: "02:00", Loc: utc}, in: "2026-11-03T02:00:00Z", want: false},
		{name: "crossing midnight before", q: QuietHours{Start: "23:00", End: "01:00", Loc: utc}, in: "2026-11-03T23:30:00Z", want: true},
		{name: "crossing midnight after", q: QuietHours{Start: "23:00", End: "01:00", Loc: utc}, in: "2026-11-03T00:30:00Z", want: true},
		{name: "crossing midnight day", q: QuietHours{Start: "23:00", End: "01:00", Loc: utc}, in: "2026-11-03T12:00:00Z", want: false},
		{name: "local timezone", q: QuietHours{Start: "01:00", End: "02:00", Loc: loc}, in: "2026-11-02T18:30:00Z", want: true},
		{name: "local timezone outside", q: QuietHours{Start: "01:00", End: "02:00", Loc: loc}, in: "2026-11-02T19:30:00Z", want: false},
		{name: "nil location defaults to utc", q: QuietHours{Start: "01:00", End: "02:00"}, in: "2026-11-03T01:30:00Z", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			at, err := time.Parse(time.RFC3339, tt.in)
			if err != nil {
				t.Fatalf("parse time: %v", err)
			}
			if got := tt.q.inQuiet(at); got != tt.want {
				t.Fatalf("inQuiet(%s) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestNextAllowedSkipsQuiet(t *testing.T) {
	epoch := time.Unix(0, 0)
	// A 30-minute interval, no offset; quiet from 00:00 to 00:10 each day.
	sched := Schedule{
		Interval: 30 * time.Minute,
		Quiet:    QuietHours{Start: "00:00", End: "00:10", Loc: time.UTC},
	}

	// A slot at midnight is in quiet; the next allowed slot is 00:30.
	got := sched.nextAllowed(epoch.Add(-time.Minute))
	if want := epoch.Add(30 * time.Minute); !got.Equal(want) {
		t.Fatalf("nextAllowed = %v, want %v", got, want)
	}
	// A time at 00:05 is in quiet; next allowed slot is 00:30.
	got = sched.nextAllowed(epoch.Add(5 * time.Minute))
	if want := epoch.Add(30 * time.Minute); !got.Equal(want) {
		t.Fatalf("nextAllowed = %v, want %v", got, want)
	}
	// A time at 00:20 (outside quiet) yields the 00:30 slot.
	got = sched.nextAllowed(epoch.Add(20 * time.Minute))
	if want := epoch.Add(30 * time.Minute); !got.Equal(want) {
		t.Fatalf("nextAllowed = %v, want %v", got, want)
	}
}

func TestNextAllowedZeroInterval(t *testing.T) {
	var sched Schedule
	if got := sched.nextAllowed(time.Now()); !got.IsZero() {
		t.Fatalf("nextAllowed with zero interval = %v, want zero", got)
	}
}

func TestParseHM(t *testing.T) {
	tests := []struct {
		in     string
		want   int
		wantOK bool
	}{
		{"00:00", 0, true},
		{"00:01", 1, true},
		{"23:59", 23*60 + 59, true},
		{"12:30", 12*60 + 30, true},
		{"", 0, false},
		{"1:00", 0, false},
		{"24:00", 0, false},
		{"12:60", 0, false},
		{"ab:cd", 0, false},
		{"1200", 0, false},
	}
	for _, tt := range tests {
		got, ok := parseHM(tt.in)
		if ok != tt.wantOK || got != tt.want {
			t.Fatalf("parseHM(%q) = %d, %v; want %d, %v", tt.in, got, ok, tt.want, tt.wantOK)
		}
	}
}

// fakeClock is a deterministic clock: Now returns the current fake time and
// After registers a timer that fires once the fake time passes its deadline.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	c    chan time.Time
	at   time.Time
	once sync.Once
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.timers = append(c.timers, &fakeTimer{c: ch, at: c.now.Add(d)})
	return ch
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	c.fireLocked()
}

func (c *fakeClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

func (c *fakeClock) fireLocked() {
	remaining := c.timers[:0]
	for _, t := range c.timers {
		if !t.at.After(c.now) {
			t.once.Do(func() { t.c <- c.now })
		} else {
			remaining = append(remaining, t)
		}
	}
	c.timers = remaining
}

// recordingRunner reports run start times for synchronization and can block
// until released. It tracks the maximum number of concurrent runs.
type recordingRunner struct {
	clock   *fakeClock
	started chan time.Time
	release chan struct{}
	block   bool

	mu      sync.Mutex
	maxConc int
	current int
}

func newRecordingRunner(clock *fakeClock, block bool) *recordingRunner {
	return &recordingRunner{
		clock:   clock,
		started: make(chan time.Time, 64),
		release: make(chan struct{}),
		block:   block,
	}
}

func (r *recordingRunner) Run(ctx context.Context) error {
	r.mu.Lock()
	r.current++
	if r.current > r.maxConc {
		r.maxConc = r.current
	}
	r.mu.Unlock()

	r.started <- r.clock.Now()

	if r.block {
		select {
		case <-r.release:
		case <-ctx.Done():
		}
	}

	r.mu.Lock()
	r.current--
	r.mu.Unlock()
	return nil
}

func (r *recordingRunner) releaseOne() { r.release <- struct{}{} }

func (r *recordingRunner) concurrentMax() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.maxConc
}

// waitFor polls cond until it holds or the deadline elapses.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for condition")
}

func awaitRun(t *testing.T, started <-chan time.Time) time.Time {
	t.Helper()
	select {
	case at := <-started:
		return at
	case <-time.After(5 * time.Second):
		t.Fatal("test did not run")
		return time.Time{}
	}
}

// syncBuffer is a mutex-guarded buffer so the scheduler goroutine and the test
// can share log output safely.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// startScheduler wires a scheduler to a fake clock and deterministic jitter and
// starts its run loop. The returned cancel stops the loop.
func startScheduler(t *testing.T, sch Schedule, clock *fakeClock, runner Runner) (*Scheduler, *syncBuffer, context.CancelFunc) {
	t.Helper()
	buf := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(buf, nil))
	s := New(runner, logger, WithClock(clock), WithJitter(func(time.Duration) time.Duration { return 0 }))
	s.SetSchedule(sch)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Run(ctx) }()
	return s, buf, cancel
}

// epoch0000 is a fixed wall time 30 seconds past the Unix epoch, chosen so that
// no test schedule starts exactly on a slot boundary.
var epoch0000 = time.Unix(30, 0)

func TestRunsOnSchedule(t *testing.T) {
	clock := &fakeClock{now: epoch0000}
	runner := newRecordingRunner(clock, false)
	sch := Schedule{Interval: time.Minute, Offset: 0, Enabled: true}
	_, _, cancel := startScheduler(t, sch, clock, runner)
	defer cancel()

	waitFor(t, func() bool { return clock.pending() > 0 })
	clock.advance(30 * time.Second) // now == 00:01:00

	if got := awaitRun(t, runner.started); !got.Equal(time.Unix(60, 0)) {
		t.Fatalf("first run at %v, want 00:01:00", got)
	}
}

func TestDisabledScheduleDoesNotRun(t *testing.T) {
	clock := &fakeClock{now: epoch0000}
	runner := newRecordingRunner(clock, false)
	sch := Schedule{Interval: time.Minute, Enabled: false}
	_, _, cancel := startScheduler(t, sch, clock, runner)
	defer cancel()

	waitFor(t, func() bool { return clock.pending() == 0 })
	clock.advance(10 * time.Minute)
	time.Sleep(20 * time.Millisecond)
	select {
	case <-runner.started:
		t.Fatal("test ran despite disabled schedule")
	default:
	}
}

func TestQuietHoursSkipped(t *testing.T) {
	clock := &fakeClock{now: epoch0000}
	runner := newRecordingRunner(clock, false)
	// Quiet 00:00-00:10 with a 5-minute interval: the 00:00 and 00:05 slots are
	// quiet, so the first run is at 00:10.
	sch := Schedule{Interval: 5 * time.Minute, Enabled: true, Quiet: QuietHours{Start: "00:00", End: "00:10", Loc: time.UTC}}
	_, _, cancel := startScheduler(t, sch, clock, runner)
	defer cancel()

	waitFor(t, func() bool { return clock.pending() > 0 })
	clock.advance(9*time.Minute + 30*time.Second) // now == 00:10:00

	if got := awaitRun(t, runner.started); !got.Equal(time.Unix(600, 0)) {
		t.Fatalf("first run at %v, want 00:10:00", got)
	}
}

func TestNeverTwoTestsAtOnce(t *testing.T) {
	clock := &fakeClock{now: epoch0000}
	runner := newRecordingRunner(clock, true)
	sch := Schedule{Interval: time.Minute, Enabled: true}
	s, _, cancel := startScheduler(t, sch, clock, runner)
	defer cancel()

	waitFor(t, func() bool { return clock.pending() > 0 })
	clock.advance(30 * time.Second) // first run starts at 00:01:00 and blocks
	awaitRun(t, runner.started)

	// A manual run request must not start a second test while one is running.
	s.RunNow()
	time.Sleep(20 * time.Millisecond)
	if got := runner.concurrentMax(); got != 1 {
		t.Fatalf("max concurrent tests = %d, want 1", got)
	}

	runner.releaseOne()
	time.Sleep(20 * time.Millisecond)
	if got := runner.concurrentMax(); got != 1 {
		t.Fatalf("max concurrent tests after release = %d, want 1", got)
	}
}

func TestOverrunSkipsNextSlotAndLogs(t *testing.T) {
	clock := &fakeClock{now: epoch0000}
	runner := newRecordingRunner(clock, true)
	sch := Schedule{Interval: time.Minute, Enabled: true}
	_, buf, cancel := startScheduler(t, sch, clock, runner)
	defer cancel()

	waitFor(t, func() bool { return clock.pending() > 0 })
	clock.advance(30 * time.Second) // first run starts at 00:01:00 and blocks
	awaitRun(t, runner.started)

	// The test overruns its slot (00:01) into 00:03.
	clock.advance(2 * time.Minute)
	runner.releaseOne()

	waitFor(t, func() bool { return strings.Contains(buf.String(), "skipped next slot(s)") })
	if !strings.Contains(buf.String(), `"skipped":1`) {
		t.Fatalf("log = %q, want skipped:1", buf.String())
	}

	// The next run is at 00:03:00; the 00:02:00 slot was skipped.
	if got := awaitRun(t, runner.started); !got.Equal(time.Unix(180, 0)) {
		t.Fatalf("next run at %v, want 00:03:00", got)
	}
}

func TestRunNowRunsImmediately(t *testing.T) {
	clock := &fakeClock{now: epoch0000}
	runner := newRecordingRunner(clock, false)
	// Next slot is an hour away; a manual run must not wait for it.
	sch := Schedule{Interval: 24 * time.Hour, Offset: time.Hour, Enabled: true}
	s, _, cancel := startScheduler(t, sch, clock, runner)
	defer cancel()

	waitFor(t, func() bool { return clock.pending() > 0 })

	s.RunNow()
	if got := awaitRun(t, runner.started); !got.Equal(epoch0000) {
		t.Fatalf("manual run at %v, want %v", got, epoch0000)
	}
	if got := s.LastRun(); !got.Equal(epoch0000) {
		t.Fatalf("LastRun = %v, want %v", got, epoch0000)
	}
}

func TestRunNowWhileDisabled(t *testing.T) {
	clock := &fakeClock{now: epoch0000}
	runner := newRecordingRunner(clock, false)
	sch := Schedule{Interval: time.Minute, Enabled: false}
	s, _, cancel := startScheduler(t, sch, clock, runner)
	defer cancel()

	time.Sleep(10 * time.Millisecond)
	s.RunNow()
	if got := awaitRun(t, runner.started); !got.Equal(epoch0000) {
		t.Fatalf("manual run at %v, want %v", got, epoch0000)
	}
}

func TestPauseAndResume(t *testing.T) {
	clock := &fakeClock{now: epoch0000}
	runner := newRecordingRunner(clock, false)
	sch := Schedule{Interval: time.Minute, Enabled: true}
	s, _, cancel := startScheduler(t, sch, clock, runner)
	defer cancel()

	waitFor(t, func() bool { return clock.pending() > 0 })

	s.Pause()
	if s.Schedule().Enabled {
		t.Fatal("schedule still enabled after Pause")
	}
	clock.advance(3 * time.Minute)
	time.Sleep(20 * time.Millisecond)
	select {
	case <-runner.started:
		t.Fatal("test ran while paused")
	default:
	}

	s.Resume()
	if !s.Schedule().Enabled {
		t.Fatal("schedule not enabled after Resume")
	}
	waitFor(t, func() bool { return clock.pending() > 0 })
	clock.advance(30 * time.Second) // next slot after 00:03:30 is 00:04:00

	if got := awaitRun(t, runner.started); !got.Equal(time.Unix(240, 0)) {
		t.Fatalf("resumed run at %v, want 00:04:00", got)
	}
}

func TestLastRunInitiallyZero(t *testing.T) {
	s := New(nil, nil)
	if !s.LastRun().IsZero() {
		t.Fatal("LastRun not zero before any test")
	}
	if s.Schedule().Enabled {
		t.Fatal("initial schedule should be disabled until configured")
	}
}

func TestRunnerErrorIsNotFatal(t *testing.T) {
	clock := &fakeClock{now: epoch0000}
	runner := &errorRunner{started: make(chan struct{}, 1)}
	_, buf, cancel := startScheduler(t, Schedule{Interval: time.Minute, Enabled: true}, clock, runner)
	defer cancel()

	waitFor(t, func() bool { return clock.pending() > 0 })
	clock.advance(30 * time.Second)
	select {
	case <-runner.started:
	case <-time.After(5 * time.Second):
		t.Fatal("test did not start")
	}
	waitFor(t, func() bool { return strings.Contains(buf.String(), "speed test failed") })
}

type errorRunner struct {
	started chan struct{}
}

func (e *errorRunner) Run(_ context.Context) error {
	e.started <- struct{}{}
	return errors.New("boom")
}
