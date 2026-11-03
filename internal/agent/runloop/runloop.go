// Package runloop composes measurement, scheduling, control and durable delivery.
package runloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sync"

	agentconfig "github.com/jooservices/speedtest/internal/agent/config"
	"github.com/jooservices/speedtest/internal/agent/control"
	"github.com/jooservices/speedtest/internal/agent/engine"
	"github.com/jooservices/speedtest/internal/agent/netpath"
	"github.com/jooservices/speedtest/internal/agent/report"
	"github.com/jooservices/speedtest/internal/agent/sched"
)

type measurementConfig struct {
	engine  engine.Engine
	binding netpath.Binding
	server  int64
	threads int
	name    string
}

type commandState struct {
	Ack    control.CommandAck `json:"ack"`
	Result string             `json:"result"`
}

// Loop owns one spool and the three workers. Close only after Run returns.
type Loop struct {
	Scheduler  *sched.Scheduler
	Controller *control.Controller
	spool      *report.Spool
	reporter   *report.Reporter
	logger     *slog.Logger
	center     string
	credential string
	mu         sync.Mutex
	cfg        measurementConfig
	commands   map[string]commandState
	notify     chan struct{}
	fatal      chan error
}

// New validates bootstrap configuration before opening durable storage.
func New(cfg agentconfig.Config, center *url.URL, credential, version string, logger *slog.Logger, schedulerOptions ...sched.Option) (*Loop, error) {
	if logger == nil {
		logger = slog.Default()
	}
	runner, err := cfg.NewEngine()
	if err != nil {
		return nil, err
	}
	if center == nil {
		return nil, errors.New("center URL is required")
	}
	binding := netpath.Binding{Interface: cfg.NetworkInterface, SourceIP: cfg.SourceIP}
	if _, err = netpath.Resolve(binding); err != nil {
		return nil, err
	}
	if _, err = netpath.NewRunner(runner, center.String(), credential, binding); err != nil {
		return nil, err
	}
	sc, err := report.SpoolConfigFromEnv()
	if err != nil {
		return nil, err
	}
	spool, err := report.Open(sc, logger)
	if err != nil {
		return nil, err
	}
	threads := cfg.ParallelConnections
	if threads == 0 {
		threads = report.DefaultThreads
		if cfg.Engine == "librespeed" {
			ec, _ := engine.LibreSpeedConfigFromEnv()
			threads = ec.Concurrent
		}
	}
	l := &Loop{spool: spool, logger: logger, center: center.String(), credential: credential,
		cfg: measurementConfig{runner, binding, cfg.ServerID, threads, cfg.Engine}, commands: make(map[string]commandState), notify: make(chan struct{}, 1), fatal: make(chan error, 1)}
	saved, err := spool.Commands()
	if err != nil {
		_ = spool.Close()
		return nil, err
	}
	for id, data := range saved {
		var state commandState
		if json.Unmarshal(data, &state) != nil {
			_ = spool.Close()
			return nil, errors.New("invalid command journal")
		}
		if state.Result != "" || state.Ack.Status != "running" {
			l.commands[id] = state
		}
	}
	l.reporter = report.NewReporter(spool, &report.Client{BaseURL: center, Credential: credential}, report.WithLogger(logger), report.WithDeliveryObserver(l.delivered))
	l.Scheduler = sched.New(l, logger, schedulerOptions...)
	l.Scheduler.SetSchedule(sched.Schedule{Interval: cfg.Interval, Enabled: true})
	l.Controller = control.New(control.Client{BaseURL: center, Credential: credential, Agent: "speedtest-agent/" + version}, l.Scheduler, control.WithLogger(logger))
	l.Controller.ApplyConfig = l.applyConfig
	l.Controller.RunCommand = l.command
	return l, nil
}

// Close releases the spool after all workers have joined.
func (l *Loop) Close() error { return l.spool.Close() }

// Serve runs the workers, cancels siblings on fatal storage/protocol errors,
// and joins every worker before returning. Transient delivery retries are owned
// by Reporter; cancellation leaves committed results for the next startup.
func (l *Loop) Serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); _ = l.Scheduler.Run(ctx) }()
	go func() { defer wg.Done(); _ = l.Controller.Run(ctx) }()
	go func() { defer wg.Done(); l.deliver(ctx) }()
	var err error
	select {
	case <-ctx.Done():
	case err = <-l.fatal:
		cancel()
	}
	wg.Wait()
	if err == nil {
		select {
		case err = <-l.fatal:
		default:
		}
	}
	return err
}

func (l *Loop) fail(err error) {
	select {
	case l.fatal <- err:
	default:
	}
}
func (l *Loop) wake() {
	select {
	case l.notify <- struct{}{}:
	default:
	}
}

