package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/jooservices/speedtest/internal/shared/config"
)

func TestRunCenterSuccess(t *testing.T) {
	load := func() (config.CenterConfig, error) {
		return config.CenterConfig{
			HTTPAddr: ":8080",
			Database: config.DatabaseConfig{
				Host:     "127.0.0.1",
				Port:     5432,
				Name:     "speedtest",
				User:     "speedtest",
				Password: "s3cret",
				SSLMode:  "disable",
			},
		}, nil
	}

	var stdout, stderr bytes.Buffer
	code := run(load, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("run() = %d, want 0", code)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	want := "center: configuration loaded\n" +
		"center: would listen on :8080 (HTTP server not implemented yet)\n" +
		"center: database host 127.0.0.1 port 5432\n" +
		"center: database name speedtest user speedtest\n" +
		"center: database sslmode disable\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
	if strings.Contains(stdout.String(), "s3cret") {
		t.Fatalf("stdout = %q, must never print the password", stdout.String())
	}
}

func TestRunCenterInvalidConfig(t *testing.T) {
	load := func() (config.CenterConfig, error) {
		return config.CenterConfig{}, errors.New("SPEEDTEST_DB_PORT must be between 1 and 65535, got 0")
	}

	var stdout, stderr bytes.Buffer
	code := run(load, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "center: invalid configuration: SPEEDTEST_DB_PORT must be between 1 and 65535, got 0") {
		t.Fatalf("stderr = %q, want it to name the offending variable", stderr.String())
	}
}
