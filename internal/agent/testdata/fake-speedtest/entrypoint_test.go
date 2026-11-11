package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestMainProcessHelper(t *testing.T) {
	if os.Getenv("SPEEDTEST_FAKE_MAIN_HELPER") != "1" {
		return
	}
	os.Args = []string{"fake-speedtest", "--scenario", "crash"}
	main()
}

func TestLibreSpeedFixtureOutputAndPinnedFallback(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if status := run(context.Background(), []string{"--list"}, &stdout, &stderr); status != 0 || stdout.String() != libreSpeedServerList {
		t.Fatalf("captured LibreSpeed list status=%d output=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if status := run(context.Background(), []string{"--json", "--server", "52"}, &stdout, &stderr); status != 0 || stdout.String() != "null\n" || stderr.Len() != 0 {
		t.Fatalf("pinned LibreSpeed server status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if status := run(context.Background(), []string{"--json"}, &stdout, &stderr); status != 0 {
		t.Fatalf("automatic LibreSpeed status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	var result struct {
		Server struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"server"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || !strings.Contains(result.Server.Name, "Hanoi") || !strings.Contains(result.Server.URL, "hn.example.test") {
		t.Fatalf("automatic LibreSpeed server=%+v err=%v", result.Server, err)
	}
}

func TestMainProcessReportsEngineCrash(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestMainProcessHelper$")
	cmd.Env = append(os.Environ(), "SPEEDTEST_FAKE_MAIN_HELPER=1")
	out, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 2 {
		t.Fatalf("exit=%v output=%s", err, out)
	}
	if !strings.Contains(string(out), "simulated engine crash") {
		t.Fatalf("missing crash diagnostic: %s", out)
	}
}
