package runloop

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/agent/control"
	"github.com/jooservices/speedtest/internal/agent/engine"
	"github.com/jooservices/speedtest/internal/agent/netpath"
	"github.com/jooservices/speedtest/internal/agent/sched"
	"github.com/jooservices/speedtest/internal/agent/testutil"
)

type pinnedFailureEngine struct{ calls []engine.Options }

func (e *pinnedFailureEngine) Run(_ context.Context, options engine.Options) (engine.Result, error) {
	e.calls = append(e.calls, options)
	if options.ServerID > 0 {
		return engine.Result{}, &engine.RunError{Class: engine.ServerUnavailable}
	}
	return engine.Result{ExternalIP: "198.51.100.7"}, nil
}

type serverListingEngine struct {
	options engine.Options
	servers []engine.ServerOption
	err     error
}

func (*serverListingEngine) Run(context.Context, engine.Options) (engine.Result, error) {
	return engine.Result{}, nil
}

func (e *serverListingEngine) ListServers(_ context.Context, options engine.Options) ([]engine.ServerOption, error) {
	e.options = options
	return e.servers, e.err
}

func TestPinnedServerFailureRetriesAutomaticallyWithWarning(t *testing.T) {
	center := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"code":200,"data":{"ip":"198.51.100.7"}}`))
	}))
	defer center.Close()
	measurement := &pinnedFailureEngine{}
	runner, err := netpath.NewRunner(measurement, center.URL, "test-token", netpath.Binding{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runWithServerFallback(t.Context(), runner, 54321)
	if err != nil || len(measurement.calls) != 2 || measurement.calls[0].ServerID != 54321 || measurement.calls[1].ServerID != 0 {
		t.Fatalf("fallback calls=%+v err=%v", measurement.calls, err)
	}
	if result.Measurement.ExternalIP != "198.51.100.7" || result.PublicIP != "198.51.100.7" || len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "automatically") {
		t.Fatalf("fallback result=%+v", result)
	}
}

type endpointFallbackEngine struct{}

func (endpointFallbackEngine) Run(_ context.Context, options engine.Options) (engine.Result, error) {
	if options.ServerID > 0 {
		return engine.Result{}, &engine.RunError{Class: engine.ServerUnavailable}
	}
	return engine.Result{ExternalIP: "198.51.100.7", Server: &engine.Server{
		Name: "Second endpoint", Host: "shared.example.test", Endpoint: "https://shared.example.test/b",
	}}, nil
}

func (endpointFallbackEngine) ListServers(context.Context, engine.Options) ([]engine.ServerOption, error) {
	return []engine.ServerOption{
		{ID: 51, Name: "First endpoint", Host: "shared.example.test", Endpoint: "https://shared.example.test/a"},
		{ID: 52, Name: "Second endpoint", Host: "shared.example.test", Endpoint: "https://shared.example.test/b"},
	}, nil
}

func TestPinnedFallbackResolvesServerByFullEndpoint(t *testing.T) {
	center := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"code":200,"data":{"ip":"198.51.100.7"}}`))
	}))
	defer center.Close()
	runner, err := netpath.NewRunner(endpointFallbackEngine{}, center.URL, "test-token", netpath.Binding{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := runWithServerFallback(t.Context(), runner, 99)
	if err != nil || out.Measurement.Server == nil || out.Measurement.Server.ID != 52 || out.Measurement.Server.Endpoint != "https://shared.example.test/b" {
		t.Fatalf("fallback identity=%+v err=%v", out.Measurement.Server, err)
	}
}

func TestPinnedServerFallbackIsLimitedToUnavailableFailures(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		class engine.ErrorClass
		id    int64
		want  bool
	}{{engine.ServerUnavailable, 12, true}, {engine.NoRoute, 12, true}, {engine.Timeout, 12, true}, {engine.ServerUnavailable, 0, false}} {
		if got := retryAutomaticServer(ctx, &engine.RunError{Class: tc.class}, tc.id); got != tc.want {
			t.Errorf("retryAutomaticServer(%s,%d)=%v", tc.class, tc.id, got)
		}
	}
	if retryAutomaticServer(ctx, nil, 12) || retryAutomaticServer(ctx, errors.New("network failure"), 12) {
		t.Fatal("retried without a server availability error")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if retryAutomaticServer(canceled, &engine.RunError{Class: engine.Timeout}, 12) {
		t.Fatal("retried after caller cancellation")
	}
}

func TestPinnedServerCLIReportedTimeoutRetriesAutomatically(t *testing.T) {
	fixture, err := filepath.Abs("../engine/testdata/legacy.json")
	if err != nil {
		t.Fatal(err)
	}
	argsLog := filepath.Join(t.TempDir(), "args.log")
	script := "#!/bin/sh\necho \"$*\" >> " + shellQuote(argsLog) + "\ncase \"$*\" in\n*'--server-id 54321'*) echo 'Operation timed out' >&2; exit 1;;\n*) cat " + shellQuote(fixture) + ";;\nesac\n"
	runner, closeCenter := newOoklaFallbackRunner(t, writeFallbackEngine(t, script), time.Second)
	defer closeCenter()
	out, err := runWithServerFallbackTimeout(t.Context(), runner, 54321, 250*time.Millisecond)
	if err != nil || out.Measurement.Server == nil || out.Measurement.Server.ID != 37471 || !strings.Contains(strings.Join(out.Warnings, " "), "automatically") {
		t.Fatalf("CLI timeout did not fall back: result=%+v err=%v", out, err)
	}
	if got := strings.Count(readFallbackArgs(t, argsLog), "\n"); got != 2 {
		t.Fatalf("engine invocations=%d, want pinned attempt and one automatic retry", got)
	}
}

func TestPinnedServerEngineTimeoutRetriesWithBoundedAutomaticRun(t *testing.T) {
	fixture, err := filepath.Abs("../engine/testdata/legacy.json")
	if err != nil {
		t.Fatal(err)
	}
	argsLog := filepath.Join(t.TempDir(), "args.log")
	script := "#!/bin/sh\necho \"$*\" >> " + shellQuote(argsLog) + "\ncase \"$*\" in\n*'--server-id 54321'*) exec sleep 1;;\n*) cat " + shellQuote(fixture) + ";;\nesac\n"
	runner, closeCenter := newOoklaFallbackRunner(t, writeFallbackEngine(t, script), 25*time.Millisecond)
	defer closeCenter()
	out, err := runWithServerFallbackTimeout(t.Context(), runner, 54321, 250*time.Millisecond)
	if err != nil || out.Measurement.Server == nil || out.Measurement.Server.ID != 37471 || !strings.Contains(strings.Join(out.Warnings, " "), "automatically") {
		t.Fatalf("timed-out pinned run did not fall back: result=%+v err=%v", out, err)
	}
	if got := strings.Count(readFallbackArgs(t, argsLog), "\n"); got != 2 {
		t.Fatalf("engine invocations=%d, want pinned attempt and one automatic retry", got)
	}
}

func TestPinnedServerTimeoutDoesNotRetryCallerDeadline(t *testing.T) {
	argsLog := filepath.Join(t.TempDir(), "args.log")
	script := "#!/bin/sh\necho \"$*\" >> " + shellQuote(argsLog) + "\nexec sleep 1\n"
	runner, closeCenter := newOoklaFallbackRunner(t, writeFallbackEngine(t, script), time.Second)
	defer closeCenter()
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	_, err := runWithServerFallbackTimeout(ctx, runner, 54321, 250*time.Millisecond)
	var runErr *engine.RunError
	if !errors.As(err, &runErr) || runErr.Class != engine.Timeout {
		t.Fatalf("caller deadline error=%v, want engine timeout", err)
	}
	if got := strings.Count(readFallbackArgs(t, argsLog), "\n"); got != 1 {
		t.Fatalf("engine invocations after caller deadline=%d, want 1", got)
	}
}

func TestAutomaticRetryHasItsOwnDeadline(t *testing.T) {
	argsLog := filepath.Join(t.TempDir(), "args.log")
	script := "#!/bin/sh\necho \"$*\" >> " + shellQuote(argsLog) + "\ncase \"$*\" in\n*'--server-id 54321'*) echo 'server unavailable' >&2; exit 1;;\n*) exec sleep 1;;\nesac\n"
	runner, closeCenter := newOoklaFallbackRunner(t, writeFallbackEngine(t, script), time.Second)
	defer closeCenter()
	started := time.Now()
	_, err := runWithServerFallbackTimeout(t.Context(), runner, 54321, 100*time.Millisecond)
	var runErr *engine.RunError
	if !errors.As(err, &runErr) || runErr.Class != engine.Timeout || time.Since(started) > time.Second {
		t.Fatalf("automatic run exceeded its own deadline: err=%v elapsed=%s", err, time.Since(started))
	}
	if got := strings.Count(readFallbackArgs(t, argsLog), "\n"); got != 2 {
		t.Fatalf("engine invocations=%d, want one pinned attempt and one bounded retry", got)
	}
}

func writeFallbackEngine(t *testing.T, script string) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "speedtest")
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return binary
}

