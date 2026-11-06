// Package sched runs speed tests on a fixed local schedule. A node keeps only
// its own schedule: an interval, an offset (which keeps nodes from overlapping),
// an enabled flag and optional quiet hours. Tests run one at a time; a test that
// overruns its slot is never stacked, the missed slot is skipped and logged.
package sched

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"
)

// maxJitter bounds the small random delay added to each scheduled run so
// nodes do not all start at the exact same instant.
const maxJitter = 5 * time.Second

// Clock is the time source the scheduler uses to read time and to wait for the
// next slot. SystemClock is the production implementation; tests inject a fake.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// After returns a channel that fires after d.
	After(d time.Duration) <-chan time.Time
}

// SystemClock is the wall clock.
type SystemClock struct{}

func (SystemClock) Now() time.Time                         { return time.Now() }
func (SystemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Runner runs one speed test. Run must return only once the test has finished
// or ctx is cancelled. The scheduler never calls Run concurrently.
type Runner interface {
	Run(context.Context) error
}

// QuietHours is a daily window in which scheduled tests are skipped. Start and
// End are "HH:mm" in the window's own time zone; both empty means no quiet
// window. A window may cross midnight.
type QuietHours struct {
	Start string
	End   string
	Loc   *time.Location
}

// enabled reports whether a quiet window is configured.
func (q QuietHours) enabled() bool { return q.Start != "" && q.End != "" }

// inQuiet reports whether t falls inside the quiet window.
func (q QuietHours) inQuiet(t time.Time) bool {
	if !q.enabled() {
		return false
	}
	loc := q.Loc
	if loc == nil {
		loc = time.UTC
	}
	t = t.In(loc)
	minutes := t.Hour()*60 + t.Minute()
	start, ok1 := parseHM(q.Start)
	end, ok2 := parseHM(q.End)
	if !ok1 || !ok2 || start == end {
		return false
	}
	if start < end {
		return minutes >= start && minutes < end
	}
	// Crossing midnight.
	return minutes >= start || minutes < end
}

// Schedule describes when the node runs tests. Interval and Offset are
// durations of whole minutes; slots fall on the Unix-epoch grid:
// epoch + offset + k*interval.
type Schedule struct {
	Interval time.Duration
	Offset   time.Duration
	Enabled  bool
	Quiet    QuietHours
}

// Validate rejects a schedule the scheduler cannot honour.
func (s Schedule) Validate() error {
	if s.Interval <= 0 {
		return errors.New("interval must be positive")
	}
	if s.Offset < 0 || s.Offset >= s.Interval {
		return errors.New("offset must be >= 0 and < interval")
	}
	if (s.Quiet.Start == "") != (s.Quiet.End == "") {
		return errors.New("quiet start and end must both be set or both empty")
	}
	if s.Quiet.Start != "" {
		start, ok1 := parseHM(s.Quiet.Start)
		end, ok2 := parseHM(s.Quiet.End)
		if !ok1 || !ok2 {
			return fmt.Errorf("quiet hours must be HH:mm, got %q-%q", s.Quiet.Start, s.Quiet.End)
		}
		if start == end {
			return errors.New("quiet start and end must differ")
		}
	}
	return nil
}

// nextSlot returns the smallest slot at or after after.
func nextSlot(after time.Time, offset, interval time.Duration) time.Time {
	epoch := time.Unix(0, 0).Add(offset)
	d := after.Sub(epoch)
	if d <= 0 {
		return epoch
	}
	k := (d + interval - 1) / interval
	return epoch.Add(k * interval)
}

// nextAllowed returns the smallest slot at or after after that is not inside
// the quiet window. It returns the zero time when no slot qualifies.
func (s Schedule) nextAllowed(after time.Time) time.Time {
	if s.Interval <= 0 {
		return time.Time{}
	}
	slot := nextSlot(after, s.Offset, s.Interval)
	if !s.Quiet.enabled() {
		return slot
	}
	for range 1500 {
		if !s.Quiet.inQuiet(slot) {
			return slot
		}
		slot = slot.Add(s.Interval)
	}
	return time.Time{}
}

// Scheduler runs tests on a schedule. It is safe for concurrent use: the
// schedule and enabled flag can be updated while Run is active.
type Scheduler struct {
	runner Runner
	logger *slog.Logger
	clock  Clock
	jitter func(time.Duration) time.Duration

	manualID string
	mu       sync.Mutex
	schedule Schedule
	lastRun  time.Time
	manual   chan string
	wake     chan struct{}
}

// Option customizes a Scheduler created by New.
type Option func(*Scheduler)

// WithClock replaces the time source, for deterministic tests.
func WithClock(c Clock) Option { return func(s *Scheduler) { s.clock = c } }

// WithJitter replaces the jitter source, for deterministic tests.
func WithJitter(fn func(time.Duration) time.Duration) Option {
	return func(s *Scheduler) { s.jitter = fn }
}

// New builds a Scheduler. The initial schedule is disabled and produces no runs
// until SetSchedule is called. A nil logger uses the default logger.
func New(runner Runner, logger *slog.Logger, opts ...Option) *Scheduler {
	s := &Scheduler{
		runner: runner,
		logger: logger,
		clock:  SystemClock{},
		jitter: defaultJitterFunc,
		manual: make(chan string, 100),
		wake:   make(chan struct{}, 1),
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

func defaultJitterFunc(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(max)))
}

// SetSchedule replaces the schedule, applying a new configuration without
// restarting the scheduler.
func (s *Scheduler) SetSchedule(sch Schedule) {
	s.mu.Lock()
	s.schedule = sch
	s.mu.Unlock()
	s.signalWake()
}

// Schedule returns a copy of the current schedule.
func (s *Scheduler) Schedule() Schedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.schedule
}

