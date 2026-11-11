package report_test

import (
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/agent/engine"
	"github.com/jooservices/speedtest/internal/agent/report"
	"github.com/jooservices/speedtest/internal/shared/failureclass"
)

func TestFromEngineClassifiesFailures(t *testing.T) {
	cases := []struct {
		err  string
		want string
	}{
		{"Too many requests", failureclass.RateLimited},
		{"speedtest: timeout", failureclass.Timeout},
		{"speedtest: no_route", failureclass.NoNetwork},
		{"speedtest: server_unavailable", failureclass.ServerUnreachable},
		{"speedtest: binary_missing", failureclass.EngineMissing},
		{"speedtest: invalid_result", failureclass.Other},
	}
	for _, tc := range cases {
		res, err := report.FromEngine(engine.Result{Error: tc.err, Timestamp: time.Unix(1, 0).UTC()}, report.Options{Threads: 1})
		if err != nil {
			t.Fatalf("%s: %v", tc.err, err)
		}
		if res.Status != report.StatusFailed || res.FailureClass != tc.want {
			t.Fatalf("%s: status=%s class=%s want %s", tc.err, res.Status, res.FailureClass, tc.want)
		}
	}
	ok, err := report.FromEngine(engine.Result{
		Timestamp: time.Unix(1, 0).UTC(),
		Down:      floatPtr(10), Up: floatPtr(5), Ping: floatPtr(1),
		Server:     &engine.Server{ID: 1, Name: "s", Host: "h"},
		ExternalIP: "203.0.113.9",
	}, report.Options{Threads: 1, IP: "203.0.113.9"})
	if err != nil || ok.FailureClass != failureclass.Other {
		t.Fatalf("success class = %q err=%v", ok.FailureClass, err)
	}
}

func floatPtr(v float64) *float64 { return &v }
