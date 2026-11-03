package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// These are replaced in tests so that no real service or account is ever touched.
var (
	lookPath = exec.LookPath
	runTool  = func(name string, args ...string) error { return exec.Command(name, args...).Run() }
	isRoot   = func() bool { return os.Geteuid() == 0 }
)

// agentPaths are the only places "agent uninstall --all" may delete. Nothing outside this list is ever touched.
type agentPaths struct {
	Files []string // the service unit, the binary and its previous version
	Dirs  []string // configuration, working state and the self-update state
	User  string   // the dedicated account created by the installer
}

func defaultAgentPaths(root string) agentPaths {
	join := func(p string) string { return filepath.Join(root, p) }
	return agentPaths{
		Files: []string{join("/etc/systemd/system/speedtest-agent.service"), join("/usr/local/bin/speedtest-agent"), join("/usr/local/bin/speedtest-agent.prev")},
		Dirs:  []string{join("/etc/speedtest-agent"), join("/var/lib/speedtest-agent"), join("/var/lib/speedtest-agent-update")},
		User:  "speedtest-agent",
	}
}

// removeEverything deletes the service, binary, configuration, credential, spool and update state of this agent below
// root (empty means the real system). With system set it also stops the service and removes the account. Idempotent.
func removeEverything(root string, system bool, stdout, stderr io.Writer, keepUser bool) bool {
	paths := defaultAgentPaths(root)
	ok := true
	if system {
		if systemctl, err := lookPath("systemctl"); err == nil {
			_ = runTool(systemctl, "disable", "--now", "speedtest-agent")
		}
	}
	for _, file := range paths.Files {
		switch err := os.Remove(file); {
		case err == nil:
			_, _ = fmt.Fprintf(stdout, "agent: removed %s\n", file)
		case os.IsNotExist(err):
		default:
			_, _ = fmt.Fprintf(stderr, "agent uninstall: remove %s: %v\n", file, err)
			ok = false
		}
	}
	for _, dir := range paths.Dirs {
		if _, err := os.Lstat(dir); os.IsNotExist(err) {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			_, _ = fmt.Fprintf(stderr, "agent uninstall: remove %s: %v\n", dir, err)
			ok = false
			continue
		}
		_, _ = fmt.Fprintf(stdout, "agent: removed %s\n", dir)
	}
	if system {
		if systemctl, err := lookPath("systemctl"); err == nil {
			_ = runTool(systemctl, "daemon-reload")
		}
		if !keepUser && isRoot() {
			if userdel, err := lookPath("userdel"); err == nil && runTool("id", paths.User) == nil {
				if err := runTool(userdel, paths.User); err == nil {
					_, _ = fmt.Fprintf(stdout, "agent: removed account %s\n", paths.User)
				}
			}
		}
	}
	return ok
}
