package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/agent/report"

	"github.com/jooservices/speedtest/internal/agent/testutil"
)

func TestRunAgentInvalidEngineEnvironment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SPEEDTEST_SPOOL_DIR", filepath.Join(dir, "spool"))
	writeConfig(t, filepath.Join(dir, "config.yaml"), "name: node\n")
	writeCredential(t, filepath.Join(dir, "credential"), "secret", 0600)
	t.Setenv("SPEEDTEST_AGENT_CONFIG", filepath.Join(dir, "config.yaml"))
	t.Setenv("SPEEDTEST_AGENT_CREDENTIAL", filepath.Join(dir, "credential"))
	t.Setenv("SPEEDTEST_AGENT_CENTER", "http://center.invalid")
	t.Setenv("SPEEDTEST_OOKLA_ARGS", "invalid-json")
	var out, errout bytes.Buffer
	if code := run(context.Background(), []string{"run"}, &out, &errout); code != 1 || !strings.Contains(out.String(), "agent initialization failed") {
		t.Fatalf("code=%d log=%s", code, out.String())
	}
}

func TestRunAgentControlAndEngine(t *testing.T) {
	center := testutil.NewCenter(testutil.WithCommands(testutil.Command{ID: "manual", Type: "run_now"}))
	defer center.Close()
	dir := t.TempDir()
	t.Setenv("SPEEDTEST_SPOOL_DIR", filepath.Join(dir, "spool"))
	writeConfig(t, filepath.Join(dir, "config.yaml"), "name: node\n")
	writeCredential(t, filepath.Join(dir, "credential"), "test-node-credential", 0600)
	t.Setenv("SPEEDTEST_AGENT_CONFIG", filepath.Join(dir, "config.yaml"))
	t.Setenv("SPEEDTEST_AGENT_CREDENTIAL", filepath.Join(dir, "credential"))
	t.Setenv("SPEEDTEST_AGENT_CENTER", center.URL)
	t.Setenv("SPEEDTEST_OOKLA_BINARY", testutil.BuildFakeSpeedtest(t))
	t.Setenv("SPEEDTEST_OOKLA_ARGS", `["--scenario","success"]`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, errout safeLog
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"run"}, &out, &errout) }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "speed test finished") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("code=%d log=%s", code, out.String())
	}
	if len(center.Heartbeats()) != 1 || len(center.Acks()) != 1 || center.Acks()[0].Ack.Status != "running" {
		t.Fatal("control protocol did not complete")
	}
	if !strings.Contains(out.String(), "speed test finished") || strings.Contains(out.String(), "speed test failed") || strings.Contains(out.String(), "test-node-credential") {
		t.Fatalf("agent log=%s", out.String())
	}
}

type safeLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *safeLog) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *safeLog) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func TestAgentRunFailureExitCodes(t *testing.T) {
	t.Setenv("SPEEDTEST_AGENT_CONFIG", filepath.Join(t.TempDir(), "config.yaml"))
	t.Setenv("SPEEDTEST_AGENT_CREDENTIAL", filepath.Join(t.TempDir(), "credential"))
	var out, errout bytes.Buffer
	if code := run(t.Context(), []string{"run", "--unknown"}, &out, &errout); code != 2 {
		t.Fatal(code)
	}
	dir := t.TempDir()
	writeCredential(t, filepath.Join(dir, "credential"), "secret", 0600)
	t.Setenv("SPEEDTEST_AGENT_CREDENTIAL", filepath.Join(dir, "credential"))
	t.Setenv("SPEEDTEST_AGENT_CENTER", "http://127.0.0.1:1")
	spool := filepath.Join(dir, "spool")
	t.Setenv("SPEEDTEST_SPOOL_DIR", spool)
	// A corrupt private spool is a startup failure, never an apparently healthy agent.
	if err := os.Mkdir(spool, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spool, "spool.sqlite"), []byte("invalid database"), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := run(t.Context(), []string{"run"}, &out, &errout); code != 1 {
		t.Fatal(code)
	}
	if strings.Contains(out.String(), "secret") {
		t.Fatal("credential leaked")
	}
}

func TestAgentSignalHelper(t *testing.T) {
	if os.Getenv("SPEEDTEST_SIGNAL_HELPER") != "1" {
		return
	}
	os.Args = []string{"agent", "run"}
	main()
}

func TestAgentSignalsJoinAndCloseSpool(t *testing.T) {
	for _, sig := range []os.Signal{syscall.SIGTERM, os.Interrupt} {
		t.Run(sig.String(), func(t *testing.T) {
			center := testutil.NewCenter(testutil.WithCommands(testutil.Command{ID: "manual-signal", Type: "run_now"}))
			defer center.Close()
			dir := t.TempDir()
			writeConfig(t, filepath.Join(dir, "config.yaml"), "name: node\n")
			writeCredential(t, filepath.Join(dir, "credential"), "test-node-credential", 0600)
			cmd := exec.Command(os.Args[0], "-test.run=^TestAgentSignalHelper$")
			cmd.Env = append(os.Environ(), "SPEEDTEST_SIGNAL_HELPER=1", "SPEEDTEST_AGENT_CONFIG="+filepath.Join(dir, "config.yaml"), "SPEEDTEST_AGENT_CREDENTIAL="+filepath.Join(dir, "credential"), "SPEEDTEST_AGENT_CENTER="+center.URL, "SPEEDTEST_SPOOL_DIR="+filepath.Join(dir, "spool"), "SPEEDTEST_OOKLA_BINARY="+testutil.BuildFakeSpeedtest(t), `SPEEDTEST_OOKLA_ARGS=["--scenario","timeout"]`)
			var out safeLog
			cmd.Stdout = &out
			cmd.Stderr = &out
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					if killErr := cmd.Process.Kill(); killErr != nil {
						t.Logf("kill during cleanup: %v", killErr)
					}
					if waitErr := <-done; waitErr != nil {
						t.Logf("agent exited during cleanup: %v", waitErr)
					}
				}
			})
			deadline := time.Now().Add(5 * time.Second)
			for !strings.Contains(out.String(), "speed test started") && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if !strings.Contains(out.String(), "speed test started") {
				t.Fatal("measurement did not start", out.String())
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err, out.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("agent did not stop")
			}
			if !strings.Contains(out.String(), "agent stopped") || strings.Contains(out.String(), "test-node-credential") {
				t.Fatal(out.String())
			}
			spool, err := report.Open(report.SpoolConfig{Dir: filepath.Join(dir, "spool")}, nil)
			if err != nil {
				t.Fatal("spool could not reopen", err)
			}
			defer func() { _ = spool.Close() }()
			rows, err := spool.Peek(10)
			if err != nil || len(rows) != 1 || rows[0].Status != "failed" {
				t.Fatal("interrupted measurement not durable", err)
			}
		})
	}
}
