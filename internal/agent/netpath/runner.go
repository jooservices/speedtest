package netpath

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/jooservices/speedtest/internal/agent/engine"
)

// RunResult keeps the center-observed public IP authoritative while retaining
// the engine's own external IP in Measurement for cross-checking and reporting.
type RunResult struct {
	Measurement engine.Result `json:"measurement"`
	PublicIP    string        `json:"ip"`
	Warnings    []string      `json:"warnings,omitempty"`
}

// Runner executes a measurement and observes its public IP from the command
// center over the same local binding.
type Runner struct {
	engine     engine.Engine
	centerURL  url.URL
	credential string
	binding    Binding
}

// NewRunner constructs a one-run coordinator. The center URL is the agent API
// base URL returned by the enrollment flow; credentials are held only in
// memory and sent only to that center.
func NewRunner(measurement engine.Engine, centerURL, credential string, binding Binding) (*Runner, error) {
	if measurement == nil {
		return nil, fmt.Errorf("speed-test engine is required")
	}
	parsed, err := url.Parse(centerURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("center URL must be an absolute http or https URL without user info, query, or fragment")
	}
	if strings.TrimSpace(credential) == "" {
		return nil, fmt.Errorf("node credential is required")
	}
	if err := binding.Validate(); err != nil {
		return nil, fmt.Errorf("network binding: %w", err)
	}
	return &Runner{engine: measurement, centerURL: *parsed, credential: credential, binding: binding.normalized()}, nil
}

// Run validates the path before starting Ookla, then sends GET /api/agent/ip
// with that same path. A center echo failure leaves the valid measurement
// intact and is returned as a warning so callers can still report the result.
func (r *Runner) Run(ctx context.Context, opts engine.Options) (RunResult, error) {
	path, err := Resolve(r.binding)
	if err != nil {
		return RunResult{}, fmt.Errorf("network path: %w", err)
	}
	opts.InterfaceName = path.Interface
	opts.SourceIP = ""
	if path.SourceIP != nil {
		opts.SourceIP = path.SourceIP.String()
	}

	measurement, runErr := r.engine.Run(ctx, opts)
	result := RunResult{Measurement: measurement}
	publicIP, err := r.centerPublicIP(ctx, path)
	if err != nil {
		result.Warnings = append(result.Warnings, "public IP detection failed: command center echo endpoint was unavailable")
		return result, runErr
	}
	result.PublicIP = publicIP.String()
	if measurement.ExternalIP != "" {
		reported := net.ParseIP(measurement.ExternalIP)
		switch {
		case reported != nil && !publicAddress(publicIP) && publicAddress(reported):
			// The agent reaches the center over a private or loopback network (same host or LAN), so the
			// center cannot see the real public address; the engine's own report is the better answer.
			result.Warnings = append(result.Warnings, fmt.Sprintf("command center observed the non-public address %s; using the engine-reported public IP %s", publicIP, reported))
			result.PublicIP = reported.String()
		case reported == nil:
			result.Warnings = append(result.Warnings, fmt.Sprintf("engine reported an invalid external IP; command center observed %s", publicIP))
		case !reported.Equal(publicIP):
			result.Warnings = append(result.Warnings, fmt.Sprintf("external IP mismatch: engine reported %s; command center observed %s", reported, publicIP))
		}
	}
	return result, runErr
}

// ListServers discovers targets on the same validated network path as Run.
func (r *Runner) ListServers(ctx context.Context) ([]engine.ServerOption, error) {
	path, err := Resolve(r.binding)
	if err != nil {
		return nil, fmt.Errorf("network path: %w", err)
	}
	lister, ok := r.engine.(engine.ServerLister)
	if !ok {
		return nil, fmt.Errorf("speed-test engine does not support server discovery")
	}
	options := engine.Options{InterfaceName: path.Interface}
	if path.SourceIP != nil {
		options.SourceIP = path.SourceIP.String()
	}
	return lister.ListServers(ctx, options)
}

// publicAddress reports whether ip could be a globally routable address.
func publicAddress(ip net.IP) bool {
	return ip != nil && !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified() && !ip.IsMulticast()
}
