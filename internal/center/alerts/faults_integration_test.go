//go:build integration

package alerts_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jooservices/speedtest/internal/center/agentapi"
	"github.com/jooservices/speedtest/internal/center/alerts"
)

var errFault = errors.New("injected alert database failure")

type faultDB struct {
	pgx.Tx
	at, calls int
}

func (f *faultDB) hit() bool { f.calls++; return f.calls == f.at }
func (f *faultDB) Begin(ctx context.Context) (pgx.Tx, error) {
	if f.hit() {
		return nil, errFault
	}
	tx, err := f.Tx.Begin(ctx)
	return &faultTx{Tx: tx, f: f}, err
}
func (f *faultDB) Exec(ctx context.Context, q string, a ...any) (pgconn.CommandTag, error) {
	if f.hit() {
		return pgconn.CommandTag{}, errFault
	}
	return f.Tx.Exec(ctx, q, a...)
}
func (f *faultDB) QueryRow(ctx context.Context, q string, a ...any) pgx.Row {
	if f.hit() {
		return faultRow{}
	}
	return f.Tx.QueryRow(ctx, q, a...)
}
func (f *faultDB) Query(ctx context.Context, q string, a ...any) (pgx.Rows, error) {
	if f.hit() {
		return nil, errFault
	}
	rows, err := f.Tx.Query(ctx, q, a...)
	if err != nil {
		return nil, err
	}
	return &faultRows{Rows: rows, f: f}, nil
}

type faultRow struct{}

func (faultRow) Scan(...any) error { return errFault }

type faultRows struct {
	pgx.Rows
	f *faultDB
}

func (r *faultRows) Scan(a ...any) error {
	if r.f.hit() {
		return errFault
	}
	return r.Rows.Scan(a...)
}
func (r *faultRows) Err() error {
	if r.f.hit() {
		return errFault
	}
	return r.Rows.Err()
}

type faultTx struct {
	pgx.Tx
	f *faultDB
}

func (x *faultTx) Exec(ctx context.Context, q string, a ...any) (pgconn.CommandTag, error) {
	if x.f.hit() {
		return pgconn.CommandTag{}, errFault
	}
	return x.Tx.Exec(ctx, q, a...)
}
func (x *faultTx) QueryRow(ctx context.Context, q string, a ...any) pgx.Row {
	if x.f.hit() {
		return faultRow{}
	}
	return x.Tx.QueryRow(ctx, q, a...)
}
func (x *faultTx) Query(ctx context.Context, q string, a ...any) (pgx.Rows, error) {
	if x.f.hit() {
		return nil, errFault
	}
	rows, err := x.Tx.Query(ctx, q, a...)
	if err != nil {
		return nil, err
	}
	return &faultRows{Rows: rows, f: x.f}, nil
}
func (x *faultTx) Commit(ctx context.Context) error {
	if x.f.hit() {
		return errFault
	}
	return x.Tx.Commit(ctx)
}

