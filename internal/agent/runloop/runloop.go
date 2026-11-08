// Package runloop composes measurement, scheduling, control and durable delivery.
package runloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	agentconfig "github.com/jooservices/speedtest/internal/agent/config"
	"github.com/jooservices/speedtest/internal/agent/control"
	"github.com/jooservices/speedtest/internal/agent/engine"
	"github.com/jooservices/speedtest/internal/agent/netpath"
	"github.com/jooservices/speedtest/internal/agent/report"
	"github.com/jooservices/speedtest/internal/agent/sched"
	"github.com/jooservices/speedtest/internal/agent/update"
	"github.com/jooservices/speedtest/internal/shared/agentid"
	"github.com/jooservices/speedtest/internal/shared/failureclass"
)

// restartDelay lets the acknowledgement of an update command reach the center before the process exits.
var restartDelay = 3 * time.Second

// watchdogInterval is how often a freshly updated agent checks whether its confirmation window has passed.
var watchdogInterval = 5 * time.Second

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
	// Updater decides on self-updates; its paths come from the environment.
	Updater       *update.Agent
	stop          atomic.Pointer[context.CancelFunc]
	spool         *report.Spool
	reporter      *report.Reporter
	rateLimits    *report.RateLimitGate
	logger        *slog.Logger
	center        string
	credential    string
	mu            sync.Mutex
	cfg           measurementConfig
	engineConfigs map[string]measurementConfig
	commands      map[string]commandState
	notify        chan struct{}
	fatal         chan error
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
	backoffCfg, err := report.RateLimitBackoffConfigFromEnv()
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
	l := &Loop{spool: spool, rateLimits: report.NewRateLimitGate(backoffCfg, nil), logger: logger, center: center.String(), credential: credential,
		cfg: measurementConfig{runner, binding, cfg.ServerID, threads, cfg.Engine}, commands: make(map[string]commandState), notify: make(chan struct{}, 1), fatal: make(chan error, 1)}
	if err := l.restoreCommands(); err != nil {
		return nil, err
	}
	l.reporter = report.NewReporter(spool, &report.Client{BaseURL: center, Credential: credential}, report.WithLogger(logger), report.WithDeliveryObserver(l.delivered))
	l.Scheduler = sched.New(l, logger, schedulerOptions...)
	l.Scheduler.SetSchedule(sched.Schedule{Interval: cfg.Interval, Enabled: true})
	l.Controller = control.New(control.Client{BaseURL: center, Credential: credential, Agent: agentid.Current(version)}, l.Scheduler, control.WithLogger(logger))
	l.Controller.Engines = installedEngines
	l.Controller.ApplyConfig = l.applyConfig
	l.Controller.RunCommand = l.command
	l.Updater = &update.Agent{Paths: update.DefaultPaths(), Version: version, Logger: logger, Restart: l.restartSoon}
	l.Controller.UpdateResult = l.Updater.Result
	l.Controller.HeartbeatOK = l.Updater.Confirm
	l.Controller.OfferUpdate = func(c control.AgentConfig) { l.Updater.Offer(c.AutoUpdate, c.Release) }
	l.Controller.UpdateAgent = l.updateAgent
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
	l.stop.Store(&cancel)
	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); l.watchUpdate(ctx) }()
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

// restartSoon stops the loop shortly, so the service manager starts the freshly installed version.
func (l *Loop) restartSoon() {
	time.AfterFunc(restartDelay, func() {
		if stop := l.stop.Load(); stop != nil {
			(*stop)()
		}
	})
}

// updateAgent serves the owner's update_agent command.
func (l *Loop) updateAgent(cmd control.Command, cfg control.AgentConfig) control.CommandAck {
	version := ""
	if cmd.Version != nil {
		version = *cmd.Version
	}
	if version != "" && version == l.Updater.Version {
		return control.CommandAck{Status: "succeeded", Phase: "already installed", Pct: 100}
	}
	if err := l.Updater.Command(version, cfg.Release); err != nil {
		msg := err.Error()
		return control.CommandAck{Status: "failed", Error: &msg}
	}
	return control.CommandAck{Status: "succeeded", Phase: "restarting to install", Pct: 100}
}

// watchUpdate makes a freshly installed version that cannot confirm itself exit with an error, which lets the
// privileged start step restore the previous binary.
func (l *Loop) watchUpdate(ctx context.Context) {
	t := time.NewTicker(watchdogInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if l.Updater.Expired() {
				l.fail(errors.New("the new version did not complete a heartbeat in time; exiting so the previous version is restored"))
				return
			}
		}
	}
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

func (l *Loop) measure(ctx context.Context, id string) error { return l.measureEngine(ctx, id, "") }

// RunEngine executes a scheduled engine on the scheduler single worker.
func (l *Loop) RunEngine(ctx context.Context, name string) error {
	return l.measureEngine(ctx, "", name)
}

