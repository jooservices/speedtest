// Package control runs the agent's control loop: a heartbeat every interval,
// conditional configuration sync (ETag) applied to the scheduler without
// restart, and polling/acknowledging control commands. When the center is
// unreachable the loop keeps the last known schedule and only logs.
package control

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jooservices/speedtest/internal/agent/sched"
)

// HeartbeatInterval is how often the agent heartbeats, syncs its configuration
// and polls commands.
const HeartbeatInterval = 30 * time.Second

// Scheduler is the control loop's view of the local scheduler. It applies new
// schedules without restart and exposes the last test time for heartbeats.
type Scheduler interface {
	SetSchedule(sched.Schedule)
	Pause()
	Resume()
	RunNow()
	LastRun() time.Time
}

// Controller drives the client and scheduler on a fixed cadence.
type Controller struct {
	Client    Client
	Scheduler Scheduler
	Logger    *slog.Logger

	// ApplyConfig atomically applies measurement settings and schedule when composed.
	ApplyConfig func(AgentConfig, sched.Schedule) error
	// RunCommand returns current asynchronous run_now progress on redelivery.
	RunCommand func(context.Context, Command) CommandAck
	interval   time.Duration
	now        func() time.Time
	startedAt  time.Time

	etag     string
	commands *commandStore
}

// Option customizes a Controller created by New.
type Option func(*Controller)

// WithInterval replaces the heartbeat/poll cadence, for tests.
func WithInterval(d time.Duration) Option { return func(c *Controller) { c.interval = d } }

// WithNow replaces the uptime clock, for tests.
func WithNow(fn func() time.Time) Option { return func(c *Controller) { c.now = fn } }

// WithLogger replaces the default logger.
func WithLogger(l *slog.Logger) Option { return func(c *Controller) { c.Logger = l } }

