// Package failureclass is the single source of the product failure vocabulary
// stored on results, exposed by the API and shown in the UI.
package failureclass

import (
	"regexp"
	"strings"
)

var http429Status = regexp.MustCompile(`(?i)\b(?:http(?:/\d+(?:\.\d+)?)?|status(?:[\s_-]+code)?|response(?:[\s_-]+status)?(?:[\s_-]+code)?)\s*[:=]?\s*429\b`)

// Fixed vocabulary. New classes require a migration of the results check constraint.
const (
	RateLimited       = "rate_limited"
	NoNetwork         = "no_network"
	ServerUnreachable = "server_unreachable"
	EngineMissing     = "engine_missing"
	Timeout           = "timeout"
	Other             = "other"
)

// All is the ordered vocabulary for docs and tests.
var All = []string{RateLimited, NoNetwork, ServerUnreachable, EngineMissing, Timeout, Other}

// Valid reports whether class is one of the fixed values.
func Valid(class string) bool {
	switch class {
	case RateLimited, NoNetwork, ServerUnreachable, EngineMissing, Timeout, Other:
		return true
	default:
		return false
	}
}

// Normalize returns class when valid, otherwise Other.
func Normalize(class string) string {
	if Valid(class) {
		return class
	}
	return Other
}

// FromText classifies a free-form engine or process message into the vocabulary.
func FromText(s string) string {
	lower := strings.ToLower(s)
	switch {
	case IsRateLimitedText(s):
		return RateLimited
	case containsAny(lower, "speedtest: binary_missing", "binary_missing", "executable file not found", "not found in $path"):
		return EngineMissing
	case containsAny(lower, "speedtest: timeout", "timed out", "timeout", "deadline exceeded"):
		return Timeout
	case containsAny(lower, "speedtest: no_route", "no_network", "no route", "network is unreachable",
		"network unreachable", "cannot resolve", "could not resolve", "no such host", "lookup ",
		"public_ip_unavailable", "network path unavailable"):
		return NoNetwork
	case containsAny(lower, "speedtest: server_unavailable", "server_unreachable", "server unavailable",
		"server is unavailable", "server is unreachable", "no server is currently available", "unable to connect to server",
		"server is not responding", "server not responding", "connection refused"):
		return ServerUnreachable
	default:
		return Other
	}
}

// IsRateLimitedText matches explicit provider phrases or a bounded HTTP 429 status.
func IsRateLimitedText(s string) bool {
	lower := strings.ToLower(s)
	return containsAny(lower, "too many requests", "rate limit", "rate_limited") || http429Status.MatchString(s)
}

// ShortLabel is the short reason chip text for the Results table.
func ShortLabel(class string) string {
	switch Normalize(class) {
	case RateLimited:
		return "Rate limited"
	case NoNetwork:
		return "No network"
	case ServerUnreachable:
		return "Server unreachable"
	case EngineMissing:
		return "Engine missing"
	case Timeout:
		return "Timeout"
	default:
		return "Other"
	}
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}
