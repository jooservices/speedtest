package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func speedtestCLIFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/speedtestcli.json")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestParseSpeedtestCLIRealShape(t *testing.T) {
	r, err := ParseSpeedtestCLI(speedtestCLIFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := *r.Down; got < 94.39 || got > 94.40 {
		t.Fatalf("download = %v Mbps, want 94.39 (bits per second divided by 1e6)", got)
	}
	if got := *r.Up; got < 12.34 || got > 12.35 {
		t.Fatalf("upload = %v Mbps", got)
	}
	if *r.Ping != 18.42 {
		t.Fatalf("ping = %v", *r.Ping)
	}
	if r.Jitter != nil || r.Loss != nil || r.LoadedDown != nil || r.LoadedUp != nil {
		t.Fatal("measurements speedtest-cli does not report must stay unavailable, not zero")
	}
	if r.BytesDown != 118489088 || r.BytesUp != 15728640 {
		t.Fatalf("bytes = %d down, %d up", r.BytesDown, r.BytesUp)
	}
	if r.Server == nil || r.Server.ID != 12345 || r.Server.Name != "Example Telecom" || r.Server.Location != "Hanoi, Vietnam" || r.Server.Host != "speedtest.example.test:8080" {
		t.Fatalf("server = %+v", r.Server)
	}
	if r.ExternalIP != "203.0.113.57" || r.ISP != "Example ISP" {
		t.Fatalf("client = %q %q", r.ExternalIP, r.ISP)
	}
	if !r.Timestamp.Equal(time.Date(2026, 11, 3, 2, 30, 15, 123456000, time.UTC)) {
		t.Fatalf("timestamp = %v", r.Timestamp)
	}
	if len(r.Raw) == 0 {
		t.Fatal("raw output must be kept")
	}
}