// New builds a Controller. The client carries the center address, credential
// and agent identity; the scheduler receives configuration and commands.
func New(client Client, scheduler Scheduler, opts ...Option) *Controller {
	c := &Controller{
		Client:    client,
		Scheduler: scheduler,
		interval:  HeartbeatInterval,
		now:       time.Now,
		commands:  newCommandStore(),
	}
	for _, o := range opts {
		o(c)
	}
	c.startedAt = c.now()
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// Run executes one control pass immediately, then repeats it every interval
// until ctx is cancelled.
func (c *Controller) Run(ctx context.Context) error {
	// Slow config requests or acknowledgement retries must not hold up liveness.
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(c.interval)
		defer t.Stop()
		for {
			c.syncConfig(ctx)
			c.pollCommands(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		c.heartbeat(ctx)
		select {
		case <-ctx.Done():
			<-done
			return nil
		case <-t.C:
		}
	}
}

// Once performs a single control pass: heartbeat, configuration sync and
// command polling. Individual failures are logged, never fatal, so the loop
// keeps the last known schedule when the center is unreachable.
func (c *Controller) Once(ctx context.Context) {
	c.heartbeat(ctx)
	c.syncConfig(ctx)
	c.pollCommands(ctx)
}

func (c *Controller) heartbeat(ctx context.Context) {
	uptime := int64(c.now().Sub(c.startedAt).Seconds())
	var lastTest *string
	if t := c.Scheduler.LastRun(); !t.IsZero() {
		s := t.UTC().Format(time.RFC3339)
		lastTest = &s
	}
	err := c.Client.Heartbeat(ctx, Heartbeat{
		Agent:    c.Client.Agent,
		Uptime:   uptime,
		LastTest: lastTest,
	})
	if err != nil && ctx.Err() == nil {
		c.Logger.Warn("heartbeat failed", "error", err.Error())
	}
}

func (c *Controller) syncConfig(ctx context.Context) {
	cfg, etag, notModified, err := c.Client.Config(ctx, c.etag)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		c.Logger.Warn("config sync failed; keeping last known schedule", "error", err.Error())
		return
	}
	if notModified {
		return
	}
	sch, err := cfg.schedule()
	if err != nil {
		c.Logger.Error("invalid schedule from center; keeping last known schedule", "error", err.Error())
		return
	}
	if c.ApplyConfig != nil {
		if err := c.ApplyConfig(cfg, sch); err != nil {
			c.Logger.Error("invalid effective configuration; retaining previous configuration")
			return
		}
	} else {
		c.Scheduler.SetSchedule(sch)
	}
	c.etag = etag
}

func (c *Controller) pollCommands(ctx context.Context) {
	cmds, err := c.Client.PollCommands(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		c.Logger.Warn("command poll failed", "error", err.Error())
		return
	}
	for i := range cmds {
		c.handleCommand(ctx, cmds[i])
	}
}

// handleCommand executes a command exactly once and acknowledges it terminally.
// Redelivered commands whose id is already known are not re-executed, only
// re-acknowledged.
func (c *Controller) handleCommand(ctx context.Context, cmd Command) {
	if cmd.Type == "run_now" && c.RunCommand != nil {
		ack := c.RunCommand(ctx, cmd)
		if err := c.Client.Ack(ctx, cmd.ID, ack); err != nil {
			c.Logger.Warn("command ack failed", "id", cmd.ID)
		}
		return
	}
	if rec, ok := c.commands.get(cmd.ID); ok {
		if rec.acked {
			return
		}
		if err := c.Client.Ack(ctx, cmd.ID, rec.ack); err != nil {
			c.Logger.Warn("command ack failed", "id", cmd.ID, "error", err.Error())
			return
		}
		c.commands.markAcked(cmd.ID)
		return
	}

	if !c.commands.makeRoom() {
		c.Logger.Warn("command history full of unacknowledged commands; deferring command", "id", cmd.ID)
		return
	}
	ack := c.execute(ctx, cmd)
	c.commands.put(cmd.ID, ack)
	c.Logger.Info("command executed", "id", cmd.ID, "type", cmd.Type, "status", ack.Status)
	if err := c.Client.Ack(ctx, cmd.ID, ack); err != nil {
		c.Logger.Warn("command ack failed", "id", cmd.ID, "error", err.Error())
		return
	}
	c.commands.markAcked(cmd.ID)
}

// execute applies a command and returns its terminal acknowledgement.
func (c *Controller) execute(ctx context.Context, cmd Command) CommandAck {
	switch cmd.Type {
	case "run_now":
		c.Scheduler.RunNow()
		return CommandAck{Status: "succeeded", Phase: "done", Pct: 100}
	case "pause":
		c.Scheduler.Pause()
		return CommandAck{Status: "succeeded", Phase: "done", Pct: 100}
	case "resume":
		c.Scheduler.Resume()
		return CommandAck{Status: "succeeded", Phase: "done", Pct: 100}
	case "update_config":
		return c.updateConfig(ctx)
	default:
		return CommandAck{Status: "failed", Error: strPtr("unsupported command: " + cmd.Type)}
	}
}

// updateConfig forces a fresh configuration fetch and applies it.
func (c *Controller) updateConfig(ctx context.Context) CommandAck {
	cfg, etag, _, err := c.Client.Config(ctx, "")
	if err != nil {
		return CommandAck{Status: "failed", Error: strPtr(err.Error())}
	}
	sch, err := cfg.schedule()
	if err != nil {
		return CommandAck{Status: "failed", Error: strPtr(err.Error())}
	}
	if c.ApplyConfig != nil {
		if err := c.ApplyConfig(cfg, sch); err != nil {
			return CommandAck{Status: "failed", Error: strPtr("invalid effective configuration")}
		}
	} else {
		c.Scheduler.SetSchedule(sch)
	}
	c.etag = etag
	return CommandAck{Status: "succeeded", Phase: "done", Pct: 100}
}

// schedule converts the center configuration into the local schedule, honoring
// the interval, offset, enabled flag and quiet hours.
func (c AgentConfig) schedule() (sched.Schedule, error) {
	if c.Schedule.Interval < 5 || c.Schedule.Interval > 1440 {
		return sched.Schedule{}, fmt.Errorf("interval must be between 5 and 1440 minutes")
	}
	if c.Schedule.Offset < 0 || c.Schedule.Offset >= c.Schedule.Interval {
		return sched.Schedule{}, fmt.Errorf("offset must be >= 0 and < interval")
	}
	interval := time.Duration(c.Schedule.Interval) * time.Minute
	offset := time.Duration(c.Schedule.Offset) * time.Minute

	loc := time.UTC
	if c.Quiet.Timezone != "" {
		loaded, err := time.LoadLocation(c.Quiet.Timezone)
		if err != nil {
			return sched.Schedule{}, fmt.Errorf("invalid timezone %q: %w", c.Quiet.Timezone, err)
		}
		loc = loaded
	}

	sch := sched.Schedule{
		Interval: interval,
		Offset:   offset,
		Enabled:  c.Schedule.Enabled,
		Quiet: sched.QuietHours{
			Start: c.Quiet.QuietStart,
			End:   c.Quiet.QuietEnd,
			Loc:   loc,
		},
	}
	if err := sch.Validate(); err != nil {
		return sched.Schedule{}, err
	}
	return sch, nil
}

func strPtr(s string) *string { return &s }

// commandRecord is the acknowledgement to (re)send for a processed command.
type commandRecord struct {
	ack   CommandAck
	acked bool
}

// commandStore bounds processed IDs. Only terminally acknowledged entries may
// be evicted: an acknowledgement failure must never allow run_now to execute
// again. When all entries are unacknowledged, new commands remain at the center.
type commandStore struct {
	mu    sync.Mutex
	order []string
	store map[string]*commandRecord
	max   int
}

func newCommandStore() *commandStore {
	return &commandStore{store: make(map[string]*commandRecord), max: 10000}
}

func (s *commandStore) get(id string) (*commandRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.store[id]
	return r, ok
}

// put records a command only once; a later put for the same id is a no-op, so a
// command is never executed twice even if its acknowledgement failed.
func (s *commandStore) put(id string, ack CommandAck) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.store[id]; ok {
		return
	}
	s.store[id] = &commandRecord{ack: ack}
	s.order = append(s.order, id)

}

func (s *commandStore) markAcked(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.store[id]; ok {
		r.acked = true
	}
}

// makeRoom evicts an acknowledged record if the bounded history is full.
func (s *commandStore) makeRoom() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.order) < s.max {
		return true
	}
	for i, id := range s.order {
		if s.store[id].acked {
			delete(s.store, id)
			s.order = append(s.order[:i], s.order[i+1:]...)
			return true
		}
	}
	return false
}
