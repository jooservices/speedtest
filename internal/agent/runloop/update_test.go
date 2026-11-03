package runloop

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/agent/control"
	"github.com/jooservices/speedtest/internal/agent/update"
)

func updateLoop(t *testing.T, version string) (*Loop, update.Paths) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SPEEDTEST_AGENT_BINARY", filepath.Join(dir, "speedtest-agent"))
	t.Setenv("SPEEDTEST_AGENT_UPDATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("SPEEDTEST_AGENT_INBOX_DIR", filepath.Join(dir, "inbox"))
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	l, err := New(bootstrap(t), parseURL(t, "http://127.0.0.1:1"), "credential", version, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	return l, update.DefaultPaths()
}

func advertised(version string) *update.Release {
	return &update.Release{Version: version, URL: "https://center.example/agent", SHA256: strings.Repeat("a", 64)}
}

func TestOwnerUpdateCommandAcknowledgesBeforeRestarting(t *testing.T) {
	oldDelay := restartDelay
	restartDelay = 50 * time.Millisecond
	t.Cleanup(func() { restartDelay = oldDelay })
	l, paths := updateLoop(t, "1.0.0")
	served := make(chan error, 1)
	go func() { served <- l.Serve(t.Context()) }()
	defer func() { _ = l.Close() }()
	two := "2.0.0"
	ack := l.updateAgent(control.Command{ID: "c1", Type: "update_agent", Version: &two}, control.AgentConfig{Release: advertised("2.0.0")})
	if ack.Status != "succeeded" || ack.Pct != 100 {
		t.Fatalf("ack = %+v", ack)
	}
	var req struct{ Version string }
	data, err := os.ReadFile(filepath.Join(paths.InboxDir, "request.json"))
	if err != nil || json.Unmarshal(data, &req) != nil || req.Version != "2.0.0" {
		t.Fatalf("request = %s (%v)", data, err)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("a requested restart is a clean stop, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not stop for the restart")
	}
}

func TestOwnerUpdateCommandRefusals(t *testing.T) {
	l, _ := updateLoop(t, "1.0.0")
	defer func() { _ = l.Close() }()
	same, other := "1.0.0", "9.9.9"
	if ack := l.updateAgent(control.Command{Version: &same}, control.AgentConfig{}); ack.Status != "succeeded" || ack.Phase != "already installed" {
		t.Fatalf("a redelivered command for the running version is a success: %+v", ack)
	}
	if ack := l.updateAgent(control.Command{Version: &other}, control.AgentConfig{Release: advertised("2.0.0")}); ack.Status != "failed" || ack.Error == nil {
		t.Fatalf("a version that is not advertised must fail: %+v", ack)
	}
	if ack := l.updateAgent(control.Command{}, control.AgentConfig{}); ack.Status != "failed" {
		t.Fatalf("a command without a version must fail: %+v", ack)
	}
}

func TestUnconfirmedUpdateStopsTheLoopWithAnError(t *testing.T) {
	oldInterval := watchdogInterval
	watchdogInterval = 10 * time.Millisecond
	t.Cleanup(func() { watchdogInterval = oldInterval })
	l, paths := updateLoop(t, "2.0.0")
	defer func() { _ = l.Close() }()
	state, _ := json.Marshal(map[string]any{"status": "pending_confirm", "version": "2.0.0", "previous": "1.0.0", "deadline": time.Now().Add(-time.Minute)})
	if err := os.WriteFile(filepath.Join(paths.StateDir, "state.json"), state, 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- l.Serve(context.Background()) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "previous version is restored") {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an unconfirmed update must stop the agent so the previous binary is restored")
	}
}

func TestRestartWithoutARunningLoopIsHarmless(t *testing.T) {
	oldDelay := restartDelay
	restartDelay = time.Millisecond
	t.Cleanup(func() { restartDelay = oldDelay })
	l, _ := updateLoop(t, "1.0.0")
	defer func() { _ = l.Close() }()
	l.restartSoon()
	time.Sleep(20 * time.Millisecond)
}
