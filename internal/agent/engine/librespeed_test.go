package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func librespeedFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/librespeed.json")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestParseLibreSpeedRealSample(t *testing.T) {
	raw := librespeedFixture(t)
	r, err := ParseLibreSpeed(raw)
	if err != nil {
		t.Fatal(err)
	}
	if r.Timestamp.Format(time.RFC3339Nano) != "2021-03-16T12:05:15.3206446Z" {
		t.Fatalf("timestamp = %s", r.Timestamp)
	}
	if *r.Down != 39.52 || *r.Up != 3.2 || *r.Ping != 206.36363636363637 || *r.Jitter != 92.92 {
		t.Fatalf("measurements = %+v", r)
	}
	if r.BytesDown != 77692920 || r.BytesUp != 6291456 || r.ExternalIP != "203.0.113.10" || r.ISP != "AS64496 Example Network" {
		t.Fatalf("client/bytes = %+v", r)
	}
	if r.Server == nil || r.Server.ID != 0 || r.Server.Name != "Example LibreSpeed server" || r.Server.Host != "speed.example.test" || r.Server.Location != "" {
		t.Fatalf("server = %+v", r.Server)
	}
	if r.Loss != nil || r.LoadedDown != nil || r.LoadedUp != nil || r.Duration != 0 || r.ResultID != "" || r.ResultURL != "" || r.Error != "" {
		t.Fatalf("unavailable values should remain empty: %+v", r)
	}
	if !bytes.Equal(r.Raw, raw) {
		t.Fatal("raw report was not retained")
	}
	raw[0] = '!'
	if r.Raw[0] != '{' {
		t.Fatal("raw report aliases caller memory")
	}
}

func TestParseLibreSpeedSingleReportArray(t *testing.T) {
	array := append([]byte{'['}, librespeedFixture(t)...)
	array = append(array, ']')
	r, err := ParseLibreSpeed(array)
	if err != nil || *r.Down != 39.52 || *r.Up != 3.2 {
		t.Fatalf("one-report array: %+v %v", r, err)
	}
}

func TestParseLibreSpeedZeroAndISPFields(t *testing.T) {
	var original map[string]any
	if err := json.Unmarshal(librespeedFixture(t), &original); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		clientField string
	}{
		{"isp", "isp"},
		{"organization", "organization"},
		{"org", "org"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := cloneReport(t, original)
			client := report["client"].(map[string]any)
			client["ip"] = ""
			client["isp"] = ""
			client["organization"] = ""
			client["org"] = ""
			client[tc.clientField] = "Example ISP"
			report["timestamp"] = "2026-11-03T00:00:00Z"
			report["bytes_sent"] = 0
			report["bytes_received"] = 0
			report["ping"] = 0
			report["jitter"] = 0
			report["upload"] = 0
			report["download"] = 0
			encoded, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			r, err := ParseLibreSpeed(encoded)
			if err != nil || *r.Down != 0 || *r.Up != 0 || *r.Ping != 0 || *r.Jitter != 0 || r.BytesDown != 0 || r.BytesUp != 0 {
				t.Fatalf("zero-valued report: %+v %v", r, err)
			}
			if r.ISP != "Example ISP" {
				t.Fatalf("ISP = %q", r.ISP)
			}
		})
	}
}

func TestParseLibreSpeedRejectsInvalidReports(t *testing.T) {
	for _, raw := range []string{"", "{", "null", "{}", "[]", "[{},{}]", strings.Repeat(" ", maxOutput+1)} {
		t.Run(raw[:min(len(raw), 12)], func(t *testing.T) {
			r, err := ParseLibreSpeed([]byte(raw))
			assertClass(t, r, err, InvalidResult)
		})
	}
	mutations := []func(map[string]any){
		func(v map[string]any) { delete(v, "timestamp") },
		func(v map[string]any) { delete(v, "ping") },
		func(v map[string]any) { v["download"] = -1.0 },
		func(v map[string]any) { v["jitter"] = -0.1 },
		func(v map[string]any) { v["bytes_sent"] = -1 },
		func(v map[string]any) { v["bytes_received"] = uint64(1) << 63 },
		func(v map[string]any) { v["server"].(map[string]any)["url"] = "//relative/path" },
	}
	for _, mutate := range mutations {
		var report map[string]any
		if err := json.Unmarshal(librespeedFixture(t), &report); err != nil {
			t.Fatal(err)
		}
		mutate(report)
		changed, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		r, err := ParseLibreSpeed(changed)
		assertClass(t, r, err, InvalidResult)
	}
}

