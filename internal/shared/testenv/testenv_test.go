package testenv

import (
	"os"
	"testing"
)

func TestApplyPreservesProcessValuesAndFillsMissingOnes(t *testing.T) {
	t.Setenv("SPEEDTEST_TESTENV_OVERRIDE", "process")
	Apply(t, map[string]string{
		"SPEEDTEST_TESTENV_OVERRIDE": "file",
		"SPEEDTEST_TESTENV_DEFAULT":  "file",
	})
	if got := os.Getenv("SPEEDTEST_TESTENV_OVERRIDE"); got != "process" {
		t.Fatalf("process override = %q", got)
	}
	if got := os.Getenv("SPEEDTEST_TESTENV_DEFAULT"); got != "file" {
		t.Fatalf("file default = %q", got)
	}
}