// Pause disables scheduled tests without discarding the schedule.
func (s *Scheduler) Pause() {
	s.mu.Lock()
	s.schedule.Enabled = false
	s.mu.Unlock()
	s.signalWake()
}

// Resume re-enables scheduled tests.
func (s *Scheduler) Resume() {
	s.mu.Lock()
	s.schedule.Enabled = true
	s.mu.Unlock()
	s.signalWake()
}

// RunNow requests an immediate test. It is a manual override that ignores quiet
// hours and the enabled flag, but it never starts a second test while one is
// running.
func (s *Scheduler) RunNow() {
	select {
	case s.manual <- "":
	default:
	}
}

// LastRun returns the start time of the most recent test, or the zero time when
// no test has run yet.
func (s *Scheduler) LastRun() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastRun
}

// signalWake wakes the run loop without blocking if a wake is already pending.
func (s *Scheduler) signalWake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

type wake int

const (
	wakeTimer wake = iota
	wakeManual
	wakeConfig
	wakeCancel
)

// Run executes the schedule until ctx is cancelled. It returns nil on normal
// shutdown. Tests run one at a time; a test that overruns its slot causes the
// next slot to be skipped and logged rather than stacked.
func (s *Scheduler) Run(ctx context.Context) error {
	var consumed time.Time
	for {
		if ctx.Err() != nil {
			return nil
		}

		// Drain a prior wake before taking the current snapshot.
		sch := s.currentAfterWake()

		if !sch.Enabled {
			if s.waitUnscheduled(ctx) {
				return nil
			}
			continue
		}

		now := s.clock.Now()
		slot := nextUnconsumed(sch, now, consumed)
		if slot.IsZero() {
			if s.waitUnscheduled(ctx) {
				return nil
			}
			continue
		}

		delay := slot.Sub(now)
		if delay > 0 {
			delay += s.jitter(maxJitter)
			switch s.wait(ctx, delay) {
			case wakeCancel:
				return nil
			case wakeManual:
				s.runOne(ctx, s.manualID)
				continue
			case wakeConfig:
				continue
			}
			// wakeTimer: the slot is due; fall through.
		}

		// A timer and a config wake can become ready together. Recheck before
		// running so a paused or replaced schedule cannot fire a stale slot.
		if s.Schedule() != sch {
			continue
		}
		consumed = slot
		actual := s.clock.Now()
		if !actual.Before(slot.Add(sch.Interval)) {
			s.logger.Warn("speed test slot expired; skipped", "slot", slot.UTC().Format(time.RFC3339))
			consumed = actual
			continue
		}
		if sch.Quiet.inQuiet(actual) {
			continue
		}
		s.runScheduled(ctx, slot, sch)
	}
}