func cloneReport(t *testing.T, source map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	var clone map[string]any
	if err := json.Unmarshal(encoded, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func TestLibreSpeedRunnerArgumentsAndPinnedServer(t *testing.T) {
	fixturePath := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(fixturePath, librespeedFixture(t), 0o600); err != nil {
		t.Fatal(err)
	}
	argsPath := filepath.Join(t.TempDir(), "args.txt")
	t.Setenv("LIBRESPEED_FIXTURE", fixturePath)
	t.Setenv("LIBRESPEED_ARGS_PATH", argsPath)
	binary := script(t, "printf '%s\\n' \"$@\" > \"$LIBRESPEED_ARGS_PATH\"\ncat \"$LIBRESPEED_FIXTURE\"\n")
	runner, err := NewLibreSpeed(LibreSpeedConfig{
		Binary: binary, ServerJSON: "https://servers.example.test/list.json?region=apac", Concurrent: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.Run(context.Background(), Options{ServerID: 79})
	if err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "--json\n--concurrent\n7\n--server-json\nhttps://servers.example.test/list.json?region=apac\n--server\n79\n"
	if string(args) != want {
		t.Fatalf("arguments = %q, want %q", args, want)
	}
	if r.Server.ID != 79 {
		t.Fatalf("pinned server id = %d, want 79", r.Server.ID)
	}
}

func TestLibreSpeedRunnerDefaultsAndConcurrentRuns(t *testing.T) {
	fixturePath := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(fixturePath, librespeedFixture(t), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIBRESPEED_FIXTURE", fixturePath)
	runner, err := NewLibreSpeed(LibreSpeedConfig{Binary: script(t, "cat \"$LIBRESPEED_FIXTURE\"\n")})
	if err != nil {
		t.Fatal(err)
	}
	if runner.config.Concurrent != 4 || runner.config.Timeout != DefaultTimeout {
		t.Fatalf("defaults = %+v", runner.config)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			r, err := runner.Run(context.Background(), Options{})
			if err != nil || *r.Down != 39.52 {
				t.Errorf("concurrent run = %+v, %v", r, err)
			}
		})
	}
	wg.Wait()
}

func TestLibreSpeedRunnerFailuresAndConfig(t *testing.T) {
	fake := script(t, "cat 'testdata/librespeed.json'\n")
	for _, tc := range []struct {
		name   string
		config LibreSpeedConfig
		opts   Options
		class  ErrorClass
	}{
		{name: "missing binary", config: LibreSpeedConfig{Binary: filepath.Join(t.TempDir(), "absent")}, class: BinaryMissing},
		{name: "timeout", config: LibreSpeedConfig{Binary: script(t, "sleep 5\n"), Timeout: 20 * time.Millisecond}, class: Timeout},
		{name: "timeout override", config: LibreSpeedConfig{Binary: script(t, "sleep 5\n"), Timeout: time.Second}, opts: Options{Timeout: 20 * time.Millisecond}, class: Timeout},
		{name: "negative server", config: LibreSpeedConfig{Binary: fake}, opts: Options{ServerID: -1}, class: InvalidOptions},
		{name: "negative timeout", config: LibreSpeedConfig{Binary: fake}, opts: Options{Timeout: -1}, class: InvalidOptions},
		{name: "invalid JSON", config: LibreSpeedConfig{Binary: script(t, "printf invalid\n")}, class: InvalidResult},
		{name: "oversized output", config: LibreSpeedConfig{Binary: script(t, "head -c 70000 /dev/zero\n")}, class: InvalidResult},
		{name: "no route", config: LibreSpeedConfig{Binary: script(t, "echo 'dial tcp: lookup example.test: no such host' >&2\nexit 1\n")}, class: NoRoute},
		{name: "unavailable server", config: LibreSpeedConfig{Binary: script(t, "echo 'No server is currently available' >&2\nexit 1\n")}, class: ServerUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, err := NewLibreSpeed(tc.config)
			if err != nil {
				t.Fatal(err)
			}
			r, err := runner.Run(context.Background(), tc.opts)
			assertClass(t, r, err, tc.class)
		})
	}
	failedJSON, err := NewLibreSpeed(LibreSpeedConfig{Binary: script(t, "cat 'testdata/librespeed.json'\nexit 1\n")})
	if err != nil {
		t.Fatal(err)
	}
	r, err := failedJSON.Run(context.Background(), Options{})
	assertClass(t, r, err, Execution)
	if !bytes.Equal(r.Raw, librespeedFixture(t)) {
		t.Fatal("valid failure output was not retained")
	}

	if _, err := NewLibreSpeed(LibreSpeedConfig{Concurrent: -1}); err == nil {
		t.Fatal("negative concurrent count accepted")
	}
	if _, err := NewLibreSpeed(LibreSpeedConfig{ServerJSON: "file:///tmp/servers.json"}); err == nil {
		t.Fatal("non-HTTP server-list URL accepted")
	}
	if _, err := NewLibreSpeed(LibreSpeedConfig{ServerJSON: "https://user@servers.example.test/list.json"}); err == nil {
		t.Fatal("credential-bearing server-list URL accepted")
	}
	if _, err := NewLibreSpeed(LibreSpeedConfig{Timeout: -1}); err == nil {
		t.Fatal("negative timeout accepted")
	}
}

func TestLibreSpeedConfigFromEnvAndNodeOverride(t *testing.T) {
	for _, key := range []string{"SPEEDTEST_LIBRESPEED_BINARY", "SPEEDTEST_LIBRESPEED_SERVER_JSON", "SPEEDTEST_LIBRESPEED_CONCURRENT", "SPEEDTEST_LIBRESPEED_TIMEOUT"} {
		t.Setenv(key, "")
	}
	c, err := LibreSpeedConfigFromEnv()
	if err != nil || c.Concurrent != 4 || c.Timeout != DefaultTimeout || c.Binary != "" || c.ServerJSON != "" {
		t.Fatalf("defaults = %+v, %v", c, err)
	}
	t.Setenv("SPEEDTEST_LIBRESPEED_BINARY", "/path with spaces/librespeed-cli")
	t.Setenv("SPEEDTEST_LIBRESPEED_SERVER_JSON", "https://servers.example.test/list.json")
	t.Setenv("SPEEDTEST_LIBRESPEED_CONCURRENT", "9")
	t.Setenv("SPEEDTEST_LIBRESPEED_TIMEOUT", "45s")
	c, err = LibreSpeedConfigFromEnv()
	if err != nil || c.Binary != "/path with spaces/librespeed-cli" || c.ServerJSON != "https://servers.example.test/list.json" || c.Concurrent != 9 || c.Timeout != 45*time.Second {
		t.Fatalf("custom config = %+v, %v", c, err)
	}
	selected, err := NewConfigured("librespeed", 6)
	if err != nil {
		t.Fatal(err)
	}
	if runner := selected.(*LibreSpeed); runner.config.Concurrent != 6 {
		t.Fatalf("node override = %d, want 6", runner.config.Concurrent)
	}
	for _, keyValue := range [][2]string{{"SPEEDTEST_LIBRESPEED_CONCURRENT", "zero"}, {"SPEEDTEST_LIBRESPEED_CONCURRENT", "0"}, {"SPEEDTEST_LIBRESPEED_TIMEOUT", "0s"}, {"SPEEDTEST_LIBRESPEED_TIMEOUT", "bad"}} {
		t.Setenv(keyValue[0], keyValue[1])
		if strings.Contains(keyValue[0], "TIMEOUT") {
			t.Setenv("SPEEDTEST_LIBRESPEED_CONCURRENT", "4")
		}
		if _, err := LibreSpeedConfigFromEnv(); err == nil {
			t.Fatalf("accepted %s=%q", keyValue[0], keyValue[1])
		}
	}
	if _, err := NewConfigured("unsupported", 0); err == nil {
		t.Fatal("unsupported engine accepted")
	}
	if _, err := NewConfigured("librespeed", -1); err == nil {
		t.Fatal("negative node parallel_connections accepted")
	}
	t.Setenv("SPEEDTEST_OOKLA_BINARY", "")
	t.Setenv("SPEEDTEST_OOKLA_ARGS", "")
	t.Setenv("SPEEDTEST_OOKLA_TIMEOUT", "")
	for _, name := range []string{"", "ookla"} {
		selected, err := NewConfigured(name, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := selected.(*Ookla); !ok {
			t.Fatalf("NewConfigured(%q) = %T, want *Ookla", name, selected)
		}
	}
	t.Setenv("SPEEDTEST_OOKLA_ARGS", "invalid")
	if _, err := NewConfigured("ookla", 0); err == nil {
		t.Fatal("invalid Ookla environment accepted")
	}
	t.Setenv("SPEEDTEST_LIBRESPEED_CONCURRENT", "invalid")
	if _, err := NewConfigured("librespeed", 0); err == nil {
		t.Fatal("invalid LibreSpeed environment accepted")
	}
}

func TestLibreSpeedRunnerCancellation(t *testing.T) {
	runner, err := NewLibreSpeed(LibreSpeedConfig{Binary: script(t, "sleep 5\n")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, err := runner.Run(ctx, Options{})
	assertClass(t, r, err, Canceled)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestLibreSpeedRunnerSourceIPBinding(t *testing.T) {
	fixturePath := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(fixturePath, librespeedFixture(t), 0o600); err != nil {
		t.Fatal(err)
	}
	argsPath := filepath.Join(t.TempDir(), "args.txt")
	t.Setenv("LIBRESPEED_FIXTURE", fixturePath)
	t.Setenv("LIBRESPEED_ARGS_PATH", argsPath)
	binary := script(t, "printf '%s\\n' \"$@\" > \"$LIBRESPEED_ARGS_PATH\"\ncat \"$LIBRESPEED_FIXTURE\"\n")
	runner, err := NewLibreSpeed(LibreSpeedConfig{Binary: binary, Concurrent: 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), Options{SourceIP: "192.0.2.10"}); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := "--json\n--concurrent\n3\n--source\n192.0.2.10\n"; string(args) != want {
		t.Fatalf("arguments = %q, want %q", args, want)
	}
}

func TestLibreSpeedRunnerRefusesInterfaceAndInvalidSourceBinding(t *testing.T) {
	runner, err := NewLibreSpeed(LibreSpeedConfig{Binary: "/nonexistent/librespeed-cli"})
	if err != nil {
		t.Fatal(err)
	}
	for name, opts := range map[string]Options{
		"interface is unsupported":   {InterfaceName: "eth0"},
		"invalid source address":     {SourceIP: "not-an-ip"},
		"interface plus source":      {InterfaceName: "eth0", SourceIP: "192.0.2.10"},
		"padded source address text": {SourceIP: " 192.0.2.10"},
	} {
		_, err := runner.Run(context.Background(), opts)
		var runErr *RunError
		if !errors.As(err, &runErr) || runErr.Class != InvalidOptions {
			t.Errorf("%s: error = %v, want invalid_options (and no process started)", name, err)
		}
	}
}
