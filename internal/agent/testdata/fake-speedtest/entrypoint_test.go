package main

import (
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
