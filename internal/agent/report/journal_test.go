package report

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/agent/engine"
)

func TestResultFixturesStayWithinDefaultRetention(t *testing.T) {
	now := time.Now()
	fixtures := []struct {
		name string
		item Result
	}{
		{name: "successful", item: okResult("00000000-0000-4000-8000-000000000001")},
		{name: "failed", item: failedResult("00000000-0000-4000-8000-000000000002", "offline")},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			got, err := time.Parse(time.RFC3339, fixture.item.TS)
			if err != nil {
				t.Fatalf("fixture timestamp is invalid: %v", err)
			}
			age := now.Sub(got)
			if age < 0 || age >= DefaultMaxAge {
				t.Fatalf("fixture age = %s, want within 0..%s", age, DefaultMaxAge)
			}
		})
	}
}

func TestCommandJournalAtomicReplayAndFailures(t *testing.T) {
	log, _ := testLogger(t)
	s, err := Open(SpoolConfig{Dir: privateDir(t)}, log)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	state := json.RawMessage(`{"phase":"queued"}`)
	if err = s.SaveCommand("command", state); err != nil {
		t.Fatal(err)
	}
	entries, err := s.Commands()
	if err != nil || string(entries["command"]) != string(state) {
		t.Fatal(entries, err)
	}
	r := okResult("00000000-0000-4000-8000-000000000001")
	next := json.RawMessage(`{"result":"00000000-0000-4000-8000-000000000001"}`)
	if err = s.AppendWithState(r, "command", next); err != nil {
		t.Fatal(err)
	}
	entries, err = s.Commands()
	if err != nil || string(entries["command"]) != string(next) {
		t.Fatal(entries, err)
	}
	if _, err = s.db.Exec(`CREATE TRIGGER journal_failure BEFORE UPDATE ON command_state BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	r.ID = "00000000-0000-4000-8000-000000000002"
	if s.AppendWithState(r, "command", state) == nil {
		t.Fatal("journal failure ignored")
	}
	pending, err := s.Peek(10)
	if err != nil || len(pending) != 1 {
		t.Fatal("failed journal write committed result", err)
	}
	if err = s.DeleteCommand("command"); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if s.SaveCommand("command", state) == nil || s.DeleteCommand("command") == nil {
		t.Fatal("closed journal accepted writes")
	}
	if _, err = s.Commands(); err == nil {
		t.Fatal("closed journal accepted read")
	}
}

func TestRateLimitStateCommitsWithResultAndRestores(t *testing.T) {
	dir := privateDir(t)
	log, _ := testLogger(t)
	s, err := Open(SpoolConfig{Dir: dir}, log)
	if err != nil {
		t.Fatal(err)
	}
	limited := okResult("00000000-0000-4000-8000-000000000081")
	limited.Status = StatusFailed
	limited.Down, limited.Up, limited.Ping, limited.ServerID, limited.Server = nil, nil, nil, nil, nil
	message := "Too many requests"
	limited.Error, limited.FailureClass, limited.Engine = &message, "rate_limited", "ookla"
	state := RateLimitState{Deadline: time.Now().Add(time.Hour).UTC(), Attempt: 1, Recorded: true}
	if err := s.AppendWithMetadata(limited, "", nil, &RateLimitStateUpdate{Engine: "ookla", State: &state}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(SpoolConfig{Dir: dir}, log)
	if err != nil {
		t.Fatal(err)
	}
	states, err := s.RateLimitStates()
	if err != nil || states["ookla"].Attempt != 1 || !states["ookla"].Deadline.Equal(state.Deadline) || !states["ookla"].Recorded {
		t.Fatalf("restored rate-limit state = %+v, %v", states, err)
	}
	good := okResult("00000000-0000-4000-8000-000000000082")
	if err := s.AppendWithMetadata(good, "", nil, &RateLimitStateUpdate{Engine: "ookla"}); err != nil {
		t.Fatal(err)
	}
	states, err = s.RateLimitStates()
	if err != nil || len(states) != 0 {
		t.Fatalf("successful result did not clear durable backoff: %+v %v", states, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRateLimitSpoolCrashHelper(t *testing.T) {
	if os.Getenv("REPORT_RATE_LIMIT_CRASH_HELPER") != "1" {
		return
	}
	dir := os.Getenv("REPORT_RATE_LIMIT_CRASH_DIR")
	if dir == "" {
		return
	}
	s, err := Open(SpoolConfig{Dir: dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := okResult("00000000-0000-4000-8000-000000000086")
	result.Status = StatusFailed
	result.Down, result.Up, result.Ping, result.ServerID, result.Server = nil, nil, nil, nil, nil
	message := "Too many requests"
	result.Error, result.FailureClass, result.Engine = &message, "rate_limited", "ookla"
	state := RateLimitState{Deadline: time.Now().Add(time.Hour).UTC(), Attempt: 1, Recorded: true}
	if err := s.AppendWithMetadata(result, "", nil, &RateLimitStateUpdate{Engine: "ookla", State: &state}); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func TestRateLimitStateSurvivesAgentProcessCrash(t *testing.T) {
	dir := privateDir(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestRateLimitSpoolCrashHelper$")
	cmd.Env = []string{"REPORT_RATE_LIMIT_CRASH_HELPER=1", "REPORT_RATE_LIMIT_CRASH_DIR=" + dir}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rate-limit crash helper: %v: %s", err, output)
	}
	s, err := Open(SpoolConfig{Dir: dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	results, err := s.Peek(10)
	states, stateErr := s.RateLimitStates()
	if err != nil || len(results) != 1 || results[0].FailureClass != "rate_limited" || stateErr != nil || states["ookla"].Attempt != 1 {
		t.Fatalf("crash did not preserve the atomic result and backoff: results=%+v states=%+v err=%v stateErr=%v", results, states, err, stateErr)
	}
}

func TestRateLimitStateWriteFailureRollsBackResult(t *testing.T) {
	s := openSpool(t, SpoolConfig{})
	if _, err := s.db.Exec(`CREATE TRIGGER fail_rate_state BEFORE INSERT ON rate_limit_state BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	limited := okResult("00000000-0000-4000-8000-000000000083")
	limited.Status = StatusFailed
	limited.Down, limited.Up, limited.Ping, limited.ServerID, limited.Server = nil, nil, nil, nil, nil
	message := "Too many requests"
	limited.Error, limited.FailureClass, limited.Engine = &message, "rate_limited", "ookla"
	state := RateLimitState{Deadline: time.Now().Add(time.Hour).UTC(), Attempt: 1, Recorded: true}
	if err := s.AppendWithMetadata(limited, "", nil, &RateLimitStateUpdate{Engine: "ookla", State: &state}); err == nil {
		t.Fatal("rate-limit state write failure was ignored")
	}
	pending, err := s.Peek(10)
	states, stateErr := s.RateLimitStates()
	if err != nil || len(pending) != 0 || stateErr != nil || len(states) != 0 {
		t.Fatalf("partial result/state transaction: pending=%d states=%+v err=%v stateErr=%v", len(pending), states, err, stateErr)
	}
}

func TestRateLimitStateRejectsInvalidMetadataAtomically(t *testing.T) {
	s := openSpool(t, SpoolConfig{})
	deadline := time.Now().Add(time.Hour).UTC()
	updates := []*RateLimitStateUpdate{
		{Engine: "", State: &RateLimitState{Deadline: deadline, Attempt: 1, Recorded: true}},
		{Engine: "ookla", State: &RateLimitState{Deadline: deadline, Attempt: 0, Recorded: true}},
	}
	for i, update := range updates {
		result := okResult(fmt.Sprintf("00000000-0000-4000-8000-%012x", 84+i))
		if err := s.AppendWithMetadata(result, "", nil, update); err == nil {
			t.Fatal("invalid rate-limit state was accepted")
		}
	}
	pending, err := s.Peek(10)
	states, stateErr := s.RateLimitStates()
	if err != nil || len(pending) != 0 || stateErr != nil || len(states) != 0 {
		t.Fatalf("invalid metadata partially committed: pending=%d states=%+v err=%v stateErr=%v", len(pending), states, err, stateErr)
	}
}

func TestRateLimitStatesRejectsInvalidDurableRows(t *testing.T) {
	cases := []struct {
		name     string
		engine   string
		deadline int64
		attempt  int
		recorded int
	}{
		{name: "unknown engine", engine: "other", deadline: 1, attempt: 1, recorded: 1},
		{name: "missing deadline", engine: "ookla", deadline: 0, attempt: 1, recorded: 1},
		{name: "missing attempt", engine: "ookla", deadline: 1, attempt: 0, recorded: 1},
		{name: "not recorded", engine: "ookla", deadline: 1, attempt: 1, recorded: 0},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			s := openSpool(t, SpoolConfig{})
			if _, err := s.db.Exec(`INSERT INTO rate_limit_state(engine,deadline,attempt,recorded) VALUES(?,?,?,?)`, test.engine, test.deadline, test.attempt, test.recorded); err != nil {
				t.Fatal(err)
			}
			if _, err := s.RateLimitStates(); err == nil {
				t.Fatal("invalid durable rate-limit state was accepted")
			}
		})
	}
}

