// Package engine executes speed-test programs and normalizes their measurements.
package engine

import (
	"context"
	"encoding/json"
	"time"
)

// Engine is implemented by each independently configurable measurement backend.
type Engine interface {
	Run(context.Context, Options) (Result, error)
}

// Options applies to one measurement. Zero ServerID lets the engine select a server.
// InterfaceName and SourceIP are mutually exclusive and bind the Ookla CLI to
// one local path. Zero Timeout uses the configured timeout; a caller's earlier
// deadline wins.
type Options struct {
	ServerID      int64
	Timeout       time.Duration
	InterfaceName string
	SourceIP      string
}

// LoadedLatency preserves engine-reported milliseconds; nil means unavailable.
type LoadedLatency struct {
	Low  *float64 `json:"low"`
	IQM  *float64 `json:"iqm"`
	High *float64 `json:"high"`
}

// Server retains the engine's server identity without assigning network topology.
type Server struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Location string `json:"location"`
	Host     string `json:"host"`
}

// Result contains measurements only, not the reporting UUID or node identity.
// Speeds are Mbps, latencies ms, packet loss percent and duration seconds.
// Raw preserves all engine fields (including fields not normalized here).
// Missing measurements stay nil, including on failure; Error mirrors the returned error.
type Result struct {
	Timestamp                            time.Time
	Down, Up, Ping, Jitter, Loss         *float64
	LoadedDown, LoadedUp                 *LoadedLatency
	Server                               *Server
	ISP, ExternalIP, ResultID, ResultURL string
	BytesDown, BytesUp                   int64
	Duration                             float64
	Raw                                  json.RawMessage
	Error                                string
}

// ErrorClass identifies actionable failures without exposing subprocess diagnostics.
type ErrorClass string

const (
	Timeout           ErrorClass = "timeout"
	NoRoute           ErrorClass = "no_route"
	ServerUnavailable ErrorClass = "server_unavailable"
	License           ErrorClass = "license"
	BinaryMissing     ErrorClass = "binary_missing"
	Canceled          ErrorClass = "canceled"
	InvalidResult     ErrorClass = "invalid_result"
	Execution         ErrorClass = "execution"
	InvalidOptions    ErrorClass = "invalid_options"
)

// RunError can be inspected with errors.As; Unwrap preserves context/OS errors.
type RunError struct {
	Class ErrorClass
	cause error
}

func (e *RunError) Error() string { return "speedtest: " + string(e.Class) }
func (e *RunError) Unwrap() error { return e.cause }

func failed(r Result, class ErrorClass, cause error) (Result, error) {
	err := &RunError{Class: class, cause: cause}
	r.Error = err.Error()
	return r, err
}
