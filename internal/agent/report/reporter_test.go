package report

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

type senderFunc func(context.Context, []Result) ([]Outcome, error)

func (f senderFunc) SendBatch(ctx context.Context, rs []Result) ([]Outcome, error) { return f(ctx, rs) }
func accepted(rs []Result) []Outcome {
	var out []Outcome
	for i, r := range rs {
		out = append(out, Outcome{Index: i, ID: strp(r.ID), Outcome: "accepted", Status: 201})
	}
	return out
}

// A 6-hour outage is simulated by advancing the retry clock rather than sleeping.
func TestOfflineSixHoursThenBatchFlush(t *testing.T) {
	s := openSpool(t, SpoolConfig{})
	base := time.Now()
	var want []string
	var sent []string
	attempts := 0
	elapsed := time.Duration(0)
	var sizes []int
	sender := senderFunc(func(_ context.Context, rs []Result) ([]Outcome, error) {
		attempts++
		if elapsed < 6*time.Hour {
			return nil, &SendError{Retryable: true, Err: errors.New("offline")}
		}
		sizes = append(sizes, len(rs))
		for _, r := range rs {
			sent = append(sent, r.ID)
		}
		return accepted(rs), nil
	})
	logger, _ := testLogger(t)
	r := NewReporter(s, sender, WithBatchSize(3), WithLogger(logger), WithBackoff(Backoff{Initial: time.Hour, Max: time.Hour, Factor: 2}))
	for i := 0; i < 8; i++ {
		v := result(t)
		v.TS = base.UTC().Format(time.RFC3339)
		want = append(want, v.ID)
		if _, err := r.Enqueue(v); err != nil {
			t.Fatal(err)
		}
	}
	r.sleep = func(_ context.Context, d time.Duration) error {
		elapsed += d
		s.now = func() time.Time { return base.Add(elapsed) }
		return nil
	}
	if err := r.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts != 9 || len(sizes) != 3 || sizes[0] != 3 || sizes[1] != 3 || sizes[2] != 2 {
		t.Fatal(attempts, sizes)
	}
	for i := range want {
		if sent[i] != want[i] {
			t.Fatal("FIFO changed")
		}
	}
	if len(peek(t, s, 100)) != 0 {
		t.Fatal("not drained")
	}
	t.Logf("outage=%s batches=%v results=%d FIFO preserved", elapsed, sizes, len(sent))
}

func TestCrashMidSendAndDuplicateDelivery(t *testing.T) {
	dir := privateDir(t)
	s := openSpool(t, SpoolConfig{Dir: dir})
	var mu sync.Mutex
	stored := map[string]string{}
	calls, duplicates := 0, 0
	lose := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if req.URL.Path != "/api/agent/results" || req.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("wrong endpoint/auth")
		}
		var batch struct {
			Results []Result `json:"results"`
		}
		if err := json.NewDecoder(req.Body).Decode(&batch); err != nil {
			t.Error(err)
		}
		// Prove durable commit is visible before network delivery.
		disk, err := Open(SpoolConfig{Dir: dir}, nil)
		if err != nil {
			t.Error(err)
			return
		}
		if len(peek(t, disk, 100)) == 0 {
			t.Error("send before commit")
		}
		_ = disk.Close()
		outcomes := accepted(batch.Results)
		for i, v := range batch.Results {
			b, _ := json.Marshal(v)
			if old, ok := stored[v.ID]; ok {
				if old != string(b) {
					t.Error("UUID content changed")
				}
				outcomes[i].Outcome = "duplicate"
				outcomes[i].Status = 200
				duplicates++
			}
			stored[v.ID] = string(b)
		}
		if lose {
			lose = false
			h := w.(http.Hijacker)
			conn, _, err := h.Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(207)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "code": 207, "data": outcomes})
	}))
	defer srv.Close()
	client := &Client{BaseURL: mustURL(t, srv.URL), Credential: "fixture", HTTP: srv.Client()}
	r := NewReporter(s, client)
	r.sleep = func(context.Context, time.Duration) error { return context.Canceled }
	v := result(t)
	if err := r.Report(context.Background(), v); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	_ = s.Close()
	s = openSpool(t, SpoolConfig{Dir: dir})
	r = NewReporter(s, client)
	if err := r.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 || duplicates != 1 || len(stored) != 1 || len(peek(t, s, 100)) != 0 {
		t.Fatal(calls, duplicates, len(stored))
	}
	t.Logf("requests=%d duplicate_outcomes=%d center_rows=%d pending=0", calls, duplicates, len(stored))
}

