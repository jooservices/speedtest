package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/agent/engine"
	"github.com/jooservices/speedtest/internal/agent/netpath"
	"github.com/jooservices/speedtest/internal/agent/testutil"
)

type captureEngine struct {
	got engine.Options
	err error
}

func (e *captureEngine) Run(_ context.Context, opts engine.Options) (engine.Result, error) {
	e.got = opts
	return engine.Result{}, e.err
}

func TestEngineRunner(t *testing.T) {
	e := &captureEngine{err: errors.New("engine failed")}
	r := engineRunner{engine: e}
	if err := r.Run(context.Background()); !errors.Is(err, e.err) {
		t.Fatalf("error = %v", err)
	}
	r.binding = netpath.Binding{SourceIP: "127.0.0.1"}
	if err := r.Run(context.Background()); !errors.Is(err, e.err) {
		t.Fatal(err)
	}
	if e.got.SourceIP != "127.0.0.1" {
		t.Fatalf("options = %+v", e.got)
	}
	r.binding = netpath.Binding{Interface: "speedtest-does-not-exist"}
	if err := r.Run(context.Background()); err == nil {
		t.Fatal("invalid path accepted")
	}
}

func TestRunAgentInvalidEngineEnvironment(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, filepath.Join(dir, "config.yaml"), "name: node\n")
	writeCredential(t, filepath.Join(dir, "credential"), "secret", 0600)
	t.Setenv("SPEEDTEST_AGENT_CONFIG", filepath.Join(dir, "config.yaml"))
	t.Setenv("SPEEDTEST_AGENT_CREDENTIAL", filepath.Join(dir, "credential"))
	t.Setenv("SPEEDTEST_AGENT_CENTER", "http://center.invalid")
	t.Setenv("SPEEDTEST_OOKLA_ARGS", "invalid-json")
	var out, errout bytes.Buffer
	if code := run(context.Background(), []string{"run"}, &out, &errout); code != 1 || !strings.Contains(out.String(), "invalid engine configuration") {
		t.Fatalf("code=%d log=%s", code, out.String())
	}
}

func TestRunAgentControlAndEngine(t *testing.T) {
	center := testutil.NewCenter(testutil.WithCommands(testutil.Command{ID: "manual", Type: "run_now"}))
	defer center.Close()
	dir := t.TempDir()
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
	if len(center.Heartbeats()) != 1 || len(center.Acks()) != 1 || center.Acks()[0].Ack.Status != "succeeded" {
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
