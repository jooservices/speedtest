package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestScenarioWireMeasurements(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		down, up, ping, jitter float64
		loss                   *float64
	}{
		{"success", 300, 150, 8, 1, nil}, {"slow", 8, 3, 250, 60, nil}, {"loss", 120, 100, 45, 22, new(float64(12))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, stderr bytes.Buffer
			if code := run(t.Context(), []string{"--scenario", tc.name}, &out, &stderr); code != 0 {
				t.Fatalf("exit %d: %s", code, &stderr)
			}
			var result speedtestResult
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Type != "result" || result.Download.Bandwidth != int64(tc.down*1e6/8) || result.Upload.Bandwidth != int64(tc.up*1e6/8) {
				t.Fatalf("bandwidth: %+v", result)
			}
			if result.Download.Bytes != result.Download.Bandwidth*8 || result.Download.Elapsed != 8000 || result.Upload.Bytes != result.Upload.Bandwidth*10 || result.Upload.Elapsed != 10000 {
				t.Fatalf("transfer: %+v/%+v", result.Download, result.Upload)
			}
			if result.Ping.Latency != tc.ping || result.Ping.Jitter != tc.jitter || result.Download.Latency.Iqm != tc.ping || result.Upload.Latency.Jitter != tc.jitter || result.Download.Latency.Low != tc.ping*0.9 || result.Upload.Latency.High != tc.ping*1.1 {
				t.Fatal("latency mapping changed")
			}
			if (result.PacketLoss == nil) != (tc.loss == nil) || tc.loss != nil && *result.PacketLoss != *tc.loss {
				t.Fatal("loss mapping changed")
			}
			if _, err := time.Parse(time.RFC3339, result.Timestamp); err != nil || !strings.HasSuffix(result.Timestamp, "Z") {
				t.Fatal("invalid UTC timestamp")
			}
			if result.Server.ID != 12345 || result.Interface.ExternalIP != "203.0.113.10" || !result.Result.Persisted {
				t.Fatal("identity mapping changed")
			}
		})
	}
}

func TestLibreSpeedFallbackFixtureUsesCurrentTimestamp(t *testing.T) {
	started := time.Now().Add(-time.Second)
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, &stderr)
	}
	var report struct {
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	timestamp, err := time.Parse(time.RFC3339Nano, report.Timestamp)
	if err != nil {
		t.Fatalf("timestamp %q: %v", report.Timestamp, err)
	}
	if timestamp.Before(started) || timestamp.After(time.Now().Add(time.Second)) {
		t.Fatalf("fixture timestamp %s is not current", timestamp)
	}
}

func TestExplicitOverridesAndFailures(t *testing.T) {
	var out bytes.Buffer
	cfg, err := parseFlags([]string{"--down", "1", "--up", "2", "--ping", "3", "--jitter", "4", "--loss", "0", "--server-id", "99"}, &out)
	if err != nil || cfg.down != 1 || cfg.up != 2 || cfg.ping != 3 || cfg.jitter != 4 || cfg.loss != 0 || !cfg.lossAvailable || cfg.serverID != 99 {
		t.Fatalf("override: %+v %v", cfg, err)
	}
	for _, args := range [][]string{{"--scenario", "unknown"}, {"--unknown"}, {"--help"}, {"--scenario", "crash"}} {
		out.Reset()
		if code := run(t.Context(), args, &out, &out); code != 2 || out.Len() == 0 {
			t.Fatalf("%v: exit %d output %s", args, code, &out)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if code := run(ctx, []string{"--scenario", "timeout"}, &out, &out); code != 1 {
		t.Fatalf("timeout exit %d", code)
	}
	out.Reset()
	if code := run(t.Context(), nil, brokenWriter{}, &out); code != 2 || !strings.Contains(out.String(), "write result") {
		t.Fatalf("write failure: %d %s", code, &out)
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("fixture failure") }
