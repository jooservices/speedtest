package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func installTree(t *testing.T, root string) map[string]bool {
	t.Helper()
	files := map[string]bool{}
	for _, p := range []string{"/etc/systemd/system/speedtest-agent.service", "/usr/local/bin/speedtest-agent", "/usr/local/bin/speedtest-agent.prev",
		"/etc/speedtest-agent/config.yaml", "/etc/speedtest-agent/credential", "/etc/speedtest-agent/spool/queue.db",
		"/var/lib/speedtest-agent/inbox/request.json", "/var/lib/speedtest-agent-update/state.json"} {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		files[full] = true
	}
	return files
}

func TestUninstallAllRemovesTheAgentAndNothingElse(t *testing.T) {
	root := t.TempDir()
	agent := installTree(t, root)
	// Neighbours that must survive: other programs in the same directories and an unrelated directory.
	keep := []string{"/usr/local/bin/other-tool", "/etc/systemd/system/other.service", "/etc/speedtest-agent-extra/keep", "/var/lib/speedtest-agent-other/keep", "/home/user/speedtest-agent/keep"}
	for _, p := range keep {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"uninstall", "--all", "--root", root}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s %s", code, out.String(), errOut.String())
	}
	for path := range agent {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s was not removed", path)
		}
	}
	for _, p := range keep {
		if _, err := os.Stat(filepath.Join(root, p)); err != nil {
			t.Errorf("%s must not be touched: %v", p, err)
		}
	}
	if !strings.Contains(out.String(), "uninstalled completely") {
		t.Fatalf("output = %q", out.String())
	}
	// Idempotent: a second run on a clean machine succeeds quietly.
	out.Reset()
	if code := run(context.Background(), []string{"uninstall", "--all", "--root", root}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "uninstalled completely") {
		t.Fatalf("second run: exit %d %q %q", code, out.String(), errOut.String())
	}
}

func TestUninstallAllFailuresAndArguments(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"uninstall", "--all", "extra"}, &out, &errOut); code != 2 {
		t.Fatalf("unexpected argument = %d", code)
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := t.TempDir()
	installTree(t, root)
	locked := filepath.Join(root, "etc", "speedtest-agent")
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	errOut.Reset()
	if code := run(context.Background(), []string{"uninstall", "--all", "--root", root}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "remove") {
		t.Fatalf("a directory that cannot be emptied must fail loudly: exit %d %q", code, errOut.String())
	}
	if p := defaultAgentPaths("/r"); p.User != "speedtest-agent" || len(p.Files) != 3 || len(p.Dirs) != 3 || !strings.HasPrefix(p.Files[0], "/r/") {
		t.Fatalf("paths = %+v", p)
	}
}

func TestUninstallAllStopsTheServiceAndRemovesTheAccountOnARealSystem(t *testing.T) {
	var calls []string
	oldLook, oldRun, oldRoot := lookPath, runTool, isRoot
	lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	runTool = func(name string, args ...string) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil
	}
	isRoot = func() bool { return true }
	t.Cleanup(func() { lookPath, runTool, isRoot = oldLook, oldRun, oldRoot })
	root := t.TempDir()
	installTree(t, root)
	var out, errOut bytes.Buffer
	if !removeEverything(root, true, &out, &errOut, false) {
		t.Fatalf("failed: %s", errOut.String())
	}
	want := []string{"/usr/bin/systemctl disable --now speedtest-agent", "/usr/bin/systemctl daemon-reload", "id speedtest-agent", "/usr/bin/userdel speedtest-agent"}
	if strings.Join(calls, "|") != strings.Join(want, "|") || !strings.Contains(out.String(), "removed account speedtest-agent") {
		t.Fatalf("calls = %v output %q", calls, out.String())
	}
	calls = nil
	out.Reset()
	if !removeEverything(root, true, &out, &errOut, true) || strings.Contains(strings.Join(calls, "|"), "userdel") {
		t.Fatalf("--keep-user must keep the account: %v", calls)
	}
	isRoot = func() bool { return false }
	calls = nil
	removeEverything(root, true, &out, &errOut, false)
	if strings.Contains(strings.Join(calls, "|"), "userdel") {
		t.Fatal("only root removes the account")
	}
	lookPath = func(string) (string, error) { return "", os.ErrNotExist }
	calls = nil
	if !removeEverything(root, true, &out, &errOut, false) || len(calls) != 0 {
		t.Fatalf("a system without systemd is not an error: %v", calls)
	}
}
