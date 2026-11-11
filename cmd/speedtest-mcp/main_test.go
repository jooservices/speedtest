package main

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestMainRunsBridgeAndReportsSafeErrors(t *testing.T) {
	oldBridge, oldExit, oldStderr := runBridge, exitProcess, os.Stderr
	t.Cleanup(func() { runBridge, exitProcess, os.Stderr = oldBridge, oldExit, oldStderr })
	t.Setenv("SPEEDTEST_MCP_ENDPOINT", "http://127.0.0.1:8080/mcp")
	t.Setenv("SPEEDTEST_API_TOKEN", "unit-test-token")
	called := false
	runBridge = func(_ context.Context, endpoint, token string) error {
		called = endpoint == "http://127.0.0.1:8080/mcp" && token == "unit-test-token"
		return nil
	}
	main()
	if !called {
		t.Fatal("main did not pass the configured MCP endpoint and token to the bridge")
	}
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = stderrWrite
	exitCode := 0
	exitProcess = func(code int) { exitCode = code }
	runBridge = func(context.Context, string, string) error { return errors.New("safe bridge failure") }
	main()
	if err := stderrWrite.Close(); err != nil {
		t.Fatal(err)
	}
	stderr, err := io.ReadAll(stderrRead)
	if err != nil || exitCode != 1 || string(stderr) != "safe bridge failure\n" {
		t.Fatalf("safe failure exit=%d, stderr=%q, read error=%v", exitCode, stderr, err)
	}
}

func TestRunWithAcceptsMissingEnvAndRejectsOtherLoadErrors(t *testing.T) {
	t.Setenv("SPEEDTEST_MCP_ENDPOINT", "http://127.0.0.1:8080/mcp")
	t.Setenv("SPEEDTEST_API_TOKEN", "unit-test-token")
	oldBridge := runBridge
	t.Cleanup(func() { runBridge = oldBridge })
	called := false
	runBridge = func(_ context.Context, endpoint, token string) error {
		called = endpoint == "http://127.0.0.1:8080/mcp" && token == "unit-test-token"
		return nil
	}
	if err := runWith(func() error { return os.ErrNotExist }); err != nil || !called {
		t.Fatalf("missing .env result = %v, bridge called correctly=%t", err, called)
	}
	called = false
	if err := runWith(func() error { return errors.New("private loader detail") }); err == nil || strings.Contains(err.Error(), "private loader detail") || called {
		t.Fatalf("unsafe .env load result = %v, bridge called=%t", err, called)
	}
}
