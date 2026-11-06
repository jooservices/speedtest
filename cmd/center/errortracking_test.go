package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/jooservices/speedtest/internal/shared/errtrack"
)

func TestStartErrorTrackingNeverStopsTheCenter(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	t.Cleanup(func() { _, _ = errtrack.Init(errtrack.Options{}) })

	t.Setenv("SPEEDTEST_SENTRY_DSN", "")
	startErrorTracking("center", logger)()
	if errtrack.Enabled() || logs.Len() != 0 {
		t.Fatalf("without a DSN nothing starts and nothing is logged: %q", logs.String())
	}

	t.Setenv("SPEEDTEST_SENTRY_DSN", "https://public@sentry.example.test/1")
	t.Setenv("SPEEDTEST_SENTRY_SAMPLE_RATE", "7")
	startErrorTracking("center", logger)()
	if errtrack.Enabled() || !strings.Contains(logs.String(), "error tracking is off") {
		t.Fatalf("a bad sample rate turns tracking off with a warning: %q", logs.String())
	}

	logs.Reset()
	t.Setenv("SPEEDTEST_SENTRY_SAMPLE_RATE", "")
	t.Setenv("SPEEDTEST_SENTRY_DSN", "not a dsn")
	startErrorTracking("center", logger)()
	if errtrack.Enabled() || !strings.Contains(logs.String(), "error tracking is off") {
		t.Fatalf("a bad DSN turns tracking off with a warning: %q", logs.String())
	}

	logs.Reset()
	t.Setenv("SPEEDTEST_SENTRY_DSN", "https://public@sentry.example.test/1")
	flush := startErrorTracking("center", logger)
	defer flush()
	if !errtrack.Enabled() || !strings.Contains(logs.String(), "error tracking is on") {
		t.Fatalf("a valid DSN turns tracking on: %q", logs.String())
	}
	if strings.Contains(logs.String(), "public@") {
		t.Fatal("the DSN must never be logged")
	}
}
