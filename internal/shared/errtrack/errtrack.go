// Package errtrack sends unexpected errors and panics to Sentry. It is off unless a DSN is configured, scrubs
// everything that could identify a person or carry a secret before an event leaves the process, and never makes the
// program depend on Sentry being reachable: all calls are no-ops when disabled and never block for long.
package errtrack

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
)

// Options configures error tracking.
type Options struct {
	DSN         string
	Environment string
	Release     string
	// Component is "center" or "agent"; it is a tag on every event.
	Component  string
	SampleRate float64
	// Transport replaces the network transport (tests).
	Transport sentry.Transport
}

// FromEnv reads SPEEDTEST_SENTRY_DSN, SPEEDTEST_SENTRY_ENVIRONMENT (default APP_ENV, then "production") and
// SPEEDTEST_SENTRY_SAMPLE_RATE (0 to 1, default 1).
func FromEnv(component, release string) (Options, error) {
	o := Options{DSN: strings.TrimSpace(os.Getenv("SPEEDTEST_SENTRY_DSN")), Component: component, Release: release, SampleRate: 1}
	o.Environment = firstNonEmpty(os.Getenv("SPEEDTEST_SENTRY_ENVIRONMENT"), os.Getenv("APP_ENV"), "production")
	if raw := strings.TrimSpace(os.Getenv("SPEEDTEST_SENTRY_SAMPLE_RATE")); raw != "" {
		rate, err := strconv.ParseFloat(raw, 64)
		if err != nil || rate < 0 || rate > 1 {
			return o, errors.New("SPEEDTEST_SENTRY_SAMPLE_RATE must be a number from 0 to 1")
		}
		o.SampleRate = rate
	}
	return o, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

var enabled bool

// Init starts error tracking. With an empty DSN it does nothing and returns a no-op flush. The returned function
// waits up to two seconds for pending events; call it on shutdown.
func Init(o Options) (func(), error) {
	enabled = false
	if o.DSN == "" {
		return func() {}, nil
	}
	err := sentry.Init(sentry.ClientOptions{
		Dsn:              o.DSN,
		Environment:      o.Environment,
		Release:          o.Release,
		SampleRate:       o.SampleRate,
		AttachStacktrace: true,
		Transport:        o.Transport,
		BeforeSend:       func(event *sentry.Event, _ *sentry.EventHint) *sentry.Event { return Scrub(event) },
	})
	if err != nil {
		return func() {}, fmt.Errorf("error tracking is off: %w", err)
	}
	sentry.ConfigureScope(func(scope *sentry.Scope) { scope.SetTag("component", o.Component) })
	enabled = true
	return func() { sentry.Flush(2 * time.Second) }, nil
}

// Enabled reports whether events are being sent.
func Enabled() bool { return enabled }

// CaptureError reports an unexpected error with low-cardinality tags (never values taken from a request).
func CaptureError(err error, tags map[string]string) {
	if !enabled || err == nil {
		return
	}
	sentry.WithScope(func(scope *sentry.Scope) {
		for k, v := range tags {
			scope.SetTag(k, v)
		}
		sentry.CaptureException(err)
	})
}

// CaptureMessage reports a problem that is not a Go error, grouped by the given fingerprint.
func CaptureMessage(message string, fingerprint []string, tags map[string]string) {
	if !enabled {
		return
	}
	sentry.WithScope(func(scope *sentry.Scope) {
		for k, v := range tags {
			scope.SetTag(k, v)
		}
		if len(fingerprint) > 0 {
			scope.SetFingerprint(fingerprint)
		}
		sentry.CaptureMessage(message)
	})
}

// CapturePanic reports a recovered panic value.
func CapturePanic(recovered any, tags map[string]string) {
	if !enabled || recovered == nil {
		return
	}
	err, ok := recovered.(error)
	if !ok {
		err = fmt.Errorf("panic: %v", recovered)
	}
	CaptureError(err, tags)
}

// Recover is for `defer errtrack.Recover(tags)` at the top of a goroutine or command: it reports the panic, waits for
// the event to be sent and panics again so the program still stops loudly.
func Recover(tags map[string]string) {
	if recovered := recover(); recovered != nil {
		CapturePanic(recovered, tags)
		sentry.Flush(2 * time.Second)
		panic(recovered)
	}
}

var (
	secretAssignment = regexp.MustCompile(`(?i)\b(token|secret|password|passphrase|authorization|api[_-]?key|dsn)\b\s*[=:]\s*\S+`)
	bearerValue      = regexp.MustCompile(`(?i)\bbearer\s+\S+`)
	nodeToken        = regexp.MustCompile(`\b(?:sp|st)_[A-Za-z0-9_-]{16,}`)
	emailAddress     = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	ipv4Address      = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
)

func redact(text string) string {
	text = bearerValue.ReplaceAllString(text, "Bearer [redacted]")
	text = secretAssignment.ReplaceAllString(text, "$1=[redacted]")
	text = nodeToken.ReplaceAllString(text, "[redacted-token]")
	text = emailAddress.ReplaceAllString(text, "[redacted-email]")
	return ipv4Address.ReplaceAllString(text, "[redacted-ip]")
}

// Scrub removes request data, cookies, headers, user identity, breadcrumbs and anything in messages that looks like a
// secret, an e-mail address or an IP address. It runs on every event just before it is sent.
func Scrub(event *sentry.Event) *sentry.Event {
	if event == nil {
		return nil
	}
	event.Request = nil
	event.User = sentry.User{}
	event.Breadcrumbs = nil
	event.Contexts = nil
	event.Message = redact(event.Message)
	for i := range event.Exception {
		event.Exception[i].Value = redact(event.Exception[i].Value)
	}
	for k, v := range event.Tags {
		event.Tags[k] = redact(v)
	}
	event.ServerName = ""
	return event
}
