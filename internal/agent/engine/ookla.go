package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// DefaultTimeout bounds a test when no per-run or environment timeout is supplied.
const DefaultTimeout = 120 * time.Second
const maxOutput = 64 * 1024

// Config configures an Ookla executable. Nil Args selects the standard CLI flags;
// non-nil Args replaces them, allowing wrappers and offline test executables.
// Args are passed directly, never interpreted by a shell. Do not include secrets.
type Config struct {
	Binary  string
	Args    []string
	Timeout time.Duration
}

// ConfigFromEnv reads configuration exported from .env by the launcher.
// Arguments use a JSON string array so spaces and quotes remain single arguments.
func ConfigFromEnv() (Config, error) {
	c := Config{Binary: os.Getenv("SPEEDTEST_OOKLA_BINARY"), Timeout: DefaultTimeout}
	if raw := os.Getenv("SPEEDTEST_OOKLA_ARGS"); raw != "" {
		var args []*string
		if err := json.Unmarshal([]byte(raw), &args); err != nil || args == nil {
			return Config{}, errors.New("SPEEDTEST_OOKLA_ARGS must be a JSON string array")
		}
		c.Args = make([]string, len(args))
		for i, arg := range args {
			if arg == nil {
				return Config{}, errors.New("SPEEDTEST_OOKLA_ARGS must contain only strings")
			}
			c.Args[i] = *arg
		}
	}
	if raw := os.Getenv("SPEEDTEST_OOKLA_TIMEOUT"); raw != "" {
		value, err := time.ParseDuration(raw)
		if err != nil || value <= 0 {
			return Config{}, errors.New("SPEEDTEST_OOKLA_TIMEOUT must be a positive duration")
		}
		c.Timeout = value
	}
	return c, nil
}

// Ookla is immutable after construction and safe for concurrent Run calls.
type Ookla struct{ config Config }

var _ Engine = (*Ookla)(nil)

// NewOokla validates defaults and copies argument storage owned by the caller.
func NewOokla(c Config) (*Ookla, error) {
	if c.Timeout < 0 {
		return nil, errors.New("engine timeout must not be negative")
	}
	if c.Timeout == 0 {
		c.Timeout = DefaultTimeout
	}
	if c.Binary == "" {
		c.Binary = "speedtest"
	}
	if c.Args == nil {
		c.Args = []string{"--accept-license", "--accept-gdpr", "-f", "json"}
	}
	c.Args = append([]string{}, c.Args...)
	return &Ookla{config: c}, nil
}

// capture drains output while bounding retained memory, even for a noisy process.
type capture struct {
	data     []byte
	overflow bool
	limit    int
}

func (b *capture) Write(p []byte) (int, error) {
	n := len(p)
	limit := b.limit
	if limit == 0 {
		limit = maxOutput
	}
	remaining := limit - len(b.data)
	if n > remaining {
		b.overflow = true
		p = p[:remaining]
	}
	b.data = append(b.data, p...)
	return n, nil
}

func (e *Ookla) Run(ctx context.Context, opts Options) (Result, error) {
	if opts.ServerID < 0 || opts.Timeout < 0 || !validNetworkOptions(opts) {
		return failed(Result{}, InvalidOptions, nil)
	}
	timeout := e.config.Timeout
	if opts.Timeout > 0 {
		timeout = opts.Timeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := append([]string{}, e.config.Args...)
	if opts.ServerID > 0 {
		args = append(args, "--server-id", strconv.FormatInt(opts.ServerID, 10))
	}
	if opts.InterfaceName != "" {
		args = append(args, "--interface", opts.InterfaceName)
	}
	if opts.SourceIP != "" {
		args = append(args, "--ip", opts.SourceIP)
	}
	cmd := exec.CommandContext(ctx, e.config.Binary, args...)
	// Inherited pipes from a wrapper's descendants must not delay cancellation indefinitely.
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
		// Preserve valid engine failure JSON, never arbitrary stderr or incomplete output.
		r := Result{}
		if !stdout.overflow && json.Valid(stdout.data) {
			r.Raw = append(json.RawMessage(nil), stdout.data...)
		}
		return failed(r, class, err)
	}
	if stdout.overflow {
		return failed(Result{}, InvalidResult, nil)
	}
	return ParseOokla(stdout.data)
}

// ListServers runs Ookla's JSON server enumeration command on the node's
// configured network path; it does not start a measurement.
func (e *Ookla) ListServers(ctx context.Context, opts Options) ([]ServerOption, error) {
	if !validServerListOptions(opts) {
		return nil, &RunError{Class: InvalidOptions}
	}
	data, err := runServerListCommand(ctx, e.config.Binary, ooklaServerArgs(e.config.Args, opts), e.config.Timeout)
	if err != nil {
		return nil, err
	}
	return ParseOoklaServers(data)
}

func ooklaServerArgs(configured []string, opts Options) []string {
	args := []string{"--servers"}
	for i := 0; i < len(configured); i++ {
		arg := configured[i]
		if arg == "-f" || arg == "--format" {
			i++
			continue
		}
		if strings.HasPrefix(arg, "--format=") {
			continue
		}
		args = append(args, arg)
	}
	args = append(args, "-f", "json")
	if opts.InterfaceName != "" {
		args = append(args, "--interface", opts.InterfaceName)
	}
	if opts.SourceIP != "" {
		args = append(args, "--ip", opts.SourceIP)
	}
	return args
}

func validNetworkOptions(opts Options) bool {
	if opts.InterfaceName != "" && (strings.TrimSpace(opts.InterfaceName) != opts.InterfaceName || opts.SourceIP != "") {
		return false
	}
	if opts.InterfaceName != "" && len(opts.InterfaceName) > 15 {
		return false
	}
	if opts.SourceIP != "" {
		ip := net.ParseIP(opts.SourceIP)
		if ip == nil || strings.TrimSpace(opts.SourceIP) != opts.SourceIP {
			return false
		}
	}
	return true
}

func classify(s string) ErrorClass {
	s = strings.ToLower(s)
	switch {
	case strings.Contains(s, "license"), strings.Contains(s, "licence"), strings.Contains(s, "gdpr"):
		return License
	case strings.Contains(s, "timed out"), strings.Contains(s, "timeout"):
		return Timeout
	case strings.Contains(s, "no route"), strings.Contains(s, "network is unreachable"), strings.Contains(s, "network unreachable"), strings.Contains(s, "cannot resolve"), strings.Contains(s, "could not resolve"), strings.Contains(s, "no such host"), strings.Contains(s, "lookup "):
		return NoRoute
	case strings.Contains(s, "server unavailable"), strings.Contains(s, "server is unavailable"), strings.Contains(s, "no server is currently available"), strings.Contains(s, "unable to connect to server"), strings.Contains(s, "server is not responding"), strings.Contains(s, "server not responding"), strings.Contains(s, "specified server"), strings.Contains(s, "connection refused"):
		return ServerUnavailable
	default:
		return Execution
	}
}
