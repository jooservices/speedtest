package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestMainProcessHelper(t *testing.T) {
	if os.Getenv("SPEEDTEST_MIGRATE_MAIN_HELPER") != "1" {
		return
	}
	os.Args = []string{"command", "invalid"}
	main()
}

func TestMainProcessExitAndOutput(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestMainProcessHelper$")
	cmd.Env = append(os.Environ(), "SPEEDTEST_MIGRATE_MAIN_HELPER=1")
	output, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Fatalf("exit=%v output=%s", err, output)
	}
	if !strings.Contains(string(output), "usage: migrate up|down") {
		t.Fatalf("unexpected output: %s", output)
	}
}