func (l *Loop) measureEngine(ctx context.Context, id, name string) error {
	l.mu.Lock()
	cfg := l.cfg
	if name != "" {
		var ok bool
		cfg, ok = l.engineConfigs[name]
		if !ok {
			l.mu.Unlock()
			return errors.New("engine schedule was removed")
		}
	}
	engineName := cfg.name
	if engineName == "" {
		engineName = name
	}
	// Scheduled slots stay quiet during a provider backoff window; manual run_now still attempts.
	if id == "" && l.rateLimits != nil && l.rateLimits.Blocked(engineName) {
		l.mu.Unlock()
		l.logger.Info("skipping scheduled test during rate-limit backoff", "engine", engineName)
		return nil
	}
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
	res.Engine = engineName
	res.Warnings = out.Warnings
	if !l.acceptResult(engineName, id, res) {
		return runErr
	}
	if err := l.spoolResult(id, res); err != nil {
		return err
	}
	return runErr
}

func (l *Loop) acceptResult(engineName, id string, res report.Result) bool {
	if res.Status != report.StatusFailed {
		if l.rateLimits != nil {
			l.rateLimits.ObserveSuccess(engineName)
		}
		return true
	}
	if l.rateLimits != nil && !l.rateLimits.ObserveFailure(engineName, res.FailureClass) {
		l.logger.Info("rate-limited failure already recorded in backoff window", "engine", engineName, "class", failureclass.RateLimited)
		if id != "" {
			l.failCommand(id, res.Error)
		}
		return false
	}
	return true
}

func (l *Loop) spoolResult(id string, res report.Result) error {
	// Enqueue is independent of cancellation: an interrupted test is still durable.
	l.mu.Lock()
	var state commandState
	if id != "" {
		state = l.commands[id]
		state.Result = res.ID
		state.Ack = control.CommandAck{Status: "running", Phase: "reporting", Pct: 80}
	}
	data, _ := json.Marshal(state)
	err := l.spool.AppendWithState(res, id, data)
	if err != nil {
		l.mu.Unlock()
		l.fail(errors.New("result spool write failed"))
		return errors.New("result spool write failed")
	}
	if id != "" {
		l.commands[id] = state
	}
	l.mu.Unlock()
	l.logger.Info("result spooled", "id", res.ID, "status", res.Status, "failureClass", res.FailureClass)
	l.wake()
	return nil
}

func (l *Loop) failCommand(id string, detail *string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	state := l.commands[id]
	state.Ack = control.CommandAck{Status: "failed", Error: detail, Phase: "rate limited", Pct: 100}
	data, _ := json.Marshal(state)
	if err := l.spool.SaveCommand(id, data); err != nil {
		l.fail(errors.New("command journal write failed"))
		return
	}
	l.commands[id] = state
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
	configs, engineSchedules, err := configuredEngines(remote.Schedules, cfg, schedule)
	if err != nil {
		return err
	}
	l.cfg = cfg
	l.engineConfigs = configs
	l.Scheduler.SetSchedule(schedule)
	l.Scheduler.SetEngines(engineSchedules)
	return nil
}

func (l *Loop) restoreCommands() error {
	saved, err := l.spool.Commands()
	if err != nil {
		_ = l.spool.Close()
		return err
	}
	for id, data := range saved {
		var state commandState
		if json.Unmarshal(data, &state) != nil {
			_ = l.spool.Close()
			return errors.New("invalid command journal")
		}
		if state.Result != "" || state.Ack.Status != "running" {
			l.commands[id] = state
		}
	}
	return nil
}

func installedEngines() []string {
	names := []string{}
	ookla, err := engine.ConfigFromEnv()
	if ookla.Binary == "" {
		ookla.Binary = "speedtest"
	}
	if err == nil {
		if _, err := exec.LookPath(ookla.Binary); err == nil {
			names = append(names, "ookla")
		}
	}
	libre, err := engine.LibreSpeedConfigFromEnv()
	if libre.Binary == "" {
		libre.Binary = "librespeed-cli"
	}
	if err == nil {
		if _, err := exec.LookPath(libre.Binary); err == nil {
			names = append(names, "librespeed")
		}
	}
	return names
}

func configuredEngines(inputs []control.ScheduleInput, cfg measurementConfig, schedule sched.Schedule) (map[string]measurementConfig, []sched.Schedule, error) {
	configs := map[string]measurementConfig{}
	var engineSchedules []sched.Schedule
	seen := map[string]bool{}
	for _, input := range inputs {
		if seen[input.Engine] || (input.Engine != "ookla" && input.Engine != "librespeed") {
			return nil, nil, errors.New("invalid engine schedule")
		}
		seen[input.Engine] = true
		next := cfg
		next.name = input.Engine
		var err error
		next.engine, err = engine.NewConfigured(next.name, next.threads)
		if err != nil {
			return nil, nil, err
		}
		sch := schedule
		sch.Engine, sch.Interval, sch.Offset, sch.Enabled = input.Engine, time.Duration(input.Interval)*time.Minute, time.Duration(input.Offset)*time.Minute, input.Enabled
		if err := sch.Validate(); err != nil {
			return nil, nil, err
		}
		configs[next.name] = next
		engineSchedules = append(engineSchedules, sch)
	}
	return configs, engineSchedules, nil
}
