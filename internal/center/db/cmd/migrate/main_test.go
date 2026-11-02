package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunUsageAndConfigurationErrors(t *testing.T) {
	for _, args := range [][]string{nil, {"bad"}, {"up", "extra"}} {
		var out bytes.Buffer
		if run(args, &out) != 1 || !strings.Contains(out.String(), "usage:") {
			t.Fatal("expected usage error")
		}
	}
	dir := t.TempDir()
	t.Chdir(dir)
	var out bytes.Buffer
	if run([]string{"up"}, &out) != 1 || !strings.Contains(out.String(), "cannot load .env") {
		t.Fatal("expected missing file error")
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("SPEEDTEST_DB_PORT=bad\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPEEDTEST_DB_PORT", "bad")
	out.Reset()
	if run([]string{"up"}, &out) != 1 || !strings.Contains(out.String(), "invalid migration configuration") {
		t.Fatal("expected invalid config error")
	}
}
