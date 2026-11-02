package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/shared/config"
)

func TestRunAgentSuccess(t *testing.T) {
	load := func() (config.AgentConfig, error) {
		return config.AgentConfig{
			Name:                 "viettel-fast-50",
			ExpectedDownloadMbps: 300,
			ExpectedUploadMbps:   150,
			Interval:             30 * time.Minute,
			CenterURL:            "https://center.example.com",
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
	want := "agent: configuration loaded\n" +
		"agent: node \"viettel-fast-50\" (test loop not implemented yet)\n" +
		"agent: expected download 300 Mbps\n" +
		"agent: expected upload 150 Mbps\n" +
		"agent: would test every 30m0s\n" +
		"agent: would report to https://center.example.com\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestRunAgentInvalidConfig(t *testing.T) {
	load := func() (config.AgentConfig, error) {
		return config.AgentConfig{}, errors.New("SPEEDTEST_NODE_INTERVAL must be greater than 0")
	}

	var stdout, stderr bytes.Buffer
	code := run(load, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "agent: invalid configuration: SPEEDTEST_NODE_INTERVAL must be greater than 0") {
		t.Fatalf("stderr = %q, want it to name the offending variable", stderr.String())
	}
}