func TestJournalMalformedRows(t *testing.T) {
	log, _ := testLogger(t)
	s, err := Open(SpoolConfig{Dir: privateDir(t)}, log)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err = s.db.Exec(`INSERT INTO command_state(id,payload) VALUES(NULL,'{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Commands(); err == nil {
		t.Fatal("invalid journal row accepted")
	}
}

func TestDeliveryObserverRetainsPendingOnError(t *testing.T) {
	for _, accepted := range []bool{true, false} {
		t.Run(map[bool]string{true: "accepted", false: "quarantine"}[accepted], func(t *testing.T) {
			log, _ := testLogger(t)
			s, err := Open(SpoolConfig{Dir: privateDir(t)}, log)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			sender := senderFunc(func(_ context.Context, b []Result) ([]Outcome, error) {
				id := b[0].ID
				if accepted {
					return []Outcome{{ID: &id, Index: 0, Outcome: "accepted", Status: 201}}, nil
				}
				return []Outcome{{ID: &id, Index: 0, Outcome: "rejected", Status: 422, Problem: &Problem{Status: 422}}}, nil
			})
			fail := errors.New("journal failure")
			r := NewReporter(s, sender, WithDeliveryObserver(func(Result, bool) error { return fail }))
			if _, err = r.Enqueue(okResult("00000000-0000-4000-8000-000000000001")); err != nil {
				t.Fatal(err)
			}
			if !errors.Is(r.Flush(t.Context()), fail) {
				t.Fatal("observer error lost")
			}
			rows, err := s.Peek(10)
			if err != nil || len(rows) != 1 {
				t.Fatal("result removed before durable observer", err)
			}
		})
	}
}

func TestNullableIPAndAutomaticServer(t *testing.T) {
	r, err := FromEngine(engine.Result{Error: "speedtest: no_route"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	_ = json.Unmarshal(data, &decoded)
	if decoded["ip"] != nil {
		t.Fatal("unknown IP must be null")
	}
	var roundtrip Result
	if err = json.Unmarshal(data, &roundtrip); err != nil || roundtrip.IP != "" || roundtrip.Validate() != nil {
		t.Fatal(err)
	}
	good := okResult("00000000-0000-4000-8000-000000000001")
	good.ServerID = nil
	good.Server = &Server{Name: "LibreSpeed", Host: "speed.example"}
	if err = good.Validate(); err != nil {
		t.Fatal(err)
	}
	good.IP = ""
	if good.Validate() == nil {
		t.Fatal("successful result without IP accepted")
	}
	good.IP = "203.0.113.1"
	good.Server = &Server{}
	if good.Validate() == nil {
		t.Fatal("empty server identity accepted")
	}
}

func TestArrayRawReportsArePreserved(t *testing.T) {
	for _, raw := range []string{`[{"download":1}]`, `[invalid]`} {
		e := engine.Result{Error: "speedtest: execution", Raw: json.RawMessage(raw)}
		result, err := FromEngine(e, Options{})
		if raw == `[invalid]` {
			if err == nil {
				t.Fatal("invalid raw accepted")
			}
			continue
		}
		if err != nil || string(result.Raw) != `{"reports":[{"download":1}]}` {
			t.Fatal(string(result.Raw), err)
		}
	}
}