func (l *Loop) deliver(ctx context.Context) {
	for {
		if err := l.reporter.Flush(ctx); err != nil {
			if ctx.Err() == nil {
				l.fail(errors.New("result delivery stopped; pending results retained"))
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-l.notify:
		}
	}
}

// Run executes one scheduled measurement.
func (l *Loop) Run(ctx context.Context) error { return l.measure(ctx, "") }

// RunCommand executes the scheduler's distinct manual request.
func (l *Loop) RunCommand(ctx context.Context, id string) error { return l.measure(ctx, id) }

func (l *Loop) measure(ctx context.Context, id string) error {
	l.mu.Lock()
	cfg := l.cfg
	l.progress(id, "testing", 20)
	l.mu.Unlock()
	runner, err := netpath.NewRunner(cfg.engine, l.center, l.credential, cfg.binding)
	if err != nil {
		l.fail(errors.New("invalid measurement coordinator"))
		return err
	}
	out, runErr := runner.Run(ctx, engine.Options{ServerID: cfg.server})
	if runErr != nil && out.Measurement.Error == "" {
		out.Measurement.Error = "speedtest: network path unavailable"
	}
	if out.PublicIP == "" && out.Measurement.ExternalIP == "" && out.Measurement.Error == "" {
		out.Measurement.Error = "speedtest: public_ip_unavailable"
		runErr = errors.New(out.Measurement.Error)
	}
	res, err := report.FromEngine(out.Measurement, report.Options{IP: out.PublicIP, Threads: cfg.threads, Manual: id != ""})
	if err != nil {
		l.fail(errors.New("measurement could not be represented by reporting contract"))
		return err
	}
	res.Warnings = out.Warnings
	// Enqueue is independent of cancellation: an interrupted test is still durable.
	l.mu.Lock()
	var state commandState
	if id != "" {
		state = l.commands[id]
		state.Result = res.ID
		state.Ack = control.CommandAck{Status: "running", Phase: "reporting", Pct: 80}
	}
	data, _ := json.Marshal(state)
	err = l.spool.AppendWithState(res, id, data)
	if err != nil {
		l.mu.Unlock()
		l.fail(errors.New("result spool write failed"))
		return errors.New("result spool write failed")
	}
	if id != "" {
		l.commands[id] = state
	}
	l.mu.Unlock()
	l.logger.Info("result spooled", "id", res.ID, "status", res.Status)
	l.wake()
	return runErr
}

// progress is called with mu held. Engine CLIs expose final output only, so
// percentages describe lifecycle milestones rather than invented byte progress.
func (l *Loop) progress(id, phase string, pct float64) {
	if id != "" {
		state := l.commands[id]
		state.Ack = control.CommandAck{Status: "running", Phase: phase, Pct: pct}
		l.commands[id] = state
	}
}

func (l *Loop) command(_ context.Context, cmd control.Command) control.CommandAck {
	l.mu.Lock()
	defer l.mu.Unlock()
	if state, ok := l.commands[cmd.ID]; ok {
		return state.Ack
	}
	if len(l.commands) >= 10000 {
		for id, state := range l.commands {
			if state.Ack.Status != "running" {
				if err := l.spool.DeleteCommand(id); err != nil {
					l.fail(err)
					return state.Ack
				}
				delete(l.commands, id)
				break
			}
		}
		if len(l.commands) >= 10000 {
			return control.CommandAck{Status: "running", Phase: "waiting", Pct: 0}
		}
	}
	ack := control.CommandAck{Status: "running", Phase: "queued", Pct: 0}
	// The worker cannot read the record until the lock is released.
	state := commandState{Ack: ack}
	data, _ := json.Marshal(state)
	if err := l.spool.SaveCommand(cmd.ID, data); err != nil {
		l.fail(errors.New("command journal write failed"))
		return control.CommandAck{Status: "running", Phase: "waiting"}
	}
	if l.Scheduler.RequestRun(cmd.ID) {
		l.commands[cmd.ID] = state
	} else if err := l.spool.DeleteCommand(cmd.ID); err != nil {
		l.fail(errors.New("command journal write failed"))
	}
	return ack
}

func (l *Loop) delivered(res report.Result, accepted bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, state := range l.commands {
		if state.Result != res.ID {
			continue
		}
		ack := control.CommandAck{Status: "succeeded", Phase: "done", Pct: 100, ResultID: &res.ID}
		if !accepted {
			detail := "result rejected by center"
			ack.Status = "failed"
			ack.Error = &detail
			ack.ResultID = nil
		} else if res.Status == report.StatusFailed {
			ack.Status = "failed"
			ack.Error = res.Error
		}
		state.Ack = ack
		data, _ := json.Marshal(state)
		if err := l.spool.SaveCommand(id, data); err != nil {
			return errors.New("command journal write failed")
		}
		l.commands[id] = state
	}
	return nil
}

func (l *Loop) applyConfig(remote control.AgentConfig, schedule sched.Schedule) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	cfg := l.cfg
	if remote.Engine != "" {
		cfg.name = remote.Engine
	}
	if remote.Threads != 0 {
		if remote.Threads < 1 || remote.Threads > 64 {
			return errors.New("invalid thread count")
		}
		cfg.threads = remote.Threads
	}
	if len(remote.Server) > 0 {
		if string(remote.Server) == "null" {
			return errors.New("invalid server pin")
		}
		if string(remote.Server) == `"auto"` {
			cfg.server = 0
		} else if json.Unmarshal(remote.Server, &cfg.server) != nil || cfg.server <= 0 {
			return errors.New("invalid server pin")
		}
	}
	if remote.Binding != nil {
		cfg.binding = netpath.Binding{Interface: remote.Binding.Interface}
		if remote.Binding.SourceIP != nil {
			cfg.binding.SourceIP = *remote.Binding.SourceIP
		}
	}
	if _, err := netpath.Resolve(cfg.binding); err != nil {
		return fmt.Errorf("invalid binding: %w", err)
	}
	runner, err := engine.NewConfigured(cfg.name, cfg.threads)
	if err != nil {
		return err
	}
	cfg.engine = runner
	l.cfg = cfg
	l.Scheduler.SetSchedule(schedule)
	return nil
}