func TestReportAllocatesUUIDAndKeepsOnFatal(t *testing.T) {
	s := openSpool(t, SpoolConfig{})
	r := NewReporter(s, senderFunc(func(_ context.Context, rs []Result) ([]Outcome, error) {
		if !isUUID(rs[0].ID) {
			t.Error("missing UUID")
		}
		return nil, errors.New("fatal")
	}))
	v := result(t)
	v.ID = ""
	if err := r.Report(context.Background(), v); err == nil {
		t.Fatal("expected fatal")
	}
	if len(peek(t, s, 2)) != 1 {
		t.Fatal("lost result")
	}
	if err := r.Report(context.Background(), Result{}); err == nil {
		t.Fatal("invalid input")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Flush(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := sleepContext(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := sleepContext(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if err := sleepContext(context.Background(), time.Nanosecond); err != nil {
		t.Fatal(err)
	}
}

func TestRejectedQuarantinedAndUnknownOutcomesRetained(t *testing.T) {
	s := openSpool(t, SpoolConfig{})
	a, b := result(t), result(t)
	appendResult(t, s, a)
	appendResult(t, s, b)
	r := NewReporter(s, senderFunc(func(_ context.Context, rs []Result) ([]Outcome, error) {
		o := accepted(rs)
		o[0].Outcome = "rejected"
		o[0].Status = 422
		o[0].Problem = &Problem{Status: 422, Detail: "untrusted secret"}
		return o, nil
	}))
	if err := r.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM quarantine`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	appendResult(t, s, a)
	r.sender = senderFunc(func(_ context.Context, rs []Result) ([]Outcome, error) {
		o := accepted(rs)
		o[0].Outcome = "unknown"
		return o, nil
	})
	if err := r.Flush(context.Background()); err == nil {
		t.Fatal("unknown outcome acked")
	}
	if len(peek(t, s, 1)) != 1 {
		t.Fatal("lost result")
	}
}

func TestValidateOutcomeFailures(t *testing.T) {
	rs := []Result{result(t), result(t)}
	cases := []func([]Outcome) []Outcome{
		func(o []Outcome) []Outcome { return o[:1] },
		func(o []Outcome) []Outcome { o[1].Index = 2; return o },
		func(o []Outcome) []Outcome { o[1].Index = 0; return o },
		func(o []Outcome) []Outcome { o[0].ID = nil; return o },
		func(o []Outcome) []Outcome { o[0].ID = strp("foreign"); return o },
		func(o []Outcome) []Outcome { o[0].Status = 200; return o },
		func(o []Outcome) []Outcome { o[0].Outcome = "duplicate"; return o },
		func(o []Outcome) []Outcome { o[0].Outcome = "rejected"; return o },
		func(o []Outcome) []Outcome { o[0].Outcome = "other"; return o },
	}
	for i, mut := range cases {
		if _, err := validateOutcomes(rs, mut(accepted(rs))); err == nil {
			t.Fatal(i)
		}
	}
	o := accepted(rs)
	o[0].Outcome = "rejected"
	o[0].Status = 409
	o[0].Problem = &Problem{Status: 409}
	if _, err := validateOutcomes(rs, o); err != nil {
		t.Fatal(err)
	}
}

func TestReporterConcurrentEnqueueWhileOffline(t *testing.T) {
	s := openSpool(t, SpoolConfig{MaxResults: 2})
	started := make(chan struct{})
	resume := make(chan struct{})
	var once sync.Once
	r := NewReporter(s, senderFunc(func(_ context.Context, rs []Result) ([]Outcome, error) {
		once.Do(func() { close(started); <-resume })
		return accepted(rs), nil
	}))
	appendResult(t, s, result(t))
	done := make(chan error, 1)
	go func() { done <- r.Flush(context.Background()) }()
	<-started
	for i := 0; i < 3; i++ {
		if _, err := r.Enqueue(result(t)); err != nil {
			t.Fatal(err)
		}
	}
	close(resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(peek(t, s, 100)) != 0 {
		t.Fatal("not drained")
	}
}

func TestReporterOptionsAndStorageFailures(t *testing.T) {
	for _, size := range []int{-1, 101} {
		s := openSpool(t, SpoolConfig{})
		r := NewReporter(s, senderFunc(func(context.Context, []Result) ([]Outcome, error) { return nil, nil }), WithBatchSize(size), WithLogger(nil))
		if r.batch != 100 || r.logger == nil {
			t.Fatal("invalid defaults")
		}
		_ = s.Close()
		if err := r.Flush(context.Background()); err == nil {
			t.Fatal("closed spool")
		}
	}
	s := openSpool(t, SpoolConfig{})
	appendResult(t, s, result(t))
	if _, err := s.db.Exec(`UPDATE pending SET payload='invalid'`); err != nil {
		t.Fatal(err)
	}
	r := NewReporter(s, senderFunc(func(context.Context, []Result) ([]Outcome, error) { t.Fatal("corrupt result sent"); return nil, nil }))
	if err := r.Flush(context.Background()); err == nil {
		t.Fatal("corrupt spool")
	}
}

func TestBatchByteLimitIncludesJSONEscaping(t *testing.T) {
	s := openSpool(t, SpoolConfig{})
	var calls int
	r := NewReporter(s, senderFunc(func(_ context.Context, rs []Result) ([]Outcome, error) {
		b, err := json.Marshal(struct {
			Results []Result `json:"results"`
		}{rs})
		if err != nil || len(b) > maxBatchBytes {
			t.Error("oversized batch", len(b), err)
		}
		calls++
		return accepted(rs), nil
	}))
	for i := 0; i < 30; i++ {
		v := result(t)
		v.Raw = []byte(`{"x":"` + strings.Repeat("<", 60000) + `"}`)
		if _, err := r.Enqueue(v); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Fatal("batch not split")
	}
	v := result(t)
	v.EngineISP = strings.Repeat("x", maxBatchBytes)
	if _, err := r.Enqueue(v); err == nil {
		t.Fatal("unshippable result accepted")
	}
	if _, err := boundedBatch([]Result{v}); err == nil {
		t.Fatal("unshippable result")
	}
	v.Raw = []byte("invalid")
	if _, err := boundedBatch([]Result{v}); err == nil {
		t.Fatal("invalid JSON")
	}
	c := Client{BaseURL: mustURL(t, "http://localhost")}
	v.Raw = nil
	if _, err := c.SendBatch(context.Background(), []Result{v}); err == nil {
		t.Fatal("oversized HTTP body")
	}
}

// The child exits after receiving a committed center response but before Ack.
func TestCrashProcessHelper(t *testing.T) {
	dir := os.Getenv("JOOST17_CRASH_SPOOL")
	if dir == "" {
		return
	}
	s, err := Open(SpoolConfig{Dir: dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{BaseURL: mustURL(t, os.Getenv("JOOST17_CRASH_CENTER")), Credential: "fixture"}
	r := NewReporter(s, senderFunc(func(ctx context.Context, rs []Result) ([]Outcome, error) {
		_, err := client.SendBatch(ctx, rs)
		if err != nil {
			return nil, err
		}
		os.Exit(23)
		return nil, nil
	}))
	if err := r.Report(context.Background(), result(t)); err != nil {
		t.Fatal(err)
	}
	t.Fatal("child survived crash")
}

func TestActualProcessCrashBeforeAck(t *testing.T) {
	dir := privateDir(t)
	var mu sync.Mutex
	var saved string
	duplicates := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var batch struct {
			Results []Result `json:"results"`
		}
		if err := json.NewDecoder(req.Body).Decode(&batch); err != nil {
			t.Error(err)
			return
		}
		out := accepted(batch.Results)
		if saved != "" {
			if batch.Results[0].ID != saved {
				t.Error("crash changed UUID")
			}
			out[0].Outcome = "duplicate"
			out[0].Status = 200
			duplicates++
		} else {
			saved = batch.Results[0].ID
		}
		w.WriteHeader(207)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "code": 207, "data": out})
	}))
	defer srv.Close()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestCrashProcessHelper$")
	cmd.Env = append(os.Environ(), "JOOST17_CRASH_SPOOL="+dir, "JOOST17_CRASH_CENTER="+srv.URL)
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatalf("child: %v %s", err, output)
	}
	s := openSpool(t, SpoolConfig{Dir: dir})
	if len(peek(t, s, 100)) != 1 {
		t.Fatal("crash lost pending result")
	}
	r := NewReporter(s, &Client{BaseURL: mustURL(t, srv.URL), Credential: "fixture"})
	if err := r.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if duplicates != 1 || len(peek(t, s, 100)) != 0 {
		t.Fatal("duplicate not resolved", duplicates)
	}
	t.Log("child_exit=23 replayed_same_UUID=true duplicate_outcomes=1 pending=0")
}
