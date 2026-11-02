package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// runScenario runs the fake for one scenario and returns the exit code, stdout
// and stderr.
func runScenario(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// decodeResult parses stdout as an Ookla result.
func decodeResult(t *testing.T, out string) speedtestResult {
	t.Helper()
	var res speedtestResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("stdout is not valid Ookla JSON: %v\n%s", err, out)
	}
	return res
}

func TestRunSuccess(t *testing.T) {
	code, out, errOut := runScenario(t, "-scenario", "success")

	if code != 0 {
		t.Fatalf("run() = %d, want 0 (stderr %q)", code, errOut)
	}
	if errOut != "" {
		t.Fatalf("stderr = %q, want empty", errOut)
	}
	res := decodeResult(t, out)
	if res.Type != "result" {
		t.Fatalf("type = %q, want result", res.Type)
	}
	if res.Download.Bandwidth != mbpsToBandwidth(300) {
		t.Fatalf("download bandwidth = %d, want %d", res.Download.Bandwidth, mbpsToBandwidth(300))
	}
	if res.Upload.Bandwidth != mbpsToBandwidth(150) {
		t.Fatalf("upload bandwidth = %d, want %d", res.Upload.Bandwidth, mbpsToBandwidth(150))
	}
	if res.PacketLoss != nil {
		t.Fatalf("packetLoss = %v, want 0", res.PacketLoss)
	}
	if res.Result.Persisted != true {
		t.Fatalf("result.persisted = %v, want true", res.Result.Persisted)
	}
}

func TestRunSlow(t *testing.T) {
	code, out, errOut := runScenario(t, "-scenario", "slow")

	if code != 0 {
		t.Fatalf("run() = %d, want 0 (stderr %q)", code, errOut)
	}
	res := decodeResult(t, out)
	if res.Download.Bandwidth != mbpsToBandwidth(8) {
		t.Fatalf("download bandwidth = %d, want %d", res.Download.Bandwidth, mbpsToBandwidth(8))
	}
	if res.PacketLoss != nil {
		t.Fatalf("packetLoss = %v, want 0", res.PacketLoss)
	}
}

func TestRunLoss(t *testing.T) {
	code, out, errOut := runScenario(t, "-scenario", "loss")

	if code != 0 {
		t.Fatalf("run() = %d, want 0 (stderr %q)", code, errOut)
	}
	res := decodeResult(t, out)
	if res.PacketLoss == nil || *res.PacketLoss <= 0 {
		t.Fatalf("packetLoss = %v, want > 0", res.PacketLoss)
	}
}

func TestRunCrash(t *testing.T) {
	code, out, errOut := runScenario(t, "-scenario", "crash")

	if code != 2 {
		t.Fatalf("run() = %d, want 2", code)
	}
	if out != "" {
		t.Fatalf("stdout = %q, want empty (no valid JSON on crash)", out)
	}
	if !strings.Contains(errOut, "simulated engine crash") {
		t.Fatalf("stderr = %q, want it to explain the crash", errOut)
	}
}

func TestRunTimeoutHangsUntilCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"-scenario", "timeout"}, &stdout, &stderr) }()

	select {
	case code := <-done:
		t.Fatalf("run() returned %d before cancellation, want it to hang", code)
	case <-time.After(30 * time.Millisecond):
	}

	cancel()

	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("run() = %d after cancellation, want 1", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run() did not return after cancellation")
	}

	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty on timeout", stdout.String())
	}
}

