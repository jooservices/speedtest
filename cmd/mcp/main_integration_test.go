//go:build integration

package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/joho/godotenv"
)

func TestRunStdioListsUsageToolAgainstWorktreeDatabase(t *testing.T) {
	values, err := godotenv.Read("../../.env")
	if err != nil {
		t.Fatal("a configured worktree .env is required")
	}
	databaseName, ok := values["SPEEDTEST_DB_NAME"]
	if !ok || databaseName == "" {
		t.Fatal("a configured worktree database name is required")
	}
	for _, key := range []string{"SPEEDTEST_DB_HOST", "SPEEDTEST_DB_PORT", "SPEEDTEST_DB_USER", "SPEEDTEST_DB_PASSWORD", "SPEEDTEST_DB_SSLMODE"} {
		if value, ok := values[key]; ok {
			t.Setenv(key, value)
		}
	}
	t.Setenv("APP_ENV", "local")
	t.Setenv("SPEEDTEST_DB_NAME", databaseName)
	t.Setenv("SPEEDTEST_DB_MAX_CONNS", "1")
	input, inputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	output, outputWriter, err := os.Pipe()
	if err != nil {
		_ = input.Close()
		_ = inputWriter.Close()
		t.Fatal(err)
	}
	oldStdin, oldStdout := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = input, outputWriter
	t.Cleanup(func() { os.Stdin, os.Stdout = oldStdin, oldStdout })
	if _, err := io.WriteString(inputWriter, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"integration-test","version":"1.0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
	}, "\n")+"\n"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- run(ctx) }()
	response := readToolListResponse(t, output, cancel)
	cancel()
	_ = inputWriter.Close()
	runErr := <-runDone
	_ = outputWriter.Close()
	os.Stdin, os.Stdout = oldStdin, oldStdout
	remaining, readErr := io.ReadAll(output)
	_ = output.Close()
	_ = input.Close()
	fullResponse := response + string(remaining)
	if !errors.Is(runErr, context.Canceled) || readErr != nil {
		t.Fatalf("stdio MCP run error=%v, output read error=%v", runErr, readErr)
	}
	if !strings.Contains(fullResponse, `"name":"get_usage"`) {
		t.Fatalf("tools/list response did not advertise get_usage: %s", fullResponse)
	}
}

func readToolListResponse(t *testing.T, output io.Reader, cancel context.CancelFunc) string {
	t.Helper()
	reader := bufio.NewReader(output)
	var response strings.Builder
	for !strings.Contains(response.String(), `"name":"get_usage"`) {
		line, err := reader.ReadString('\n')
		response.WriteString(line)
		if err != nil {
			cancel()
			t.Fatalf("stdio MCP closed before tools/list response: %v", err)
		}
	}
	return response.String()
}