func newOoklaFallbackRunner(t *testing.T, binary string, timeout time.Duration) (*netpath.Runner, func()) {
	t.Helper()
	measurement, err := engine.NewOokla(engine.Config{Binary: binary, Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	center := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"code":200,"data":{"ip":"198.51.100.7"}}`))
	}))
	runner, err := netpath.NewRunner(measurement, center.URL, "test-token", netpath.Binding{})
	if err != nil {
		center.Close()
		t.Fatal(err)
	}
	return runner, center.Close
}

func readFallbackArgs(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }

func TestListServersUsesConfiguredEngineAndNodeBinding(t *testing.T) {
	wantServers := []engine.ServerOption{{ID: 12, Name: "Test", City: "Hanoi"}}
	for _, tc := range []struct {
		name    string
		binding netpath.Binding
		wantIP  string
	}{
		{name: "default route"},
		{name: "source IP", binding: netpath.Binding{SourceIP: "127.0.0.1"}, wantIP: "127.0.0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lister := &serverListingEngine{servers: wantServers}
			loop := &Loop{engineConfigs: map[string]measurementConfig{"ookla": {engine: lister, binding: tc.binding}}}
			got, err := loop.listServers(t.Context(), "ookla")
			if err != nil || !reflect.DeepEqual(got, wantServers) || lister.options.SourceIP != tc.wantIP {
				t.Fatalf("servers=%+v options=%+v err=%v", got, lister.options, err)
			}
		})
	}
}

func TestListServersRejectsUnsupportedEngineListerAndBinding(t *testing.T) {
	loop := &Loop{}
	if _, err := loop.listServers(t.Context(), "unknown"); err == nil {
		t.Fatal("accepted unsupported engine")
	}
	loop.engineConfigs = map[string]measurementConfig{"ookla": {engine: &pinnedFailureEngine{}}}
	if _, err := loop.listServers(t.Context(), "ookla"); err == nil {
		t.Fatal("accepted engine without server discovery")
	}
	loop.engineConfigs["ookla"] = measurementConfig{engine: &serverListingEngine{}, binding: netpath.Binding{Interface: "interface-does-not-exist"}}
	if _, err := loop.listServers(t.Context(), "ookla"); err == nil {
		t.Fatal("accepted unavailable network interface")
	}
}

func TestListServersBuildsConfiguredEngineFromEnvironment(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "speedtest")
	script := "#!/bin/sh\ncat <<'SERVERS'\n{\"servers\":[{\"id\":12,\"name\":\"Test server\",\"location\":\"Hanoi\",\"sponsor\":\"Test ISP\",\"host\":\"test.example\",\"distance\":5.5}]}\nSERVERS\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPEEDTEST_OOKLA_BINARY", binary)
	t.Setenv("SPEEDTEST_OOKLA_ARGS", "[]")
	t.Setenv("SPEEDTEST_OOKLA_TIMEOUT", "1s")
	loop := &Loop{}
	servers, err := loop.listServers(t.Context(), "ookla")
	if err != nil || len(servers) != 1 || servers[0].ID != 12 || servers[0].DistanceKM == nil || *servers[0].DistanceKM != 5.5 {
		t.Fatalf("servers=%+v err=%v", servers, err)
	}
}

func TestConfiguredServerAcceptsOnlyAutomaticOrPositiveIDs(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int64
		bad  bool
	}{
		{raw: `"auto"`},
		{raw: `12`, want: 12},
		{raw: `0`, bad: true},
		{raw: `-1`, bad: true},
		{raw: `"12"`, bad: true},
		{raw: `true`, bad: true},
		{raw: `{`, bad: true},
	} {
		got, err := configuredServer(json.RawMessage(tc.raw))
		if (err != nil) != tc.bad || got != tc.want {
			t.Errorf("configuredServer(%s) = %d, %v", tc.raw, got, err)
		}
	}
}

func TestInstalledEnginesUsesExecutableCapabilities(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "cli")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPEEDTEST_OOKLA_BINARY", binary)
	t.Setenv("SPEEDTEST_LIBRESPEED_BINARY", binary)
	if got := installedEngines(); !reflect.DeepEqual(got, []string{"ookla", "librespeed"}) {
		t.Fatal(got)
	}
	t.Setenv("SPEEDTEST_OOKLA_BINARY", filepath.Join(dir, "missing"))
	t.Setenv("SPEEDTEST_LIBRESPEED_BINARY", filepath.Join(dir, "missing"))
	if got := installedEngines(); len(got) != 0 {
		t.Fatal(got)
	}
	t.Setenv("SPEEDTEST_OOKLA_TIMEOUT", "bad")
	t.Setenv("SPEEDTEST_LIBRESPEED_TIMEOUT", "bad")
	if got := installedEngines(); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestConfiguredEnginesKeepIndependentSchedules(t *testing.T) {
	cfg := measurementConfig{name: "ookla", threads: 4}
	base := sched.Schedule{Interval: 30 * time.Minute, Enabled: true}
	inputs := []control.ScheduleInput{{Engine: "ookla", Interval: 30, Enabled: true, ServerID: json.RawMessage(`54321`)}, {Engine: "librespeed", Interval: 60, Offset: 5, Enabled: true, ServerID: json.RawMessage(`"auto"`)}}
	configs, schedules, err := configuredEngines(inputs, cfg, base)
	if err != nil || len(configs) != 2 || len(schedules) != 2 || schedules[1].Interval != time.Hour || schedules[1].Offset != 5*time.Minute || configs["librespeed"].name != "librespeed" || configs["ookla"].server != 54321 || configs["librespeed"].server != 0 {
		t.Fatal(configs, schedules, err)
	}
	for _, input := range [][]control.ScheduleInput{{{Engine: "bad"}}, {{Engine: "ookla", Interval: 30}, {Engine: "ookla", Interval: 30}}, {{Engine: "ookla", Interval: 0}}} {
		if _, _, err := configuredEngines(input, cfg, base); err == nil {
			t.Fatal("accepted", input)
		}
	}
	t.Setenv("SPEEDTEST_LIBRESPEED_TIMEOUT", "bad")
	if _, _, err := configuredEngines(inputs, cfg, base); err == nil {
		t.Fatal("accepted invalid engine config")
	}
}

func TestScheduledEngineProcessesTagDurableResults(t *testing.T) {
	cfg := bootstrap(t)
	data, err := os.ReadFile("../engine/testdata/librespeed.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture map[string]any
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	fixture["timestamp"] = time.Now().UTC().Format(time.RFC3339)
	data, _ = json.Marshal(fixture)
	path := filepath.Join(t.TempDir(), "libre")
	if err = os.WriteFile(path, []byte("#!/bin/sh\ncat <<'RESULT'\n"+string(data)+"\nRESULT\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPEEDTEST_LIBRESPEED_BINARY", path)
	center := testutil.NewCenter()
	defer center.Close()
	loop, err := New(cfg, parseURL(t, center.URL), "test-credential", "test", quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = loop.Close() }()
	remote := control.AgentConfig{Engine: "ookla", ServerSelectionSnapshots: true, Schedules: []control.ScheduleInput{{Engine: "ookla", Interval: 30, Enabled: true}, {Engine: "librespeed", Interval: 60, Enabled: true}}}
	if err = loop.applyConfig(remote, sched.Schedule{Interval: 30 * time.Minute, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ookla", "librespeed"} {
		if err = loop.RunEngine(t.Context(), name); err != nil {
			t.Fatal(name, err)
		}
	}
	rows, err := loop.spool.Peek(10)
	if err != nil || len(rows) != 2 {
		t.Fatal(rows, err)
	}
	engines := map[string]bool{}
	for _, row := range rows {
		engines[row.Engine] = true
		if !row.ServerSelectionKnown || row.PinnedServerID != nil {
			t.Fatalf("automatic selection snapshot=%+v", row)
		}
		wire, _ := json.Marshal(row)
		var parsed map[string]any
		if err = json.Unmarshal(wire, &parsed); err != nil || parsed["engine"] != row.Engine || parsed["serverSelectionKnown"] != true {
			t.Fatal(parsed, err)
		}
	}
	if !engines["ookla"] || !engines["librespeed"] {
		t.Fatal(engines)
	}
	if err = loop.RunEngine(t.Context(), "removed"); err == nil {
		t.Fatal("removed schedule executed")
	}
}
