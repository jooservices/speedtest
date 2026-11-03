package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// SpeedtestCLIConfig configures the speedtest-cli (Python) executable shipped as an Ubuntu package.
type SpeedtestCLIConfig struct {
	Binary  string
	Timeout time.Duration
}

// SpeedtestCLIConfigFromEnv reads SPEEDTEST_SPEEDTESTCLI_BINARY and SPEEDTEST_SPEEDTESTCLI_TIMEOUT.
func SpeedtestCLIConfigFromEnv() (SpeedtestCLIConfig, error) {
	c := SpeedtestCLIConfig{Binary: os.Getenv("SPEEDTEST_SPEEDTESTCLI_BINARY"), Timeout: DefaultTimeout}
	if raw := os.Getenv("SPEEDTEST_SPEEDTESTCLI_TIMEOUT"); raw != "" {
		value, err := time.ParseDuration(raw)
		if err != nil || value <= 0 {
			return SpeedtestCLIConfig{}, errors.New("SPEEDTEST_SPEEDTESTCLI_TIMEOUT must be a positive duration")
		}
		c.Timeout = value
	}
	return c, nil
}

// SpeedtestCLI runs speedtest-cli and normalises its --json output. It is immutable and
// safe for concurrent Run calls.
type SpeedtestCLI struct{ config SpeedtestCLIConfig }

var _ Engine = (*SpeedtestCLI)(nil)

// NewSpeedtestCLI validates the configuration and fills defaults.
func NewSpeedtestCLI(c SpeedtestCLIConfig) (*SpeedtestCLI, error) {
	if c.Timeout < 0 {
		return nil, errors.New("engine timeout must not be negative")
	}
	if c.Timeout == 0 {
		c.Timeout = DefaultTimeout
	}
	if c.Binary == "" {
		c.Binary = "speedtest-cli"
	}
	return &SpeedtestCLI{config: c}, nil
}

// Run measures once. speedtest-cli can bind a source address (--source) but not an
// interface, so an interface binding is refused instead of silently using the default route.
func (e *SpeedtestCLI) Run(ctx context.Context, opts Options) (Result, error) {
	if opts.ServerID < 0 || opts.Timeout < 0 || opts.InterfaceName != "" || !validNetworkOptions(opts) {
		return failed(Result{}, InvalidOptions, nil)
	}
	timeout := e.config.Timeout
	if opts.Timeout > 0 {
		timeout = opts.Timeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{"--json", "--secure"}
	if opts.ServerID > 0 {
		args = append(args, "--server", strconv.FormatInt(opts.ServerID, 10))
	}
	if opts.SourceIP != "" {
		args = append(args, "--source", opts.SourceIP)
	}
	cmd := exec.CommandContext(ctx, e.config.Binary, args...)
	cmd.WaitDelay = 100 * time.Millisecond
	var stdout, stderr capture
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		class := Canceled
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			class = Timeout
		}
		return failed(Result{}, class, ctx.Err())
	}
	if err != nil {
		class := classify(string(stdout.data) + "\n" + string(stderr.data))
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			class = BinaryMissing
		}
		r := Result{}
		if !stdout.overflow && json.Valid(stdout.data) {
			r.Raw = append(json.RawMessage(nil), stdout.data...)
		}
		return failed(r, class, err)
	}
	if stdout.overflow {
		return failed(Result{}, InvalidResult, nil)
	}
	r, err := ParseSpeedtestCLI(stdout.data)
	if err != nil {
		return r, err
	}
	if r.Server != nil && opts.ServerID > 0 && r.Server.ID == 0 {
		r.Server.ID = opts.ServerID
	}
	return r, nil
}
