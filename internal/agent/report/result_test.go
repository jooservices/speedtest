package report

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/agent/engine"
)

func TestValidateOK(t *testing.T) {
	r := okResult("00000000-0000-4000-8000-000000000001")
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestValidateDegraded(t *testing.T) {
	r := okResult("00000000-0000-4000-8000-000000000001")
	r.Status = StatusDegraded
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestValidateFailed(t *testing.T) {
	r := failedResult("00000000-0000-4000-8000-000000000001", "speedtest: timeout")
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestServerSelectionSnapshotWireFields(t *testing.T) {
	r := okResult("00000000-0000-4000-8000-000000000001")
	legacy, err := json.Marshal(r)
	if err != nil || strings.Contains(string(legacy), "serverSelectionKnown") || strings.Contains(string(legacy), "pinnedServerId") {
		t.Fatalf("legacy result snapshot fields=%s err=%v", legacy, err)
	}
	r.ServerSelectionKnown = true
	r.PinnedServerID = i64p(54321)
	if err := r.Validate(); err != nil {
		t.Fatal("valid pin snapshot rejected:", err)
	}
	encoded, err := json.Marshal(r)
	var fields map[string]any
	if err != nil || json.Unmarshal(encoded, &fields) != nil || fields["serverSelectionKnown"] != true || fields["pinnedServerId"] != float64(54321) {
		t.Fatalf("pin snapshot wire fields=%s err=%v", encoded, err)
	}
	r.ServerSelectionKnown = false
	if err := r.Validate(); err == nil {
		t.Fatal("pin value without a known measurement selection was accepted")
	}
}

func TestValidateRejectsBadInput(t *testing.T) {
	base := okResult("00000000-0000-4000-8000-000000000001")
	cases := map[string]func(*Result){
		"empty id":          func(r *Result) { r.ID = "" },
		"bad ts":            func(r *Result) { r.TS = "not-a-time" },
		"bad ip":            func(r *Result) { r.IP = "localhost" },
		"threads low":       func(r *Result) { r.Threads = 0 },
		"threads high":      func(r *Result) { r.Threads = 65 },
		"negative bytes":    func(r *Result) { r.BytesDown = -1 },
		"negative duration": func(r *Result) { r.Duration = -0.1 },
		"negative down":     func(r *Result) { r.Down = f64p(-1) },
		"loss over 100":     func(r *Result) { r.Loss = f64p(101) },
		"failed no error":   func(r *Result) { r.Status = StatusFailed; r.Error = nil },
		"ok with error":     func(r *Result) { r.Error = strp("x") },
		"ok missing down":   func(r *Result) { r.Down = nil },
		"ok missing server": func(r *Result) { r.Server = nil },
		"ok zero server":    func(r *Result) { r.ServerID = i64p(0) },
		"unknown status":    func(r *Result) { r.Status = "nope" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := base
			mutate(&r)
			if err := r.Validate(); err == nil {
				t.Fatalf("Validate() = nil, want error for %s", name)
			}
		})
	}
}

func TestFromEngineOK(t *testing.T) {
	ts := time.Date(2026, 11, 3, 7, 0, 0, 0, time.UTC)
	e := engine.Result{
		Timestamp:  ts,
		Down:       f64p(300),
		Up:         f64p(150),
		Ping:       f64p(8.5),
		Jitter:     f64p(1.2),
		Loss:       f64p(0),
		LoadedDown: &engine.LoadedLatency{Low: f64p(5), IQM: f64p(8), High: f64p(12)},
		Server:     &engine.Server{ID: 42, Name: "Hanoi", Location: "Hanoi, VN", Host: "h.example", Endpoint: "https://h.example/backend"},
		ISP:        "Viettel",
		ExternalIP: "203.0.113.10",
		ResultID:   "r123",
		ResultURL:  "https://example.test/r123",
		BytesDown:  1000,
		BytesUp:    500,
		Duration:   3.2,
		Raw:        json.RawMessage(`{"type":"result"}`),
	}

	r, err := FromEngine(e, Options{Manual: true, Threads: 8})
	if err != nil {
		t.Fatalf("FromEngine() error = %v", err)
	}
	if !validUUIDv4(r.ID) {
		t.Fatalf("ID = %q, want allocated UUID", r.ID)
	}
	if r.Status != StatusOK {
		t.Fatalf("Status = %q, want ok", r.Status)
	}
	if r.TS != "2026-11-03T07:00:00Z" {
		t.Fatalf("TS = %q", r.TS)
	}
	if r.Down == nil || *r.Down != 300 || r.Up == nil || *r.Up != 150 || r.Ping == nil || *r.Ping != 8.5 {
		t.Fatalf("measurements not mapped: %+v", r)
	}
	if r.DLat == nil || *r.DLat != 8 {
		t.Fatalf("DLat = %v, want IQM 8", r.DLat)
	}
	if r.LoadedDown == nil || r.LoadedDown.Low == nil || *r.LoadedDown.High != 12 {
		t.Fatalf("LoadedDown = %+v", r.LoadedDown)
	}
	if r.ServerID == nil || *r.ServerID != 42 {
		t.Fatalf("ServerID = %v, want 42", r.ServerID)
	}
	if r.Server == nil || r.Server.Name != "Hanoi" || r.Server.City != "Hanoi, VN" || r.Server.Host != "h.example" || r.Server.Endpoint != "https://h.example/backend" {
		t.Fatalf("Server = %+v", r.Server)
	}
	if r.IP != "203.0.113.10" || r.EngineIP != "203.0.113.10" || r.EngineISP != "Viettel" {
		t.Fatalf("identity fields: %+v", r)
	}
	if !r.Manual || r.Threads != 8 {
		t.Fatalf("Manual/Threads = %v/%d", r.Manual, r.Threads)
	}
	if string(r.Raw) != `{"type":"result"}` {
		t.Fatalf("Raw = %s", r.Raw)
	}
}

func TestFromEngineFailedWithIPOverride(t *testing.T) {
	e := engine.Result{Error: "speedtest: no_route"}

	r, err := FromEngine(e, Options{IP: "203.0.113.9", ID: "00000000-0000-4000-8000-000000000009"})
	if err != nil {
		t.Fatalf("FromEngine() error = %v", err)
	}
	if r.Status != StatusFailed {
		t.Fatalf("Status = %q, want failed", r.Status)
	}
	if r.Error == nil || *r.Error != "speedtest: no_route" {
		t.Fatalf("Error = %v", r.Error)
	}
	if r.Down != nil || r.Up != nil || r.Ping != nil || r.ServerID != nil {
		t.Fatalf("failed result must have null measurements: %+v", r)
	}
	if r.IP != "203.0.113.9" {
		t.Fatalf("IP = %q, want override", r.IP)
	}
	if r.ID != "00000000-0000-4000-8000-000000000009" {
		t.Fatalf("ID = %q, want pre-set", r.ID)
	}
	if r.TS == "" || !strings.HasSuffix(r.TS, "Z") {
		t.Fatalf("TS = %q, want a zero timestamp filled with now", r.TS)
	}
}

func TestFromEngineRejectsResultWithoutIP(t *testing.T) {
	e := engine.Result{
		Timestamp: time.Now(),
		Down:      f64p(1),
		Up:        f64p(1),
		Ping:      f64p(1),
		Server:    &engine.Server{ID: 1, Name: "test"},
	}
	if _, err := FromEngine(e, Options{}); err == nil {
		t.Fatal("FromEngine() = nil error, want validation error for missing IP")
	}
}

func TestFromEngineDefaultThreads(t *testing.T) {
	e := engine.Result{
		Timestamp:  time.Now(),
		Down:       f64p(1),
		Up:         f64p(1),
		Ping:       f64p(1),
		Server:     &engine.Server{ID: 1, Name: "test"},
		ExternalIP: "203.0.113.1",
	}
	r, err := FromEngine(e, Options{})
	if err != nil {
		t.Fatalf("FromEngine() error = %v", err)
	}
	if r.Threads != DefaultThreads {
		t.Fatalf("Threads = %d, want %d", r.Threads, DefaultThreads)
	}
}

func TestResultValidationBoundaries(t *testing.T) {
	cases := []func(*Result){
		func(r *Result) { r.ID = "00000000-0000-4000-8000-00000000000z" },
		func(r *Result) { r.TS = "2026-11-03T07:00:00+00:00" },
		func(r *Result) { r.Error = strp(strings.Repeat("x", 2001)) },
		func(r *Result) { r.Raw = []byte(`[]`) },
		func(r *Result) { r.Raw = []byte(`null`) },
		func(r *Result) { r.Raw = []byte(`{"x":"` + strings.Repeat("x", 65536) + `"}`) },
		func(r *Result) { r.Warnings = make([]string, 11) },
		func(r *Result) { r.Warnings = []string{strings.Repeat("x", 2001)} },
		func(r *Result) { r.LoadedUp = &LoadedLatency{IQM: f64p(-1)} },
	}
	for i, mut := range cases {
		r := result(t)
		mut(&r)
		if err := r.Validate(); err == nil {
			t.Fatal(i)
		}
	}
	r := result(t)
	r.Warnings = []string{"warning"}
	r.Raw = []byte(`{}`)
	r.Loss = f64p(0)
	r.LoadedUp = &LoadedLatency{IQM: f64p(0)}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	e := engine.Result{Error: "timeout", LoadedUp: &engine.LoadedLatency{IQM: f64p(5)}}
	if _, err := FromEngine(e, Options{IP: "203.0.113.10"}); err != nil {
		t.Fatal(err)
	}
	if loaded(nil) != nil {
		t.Fatal("nil latency")
	}
}

func TestOptionalResultSchemaAndNonfiniteValues(t *testing.T) {
	cases := []func(*Result){
		func(r *Result) { r.Server = &Server{ID: 0} },
		func(r *Result) { r.Server = &Server{ID: 1, Host: strings.Repeat("h", 256)} },
		func(r *Result) { r.EngineIP = "bad" },
		func(r *Result) { r.EngineResultURL = "relative/path" },
		func(r *Result) { r.EngineResultURL = "http://bad\n" },
		func(r *Result) { r.Down = f64p(math.NaN()) },
		func(r *Result) { r.Duration = math.Inf(1) },
		func(r *Result) { r.Loss = f64p(math.Inf(-1)) },
	}
	for i, mut := range cases {
		r := result(t)
		mut(&r)
		if err := r.Validate(); err == nil {
			t.Fatal(i)
		}
	}
	r := result(t)
	r.Server = &Server{ID: 1, Name: "test"}
	r.EngineIP = "2001:db8::1"
	r.EngineResultURL = "https://example.test/result"
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	// This valid RFC3339 value cannot be stored in Unix nanoseconds without overflow.
	s := openSpool(t, SpoolConfig{})
	r.TS = "0001-01-01T00:00:00Z"
	appendResult(t, s, r)
	if len(peek(t, s, 1)) != 0 {
		t.Fatal("ancient measurement escaped age cap")
	}
}

func TestUUIDExtraHyphensRejected(t *testing.T) {
	r := result(t)
	r.ID = "--000000-0000-4000-8000-000000000000"
	if r.Validate() == nil {
		t.Fatal("UUID with extra hyphens accepted")
	}
}