func TestAlertDatabaseFailuresArePropagatedIntegration(t *testing.T) {
	p, ctx := integrationDB(t)
	now := time.Now().UTC()
	addNode(t, ctx, p, testNodeID, "fault-node", now.Add(-24*time.Hour), 30)
	s := &alerts.Store{DB: p, AppKey: []byte(appKey)}
	input := alerts.RuleInput{Name: "failure", Metric: "failed", Op: "=", Value: 1, ForTests: 1, AllNodes: true, Severity: "warn", Channels: []string{"email"}, Enabled: true}
	id := addRule(t, ctx, s, "fault-baseline", input)
	_, etag, err := s.GetRule(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	offline := input
	offline.Name = "offline"
	offline.Metric = "offline"
	addRule(t, ctx, s, "offline-baseline", offline)
	ipRule := input
	ipRule.Name = "IP changed"
	ipRule.Metric = "ip_changed"
	addRule(t, ctx, s, "ip-baseline", ipRule)
	const resultID = "31000000-0000-4000-8000-000000000099"
	if _, err := p.Exec(ctx, `INSERT INTO ip_addresses(ip) VALUES ('203.0.113.1'),('203.0.113.2');
 INSERT INTO results(id,node_id,ts,status,threads,error) VALUES ('31000000-0000-4000-8000-000000000099','10000000-0000-4000-8000-000000000001',now(),'failed',1,'fixture');
 INSERT INTO ip_change_signals(result_id,node_id,ts,from_ip,to_ip) VALUES ('31000000-0000-4000-8000-000000000099','10000000-0000-4000-8000-000000000001',now(),'203.0.113.1','203.0.113.2')`); err != nil {
		t.Fatal(err)
	}
	if err := s.EvaluateOffline(ctx); err != nil {
		t.Fatal(err)
	}
	events, _, err := s.ListEvents(ctx, alerts.EventFilter{}, 1, 20)
	if err != nil || len(events) == 0 {
		t.Fatalf("offline event fixture: %v count=%d", err, len(events))
	}
	eventID := events[0].ID
	_, eventTag, err := s.GetEvent(ctx, eventID)
	if err != nil {
		t.Fatal(err)
	}
	ops := map[string]func(*alerts.Store) error{
		"get-event":   func(s *alerts.Store) error { _, _, e := s.GetEvent(ctx, eventID); return e },
		"acknowledge": func(s *alerts.Store) error { _, _, e := s.Acknowledge(ctx, eventID, true, eventTag); return e },
		"replay": func(s *alerts.Store) error {
			_, _, e := s.CreateRule(ctx, "owner:alerts-test", "fault-baseline", input)
			return e
		},
		"result-timestamp-fallback": func(s *alerts.Store) error {
			tx, e := s.DB.Begin(ctx)
			if e != nil {
				return e
			}
			defer func() { _ = tx.Rollback(ctx) }()
			return s.AfterResultStored(ctx, tx, testNodeID, agentapi.Result{ID: resultID, TS: "invalid", Status: "failed", IP: strPointer("203.0.113.2")})
		},
		"list-rules":   func(s *alerts.Store) error { _, _, e := s.ListRules(ctx, 1, 20); return e },
		"get-rule":     func(s *alerts.Store) error { _, _, e := s.GetRule(ctx, id); return e },
		"replace-rule": func(s *alerts.Store) error { _, _, e := s.ReplaceRule(ctx, id, input, etag); return e },
		"delete-rule":  func(s *alerts.Store) error { return s.DeleteRule(ctx, id, etag) },
		"create-rule":  func(s *alerts.Store) error { _, _, e := s.CreateRule(ctx, "owner", "new-rule", input); return e },
		"list-events":  func(s *alerts.Store) error { _, _, e := s.ListEvents(ctx, alerts.EventFilter{}, 1, 20); return e },
		"drain":        func(s *alerts.Store) error { _, e := s.DrainSignals(ctx); return e },
		"offline":      func(s *alerts.Store) error { return s.EvaluateOffline(ctx) },
		"ack-all":      func(s *alerts.Store) error { _, e := s.AcknowledgeAll(ctx); return e },
		"result": func(s *alerts.Store) error {
			tx, e := s.DB.Begin(ctx)
			if e != nil {
				return e
			}
			defer func() { _ = tx.Rollback(ctx) }()
			return s.AfterResultStored(ctx, tx, testNodeID, agentapi.Result{ID: resultID, TS: now.Format(time.RFC3339Nano), Status: "failed", IP: strPointer("203.0.113.2")})
		},
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			for at := 1; at < 100; at++ {
				tx, err := p.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				f := &faultDB{Tx: tx, at: at}
				store := &alerts.Store{DB: f, AppKey: []byte(appKey)}
				err = op(store)
				_ = tx.Rollback(ctx)
				if f.calls < at {
					if err != nil {
						t.Fatalf("baseline failed: %v", err)
					}
					return
				}
				if err == nil {
					t.Fatalf("database call %d failure swallowed", at)
				}
			}
			t.Fatal("fault sweep did not terminate")
		})
	}
}

func TestAlertLoopImmediateWorkAndCancellationIntegration(t *testing.T) {
	p, ctx := integrationDB(t)
	store := &alerts.Store{DB: p}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	done := make(chan struct{})
	go func() { store.Loop(cancelled); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("loop ignored cancellation")
	}
}

func strPointer(s string) *string { return &s }
