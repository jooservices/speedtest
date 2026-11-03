package report

import (
	"bytes"
	"log/slog"
	"net/url"
	"testing"
)

func f64p(v float64) *float64 { return &v }

func i64p(v int64) *int64 { return &v }

func strp(v string) *string { return &v }

// okResult builds a minimal valid, accepted-able result.
func okResult(id string) Result {
	down, up, ping := 300.0, 150.0, 8.5
	return Result{
		ID:       id,
		TS:       "2026-11-03T07:00:00Z",
		Status:   StatusOK,
		Down:     &down,
		Up:       &up,
		Ping:     &ping,
		ServerID: i64p(42),
		Server:   &Server{ID: 42, Name: "test"},
		IP:       "203.0.113.10",
		Threads:  DefaultThreads,
	}
}

// failedResult builds a minimal valid failed result.
func failedResult(id, err string) Result {
	return Result{
		ID:      id,
		TS:      "2026-11-03T07:00:00Z",
		Status:  StatusFailed,
		Error:   strp(err),
		IP:      "203.0.113.10",
		Threads: DefaultThreads,
	}
}

func testLogger(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, nil)), &buf
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url %q: %v", raw, err)
	}
	return u
}
