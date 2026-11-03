package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// LibreSpeedConfig configures a librespeed-cli executable.
type LibreSpeedConfig struct {
	Binary     string
	ServerJSON string
	Concurrent int
	Timeout    time.Duration
}

// LibreSpeedConfigFromEnv reads the CLI settings exported from .env by the
// application launcher. A blank server-list URL keeps the CLI's own default.
func LibreSpeedConfigFromEnv() (LibreSpeedConfig, error) {
	c := LibreSpeedConfig{
		Binary:     os.Getenv("SPEEDTEST_LIBRESPEED_BINARY"),
		ServerJSON: os.Getenv("SPEEDTEST_LIBRESPEED_SERVER_JSON"),
		Concurrent: 4,
		Timeout:    DefaultTimeout,
	}
	if raw := os.Getenv("SPEEDTEST_LIBRESPEED_CONCURRENT"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 {
			return LibreSpeedConfig{}, errors.New("SPEEDTEST_LIBRESPEED_CONCURRENT must be a positive integer")
		}
		c.Concurrent = value
	}
	if raw := os.Getenv("SPEEDTEST_LIBRESPEED_TIMEOUT"); raw != "" {
		value, err := time.ParseDuration(raw)
		if err != nil || value <= 0 {
			return LibreSpeedConfig{}, errors.New("SPEEDTEST_LIBRESPEED_TIMEOUT must be a positive duration")
		}
		c.Timeout = value
	}
	return c, nil
}

// LibreSpeed runs librespeed-cli and normalizes its --json output.
// It is immutable after construction and safe for concurrent Run calls.
type LibreSpeed struct{ config LibreSpeedConfig }

var _ Engine = (*LibreSpeed)(nil)

// NewLibreSpeed validates configuration and fills CLI defaults.
func NewLibreSpeed(c LibreSpeedConfig) (*LibreSpeed, error) {
	if c.Timeout < 0 {
		return nil, errors.New("engine timeout must not be negative")
	}
	if c.Timeout == 0 {
		c.Timeout = DefaultTimeout
	}
	if c.Binary == "" {
		c.Binary = "librespeed-cli"
	}
	if c.Concurrent == 0 {
		c.Concurrent = 4
	}
	if c.Concurrent < 1 {
		return nil, errors.New("LibreSpeed concurrent connections must be positive")
	}
	if c.ServerJSON != "" {
		u, err := url.ParseRequestURI(c.ServerJSON)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return nil, errors.New("LibreSpeed server list must be an HTTP or HTTPS URL without credentials")
		}
	}
	return &LibreSpeed{config: c}, nil
}

// NewConfigured selects an engine for one node and applies that node's
// optional parallel_connections value to LibreSpeed. Zero uses .env defaults.
func NewConfigured(name string, parallelConnections int) (Engine, error) {
	if parallelConnections < 0 {
		return nil, errors.New("parallel_connections must be positive")
	}
	if name == "" {
		name = "ookla"
	}
	switch name {
	case "ookla":
		c, err := ConfigFromEnv()
		if err != nil {
			return nil, err
		}
		return NewOokla(c)
	case "librespeed":
		c, err := LibreSpeedConfigFromEnv()
		if err != nil {
			return nil, err
		}
		if parallelConnections > 0 {
			c.Concurrent = parallelConnections
		}
		return NewLibreSpeed(c)
	case "speedtestcli":
		c, err := SpeedtestCLIConfigFromEnv()
		if err != nil {
			return nil, err
		}
		return NewSpeedtestCLI(c)
	default:
		return nil, fmt.Errorf("unsupported speed test engine %q", name)
	}
}

func (e *LibreSpeed) Run(ctx context.Context, opts Options) (Result, error) {
	// librespeed-cli can bind a source address (--source) but not an interface; refuse
	// an interface binding instead of silently measuring the default route.
	if opts.ServerID < 0 || opts.Timeout < 0 || opts.InterfaceName != "" || !validNetworkOptions(opts) {
		return failed(Result{}, InvalidOptions, nil)
	}
	timeout := e.config.Timeout
	if opts.Timeout > 0 {
		timeout = opts.Timeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{"--json", "--concurrent", strconv.Itoa(e.config.Concurrent)}
	if e.config.ServerJSON != "" {
		args = append(args, "--server-json", e.config.ServerJSON)
	}
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
	r, err := ParseLibreSpeed(stdout.data)
	if err != nil {
		return r, err
	}
	if r.Server != nil && opts.ServerID > 0 {
		r.Server.ID = opts.ServerID
	}
	return r, nil
}