// wait blocks until the timer fires, a manual run is requested, the schedule
// changes, or ctx is cancelled. A non-positive duration disables the timer.
func (s *Scheduler) wait(ctx context.Context, d time.Duration) wake {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return wakeCancel
		case id := <-s.manual:
			s.manualID = id
			return wakeManual
		case <-s.wake:
			return wakeConfig
		}
	}
	timer := s.clock.After(d)
	select {
	case <-ctx.Done():
		return wakeCancel
	case id := <-s.manual:
		s.manualID = id
		return wakeManual
	case <-s.wake:
		return wakeConfig
	case <-timer:
		return wakeTimer
	}
}

// runScheduled runs one test and logs any slot skipped because the test
// overran its own slot.
func (s *Scheduler) runScheduled(ctx context.Context, slot time.Time, sch Schedule) {
	s.runOne(ctx, "")
	finish := s.clock.Now()
	next := nextSlot(finish, sch.Offset, sch.Interval)
	skipped := int64(next.Sub(slot)/sch.Interval) - 1
	if skipped > 0 {
		s.logger.Warn("speed test overran its slot; skipped next slot(s)",
			"skipped", skipped,
			"slot", slot.UTC().Format(time.RFC3339),
		)
	}
}

// runOne runs a single test, recording its start time. The run loop calls it
// synchronously, so tests never overlap.
func (s *Scheduler) runOne(ctx context.Context, id string) {
	start := s.clock.Now()
	s.mu.Lock()
	s.lastRun = start
	s.mu.Unlock()

	s.logger.Info("speed test started", "at", start.UTC().Format(time.RFC3339))
	var err error
	if manual, ok := s.runner.(interface {
		RunCommand(context.Context, string) error
	}); ok && id != "" {
		err = manual.RunCommand(ctx, id)
	} else {
		err = s.runner.Run(ctx)
	}
	if err != nil && ctx.Err() == nil {
		s.logger.Warn("speed test failed", "error", err.Error())
	}
	s.logger.Info("speed test finished", "at", s.clock.Now().UTC().Format(time.RFC3339))
}

// parseHM parses an "HH:mm" string into minutes since midnight.
func parseHM(s string) (int, bool) {
	if len(s) != 5 || s[2] != ':' || s[0] < '0' || s[0] > '9' || s[1] < '0' || s[1] > '9' || s[3] < '0' || s[3] > '9' || s[4] < '0' || s[4] > '9' {
		return 0, false
	}
	h, errH := strconv.Atoi(s[:2])
	m, errM := strconv.Atoi(s[3:])
	if errH != nil || errM != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// RequestRun queues a distinct command without coalescing operator requests.
// A full queue leaves the command pending at the center for the next poll.
func (s *Scheduler) RequestRun(id string) bool {
	select {
	case s.manual <- id:
		return true
	default:
		return false
	}
}

func (s *Scheduler) currentAfterWake() Schedule {
	select {
	case <-s.wake:
	default:
	}
	s.mu.Lock()
	sch := s.schedule
	s.mu.Unlock()
	return sch
}

func (s *Scheduler) waitUnscheduled(ctx context.Context) bool {
	switch s.wait(ctx, 0) {
	case wakeCancel:
		return true
	case wakeManual:
		s.runOne(ctx, s.manualID)
	}
	return false
}

func nextUnconsumed(sch Schedule, now, consumed time.Time) time.Time {
	after := now
	if !consumed.IsZero() && !after.After(consumed) {
		after = consumed.Add(time.Nanosecond)
	}
	return sch.nextAllowed(after)
}