func TestParseSpeedtestCLIVariants(t *testing.T) {
	r, err := ParseSpeedtestCLI([]byte(`{"download":1000000,"upload":500000,"ping":5,"timestamp":"2026-11-03T02:30:15Z","bytes_sent":1,"bytes_received":2,"share":"https://www.speedtest.net/result/1.png","server":{"id":77,"name":"X","sponsor":"","host":"h:8080","country":""},"client":{"ip":"not-an-ip","isp":""}}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.Server.ID != 77 || r.Server.Name != "X" || r.Server.Location != "X" || r.ExternalIP != "" || r.ResultURL != "https://www.speedtest.net/result/1.png" {
		t.Fatalf("result = %+v server=%+v", r, r.Server)
	}
	noServer, err := ParseSpeedtestCLI([]byte(`{"download":1,"upload":1,"ping":1,"timestamp":"2026-11-03T02:30:15Z","bytes_sent":1,"bytes_received":1,"server":{"id":"abc"}}`))
	if err != nil || noServer.Server != nil {
		t.Fatalf("a report without server identity: server = %+v err = %v", noServer.Server, err)
	}
}

func TestParseSpeedtestCLIRejectsInvalidReports(t *testing.T) {
	base := `"timestamp":"2026-11-03T02:30:15Z","bytes_sent":1,"bytes_received":1`
	for name, raw := range map[string]string{
		"empty":            ``,
		"not json":         `speedtest failed`,
		"array":            `[{"download":1}]`,
		"missing download": `{"upload":1,"ping":1,` + base + `}`,
		"missing upload":   `{"download":1,"ping":1,` + base + `}`,
		"missing ping":     `{"download":1,"upload":1,` + base + `}`,
		"missing time":     `{"download":1,"upload":1,"ping":1,"bytes_sent":1,"bytes_received":1}`,
		"zero time":        `{"download":1,"upload":1,"ping":1,"timestamp":"0001-01-01T00:00:00Z","bytes_sent":1,"bytes_received":1}`,
		"missing bytes":    `{"download":1,"upload":1,"ping":1,"timestamp":"2026-11-03T02:30:15Z"}`,
		"negative speed":   `{"download":-1,"upload":1,"ping":1,` + base + `}`,
		"negative ping":    `{"download":1,"upload":1,"ping":-1,` + base + `}`,
		"huge bytes":       `{"download":1,"upload":1,"ping":1,"timestamp":"2026-11-03T02:30:15Z","bytes_sent":18446744073709551615,"bytes_received":1}`,
		"wrong type":       `{"download":"fast","upload":1,"ping":1,` + base + `}`,
	} {
		_, err := ParseSpeedtestCLI([]byte(raw))
		var runErr *RunError
		if !errors.As(err, &runErr) || runErr.Class != InvalidResult {
			t.Errorf("%s: error = %v, want invalid_result", name, err)
		}
	}
	if _, err := ParseSpeedtestCLI([]byte(strings.Repeat(" ", maxOutput+1))); err == nil {
		t.Error("oversized output accepted")
	}
}

func fakeSpeedtestCLI(t *testing.T, argsPath string) string {
	t.Helper()
	fixture := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(fixture, speedtestCLIFixture(t), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPEEDTESTCLI_FIXTURE", fixture)
	t.Setenv("SPEEDTESTCLI_ARGS_PATH", argsPath)
	return script(t, "printf '%s\\n' \"$@\" > \"$SPEEDTESTCLI_ARGS_PATH\"\ncat \"$SPEEDTESTCLI_FIXTURE\"\n")
}

func TestSpeedtestCLIRunnerArgumentsAndBinding(t *testing.T) {
	argsPath := filepath.Join(t.TempDir(), "args.txt")
	runner, err := NewSpeedtestCLI(SpeedtestCLIConfig{Binary: fakeSpeedtestCLI(t, argsPath)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if args, _ := os.ReadFile(argsPath); string(args) != "--json\n--secure\n" {
		t.Fatalf("default arguments = %q", args)
	}
	r, err := runner.Run(context.Background(), Options{ServerID: 999, SourceIP: "192.0.2.10"})
	if err != nil {
		t.Fatal(err)
	}
	if args, _ := os.ReadFile(argsPath); string(args) != "--json\n--secure\n--server\n999\n--source\n192.0.2.10\n" {
		t.Fatalf("pinned and bound arguments = %q", args)
	}
	if r.Server.ID != 12345 {
		t.Fatalf("an id reported by the engine wins, got %d", r.Server.ID)
	}
	noID := script(t, `printf '%s' '{"download":1000000,"upload":1000000,"ping":1,"timestamp":"2026-11-03T02:30:15Z","bytes_sent":1,"bytes_received":1,"server":{"sponsor":"S","host":"h"}}'`)
	pinned, _ := NewSpeedtestCLI(SpeedtestCLIConfig{Binary: noID})
	r, err = pinned.Run(context.Background(), Options{ServerID: 42})
	if err != nil || r.Server.ID != 42 {
		t.Fatalf("pinned id fallback: %+v err=%v", r.Server, err)
	}
}

func TestSpeedtestCLIRunnerRefusesInvalidOptions(t *testing.T) {
	runner, err := NewSpeedtestCLI(SpeedtestCLIConfig{Binary: "/nonexistent/speedtest-cli"})
	if err != nil {
		t.Fatal(err)
	}
	for name, opts := range map[string]Options{
		"interface unsupported": {InterfaceName: "eth0"},
		"bad source":            {SourceIP: "nope"},
		"negative server":       {ServerID: -1},
		"negative timeout":      {Timeout: -time.Second},
	} {
		_, err := runner.Run(context.Background(), opts)
		var runErr *RunError
		if !errors.As(err, &runErr) || runErr.Class != InvalidOptions {
			t.Errorf("%s: error = %v, want invalid_options", name, err)
		}
	}
}

func TestSpeedtestCLIRunnerFailures(t *testing.T) {
	missing, _ := NewSpeedtestCLI(SpeedtestCLIConfig{Binary: filepath.Join(t.TempDir(), "absent")})
	if _, err := missing.Run(context.Background(), Options{}); !isClass(err, BinaryMissing) {
		t.Fatalf("missing binary: %v", err)
	}
	for name, tc := range map[string]struct {
		body  string
		class ErrorClass
	}{
		"server unreachable": {"echo 'Cannot retrieve speedtest configuration' >&2; exit 1", Execution},
		"no route":           {"echo 'ERROR: network is unreachable' >&2; exit 1", NoRoute},
		"json with error":    {`printf '%s' '{"error":"x"}'; exit 1`, Execution},
		"garbage":            {"echo not-json", InvalidResult},
		"overflow":           {"head -c 70000 /dev/zero | tr '\\0' 'a'", InvalidResult},
	} {
		runner, _ := NewSpeedtestCLI(SpeedtestCLIConfig{Binary: script(t, tc.body)})
		if _, err := runner.Run(context.Background(), Options{}); !isClass(err, tc.class) {
			t.Errorf("%s: error = %v, want %s", name, err, tc.class)
		}
	}
}

func TestSpeedtestCLIRunnerTimeoutAndCancel(t *testing.T) {
	slow := script(t, "sleep 5")
	runner, _ := NewSpeedtestCLI(SpeedtestCLIConfig{Binary: slow, Timeout: 100 * time.Millisecond})
	if _, err := runner.Run(context.Background(), Options{}); !isClass(err, Timeout) {
		t.Fatalf("timeout: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runner, _ = NewSpeedtestCLI(SpeedtestCLIConfig{Binary: slow})
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	if _, err := runner.Run(ctx, Options{}); !isClass(err, Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := runner.Run(context.Background(), Options{Timeout: 50 * time.Millisecond}); !isClass(err, Timeout) {
		t.Fatalf("per-run timeout: %v", err)
	}
}

func TestSpeedtestCLIConfig(t *testing.T) {
	t.Setenv("SPEEDTEST_SPEEDTESTCLI_BINARY", "/usr/bin/speedtest-cli")
	t.Setenv("SPEEDTEST_SPEEDTESTCLI_TIMEOUT", "45s")
	c, err := SpeedtestCLIConfigFromEnv()
	if err != nil || c.Binary != "/usr/bin/speedtest-cli" || c.Timeout != 45*time.Second {
		t.Fatalf("config = %+v err=%v", c, err)
	}
	for _, bad := range []string{"soon", "0s", "-1s"} {
		t.Setenv("SPEEDTEST_SPEEDTESTCLI_TIMEOUT", bad)
		if _, err := SpeedtestCLIConfigFromEnv(); err == nil {
			t.Errorf("timeout %q accepted", bad)
		}
	}
	if _, err := NewSpeedtestCLI(SpeedtestCLIConfig{Timeout: -1}); err == nil {
		t.Error("negative timeout accepted")
	}
	defaults, err := NewSpeedtestCLI(SpeedtestCLIConfig{})
	if err != nil || defaults.config.Binary != "speedtest-cli" || defaults.config.Timeout != DefaultTimeout {
		t.Fatalf("defaults = %+v err=%v", defaults, err)
	}
	t.Setenv("SPEEDTEST_SPEEDTESTCLI_TIMEOUT", "")
	e, err := NewConfigured("speedtestcli", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := e.(*SpeedtestCLI); !ok {
		t.Fatalf("NewConfigured returned %T", e)
	}
	t.Setenv("SPEEDTEST_SPEEDTESTCLI_TIMEOUT", "bad")
	if _, err := NewConfigured("speedtestcli", 0); err == nil {
		t.Error("NewConfigured accepted a bad timeout")
	}
}

func isClass(err error, class ErrorClass) bool {
	var runErr *RunError
	return errors.As(err, &runErr) && runErr.Class == class
}
