package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestMainProcessHelper(t *testing.T) {
	if os.Getenv("SPEEDTEST_CENTER_MAIN_HELPER") != "1" {
		return
	}
	os.Args = []string{"command", "-h"}
	main()
}

func TestMainProcessExitAndOutput(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestMainProcessHelper$")
	cmd.Env = append(os.Environ(), "SPEEDTEST_CENTER_MAIN_HELPER=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("help process failed: %v %s", err, output)
	}
	if !strings.Contains(string(output), "serve UI files") {
		t.Fatalf("unexpected output: %s", output)
	}
}
