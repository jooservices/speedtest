package failureclass_test

import (
	"testing"

	"github.com/jooservices/speedtest/internal/shared/failureclass"
)

func TestVocabulary(t *testing.T) {
	if len(failureclass.All) != 6 {
		t.Fatalf("All = %v", failureclass.All)
	}
	for _, class := range failureclass.All {
		if !failureclass.Valid(class) {
			t.Fatalf("Valid(%q) = false", class)
		}
		if failureclass.Normalize(class) != class {
			t.Fatalf("Normalize(%q) changed the value", class)
		}
		if failureclass.ShortLabel(class) == "" {
			t.Fatalf("ShortLabel(%q) empty", class)
		}
	}
	if failureclass.Valid("unknown") || failureclass.Normalize("unknown") != failureclass.Other {
		t.Fatal("unknown class must fall back to other")
	}
}

func TestFromText(t *testing.T) {
	cases := map[string]string{
		"Too many requests":                 failureclass.RateLimited,
		"HTTP 429 from provider":            failureclass.RateLimited,
		"speedtest: timeout":                failureclass.Timeout,
		"context deadline exceeded":         failureclass.Timeout,
		"speedtest: no_route":               failureclass.NoNetwork,
		"dial: network is unreachable":      failureclass.NoNetwork,
		"speedtest: public_ip_unavailable":  failureclass.NoNetwork,
		"speedtest: server_unavailable":     failureclass.ServerUnreachable,
		"connection refused by test server": failureclass.ServerUnreachable,
		"speedtest: binary_missing":         failureclass.EngineMissing,
		"exec: executable file not found":   failureclass.EngineMissing,
		"speedtest: invalid_result":         failureclass.Other,
		"":                                  failureclass.Other,
	}
	for text, want := range cases {
		if got := failureclass.FromText(text); got != want {
			t.Fatalf("FromText(%q) = %q, want %q", text, got, want)
		}
	}
}
