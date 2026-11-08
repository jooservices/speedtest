package runloop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/agent/control"
	"github.com/jooservices/speedtest/internal/agent/sched"
	"github.com/jooservices/speedtest/internal/agent/testutil"
)

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
	inputs := []control.ScheduleInput{{Engine: "ookla", Interval: 30, Enabled: true}, {Engine: "librespeed", Interval: 60, Offset: 5, Enabled: true}}
	configs, schedules, err := configuredEngines(inputs, cfg, base)
	if err != nil || len(configs) != 2 || len(schedules) != 2 || schedules[1].Interval != time.Hour || schedules[1].Offset != 5*time.Minute || configs["librespeed"].name != "librespeed" {
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
	remote := control.AgentConfig{Engine: "ookla", Schedules: []control.ScheduleInput{{Engine: "ookla", Interval: 30, Enabled: true}, {Engine: "librespeed", Interval: 60, Enabled: true}}}
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
		wire, _ := json.Marshal(row)
		var parsed map[string]any
		if err = json.Unmarshal(wire, &parsed); err != nil || parsed["engine"] != row.Engine {
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
