package report

import (
	"context"
	"encoding/json"
	"errors"
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
