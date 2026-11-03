package installer

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The removal script runs as root on the owner's machine, so it is exercised for real here, but only inside a scratch
// directory (AGENT_ROOT) with fake service tools.
func TestUninstallScriptRemovesOnlyTheAgent(t *testing.T) {
	root := t.TempDir()
	tools := filepath.Join(root, "tools")
	if err := os.MkdirAll(tools, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"systemctl", "userdel", "id"} {
		if err := os.WriteFile(filepath.Join(tools, name), []byte("#!/bin/sh\necho unexpected call to "+name+" >&2\nexit 9\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	touch := func(path string) {
		t.Helper()
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	agent := []string{"etc/systemd/system/speedtest-agent.service", "usr/local/bin/speedtest-agent", "usr/local/bin/speedtest-agent.prev", "etc/speedtest-agent/credential",
		"etc/speedtest-agent/spool/q.db", "var/lib/speedtest-agent/inbox/request.json", "var/lib/speedtest-agent-update/state.json"}
	keep := []string{"usr/local/bin/other", "etc/systemd/system/other.service", "etc/speedtest-agent-extra/keep", "var/lib/speedtest-agent-other/keep", "home/me/speedtest-agent/keep"}
	for _, p := range append(append([]string{}, agent...), keep...) {
		touch(p)
	}
	run := func() (string, error) {
		cmd := exec.Command("/bin/sh", "-s")
		cmd.Env = []string{"PATH=" + tools + ":/usr/bin:/bin", "AGENT_ROOT=" + root}
		cmd.Stdin = strings.NewReader(uninstallRemoteScript)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		return out.String(), err
	}
	out, err := run()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	if strings.Contains(out, "unexpected call") {
		t.Fatalf("with AGENT_ROOT set no system tool may run: %s", out)
	}
	var stages []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		stage, pct, ok := parseProgress(line)
		if !ok {
			t.Fatalf("line %q is not a progress line", line)
		}
		stages = append(stages, stage)
		_ = pct
	}
	if strings.Join(stages, ",") != "stop,files,account,removed" {
		t.Fatalf("stages = %v", stages)
	}
	for _, p := range agent {
		if _, err := os.Stat(filepath.Join(root, p)); !os.IsNotExist(err) {
			t.Errorf("%s survived", p)
		}
	}
	for _, p := range keep {
		if _, err := os.Stat(filepath.Join(root, p)); err != nil {
			t.Errorf("%s must be untouched: %v", p, err)
		}
	}
	if out, err = run(); err != nil {
		t.Fatalf("second run on a clean machine: %v\n%s", err, out)
	}
}

func TestUninstallScriptTouchesOnlyAgentPaths(t *testing.T) {
	allowed := []string{"speedtest-agent", "speedtest-agent.service", "speedtest-agent.prev", "speedtest-agent-update"}
	for _, line := range strings.Split(uninstallRemoteScript, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "rm ") {
			continue
		}
		for _, field := range strings.Fields(line) {
			if !strings.HasPrefix(field, `"$ROOT/`) {
				continue
			}
			ok := false
			for _, name := range allowed {
				if strings.HasSuffix(strings.Trim(field, `"`), "/"+name) {
					ok = true
				}
			}
			if !ok {
				t.Errorf("rm target %s is not an agent path", field)
			}
		}
		if strings.Contains(line, " / ") || strings.Contains(line, "*") {
			t.Errorf("unsafe rm line: %s", line)
		}
	}
	if generatedMessage("stop") == "" || generatedMessage("files") == "" || generatedMessage("account") == "" || generatedMessage("removed") == "" {
		t.Fatal("every removal stage has a plain-language message")
	}
}