func TestRunOverridesScenarioValues(t *testing.T) {
	code, out, errOut := runScenario(t, "-scenario", "success", "-down", "500", "-up", "250", "-ping", "12", "-jitter", "3", "-loss", "5", "-server-id", "99")

	if code != 0 {
		t.Fatalf("run() = %d, want 0 (stderr %q)", code, errOut)
	}
	res := decodeResult(t, out)
	if res.Download.Bandwidth != mbpsToBandwidth(500) {
		t.Fatalf("download bandwidth = %d, want %d", res.Download.Bandwidth, mbpsToBandwidth(500))
	}
	if res.Upload.Bandwidth != mbpsToBandwidth(250) {
		t.Fatalf("upload bandwidth = %d, want %d", res.Upload.Bandwidth, mbpsToBandwidth(250))
	}
	if res.Ping.Latency != 12 {
		t.Fatalf("ping latency = %v, want 12", res.Ping.Latency)
	}
	if res.Ping.Jitter != 3 {
		t.Fatalf("ping jitter = %v, want 3", res.Ping.Jitter)
	}
	if res.PacketLoss == nil || *res.PacketLoss != 5 {
		t.Fatalf("packetLoss = %v, want 5", res.PacketLoss)
	}
	if res.Server.ID != 99 {
		t.Fatalf("server id = %d, want 99", res.Server.ID)
	}
}

func TestRunUnknownScenario(t *testing.T) {
	code, out, errOut := runScenario(t, "-scenario", "bogus")

	if code != 2 {
		t.Fatalf("run() = %d, want 2", code)
	}
	if out != "" {
		t.Fatalf("stdout = %q, want empty", out)
	}
	if !strings.Contains(errOut, `unknown scenario "bogus"`) {
		t.Fatalf("stderr = %q, want it to name the scenario", errOut)
	}
}

func TestRunTimestampIsRFC3339UTC(t *testing.T) {
	_, out, _ := runScenario(t, "-scenario", "success")
	res := decodeResult(t, out)

	ts, err := time.Parse(time.RFC3339, res.Timestamp)
	if err != nil {
		t.Fatalf("timestamp %q is not RFC 3339: %v", res.Timestamp, err)
	}
	if !strings.HasSuffix(res.Timestamp, "Z") {
		t.Fatalf("timestamp %q must end in Z (UTC)", res.Timestamp)
	}
	if ts.Location() != time.UTC {
		t.Fatalf("timestamp location = %v, want UTC", ts.Location())
	}
}

func TestRunHelp(t *testing.T) {
	code, out, errOut := runScenario(t, "-h")

	if code != 2 {
		t.Fatalf("run() = %d, want 2", code)
	}
	if out != "" {
		t.Fatalf("stdout = %q, want empty", out)
	}
	if !strings.Contains(errOut, "usage: fake-speedtest") {
		t.Fatalf("stderr = %q, want usage text", errOut)
	}
}

func TestRunUnknownFlag(t *testing.T) {
	code, out, errOut := runScenario(t, "-bogus")

	if code != 2 {
		t.Fatalf("run() = %d, want 2", code)
	}
	if out != "" {
		t.Fatalf("stdout = %q, want empty", out)
	}
	if !strings.Contains(errOut, "flag provided but not defined") {
		t.Fatalf("stderr = %q, want it to name the unknown flag", errOut)
	}
}

// failingWriter always fails on Write, emulating a broken stdout.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, bytes.ErrTooLarge
}

func TestRunWriteError(t *testing.T) {
	var errBuf bytes.Buffer
	code := run(context.Background(), []string{"-scenario", "success"}, failingWriter{}, &errBuf)

	if code != 2 {
		t.Fatalf("run() = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "write result") {
		t.Fatalf("stderr = %q, want it to name the write failure", errBuf.String())
	}
}

func TestMbpsToBandwidth(t *testing.T) {
	tests := []struct {
		mbps float64
		want int64
	}{
		{1000, 125_000_000},
		{300, 37_500_000},
		{1, 125_000},
		{0, 0},
		{0.008, 1000},
	}
	for _, tt := range tests {
		if got := mbpsToBandwidth(tt.mbps); got != tt.want {
			t.Fatalf("mbpsToBandwidth(%v) = %d, want %d", tt.mbps, got, tt.want)
		}
	}
}
