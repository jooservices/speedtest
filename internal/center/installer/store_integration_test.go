//go:build integration

package installer

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
)

var errInjected = errors.New("injected database failure")

type faultDB struct {
	Database
	at, calls int
}

func (f *faultDB) fail() bool { f.calls++; return f.at == f.calls }
func (f *faultDB) Begin(ctx context.Context) (pgx.Tx, error) {
	if f.fail() {
		return nil, errInjected
	}
	tx, err := f.Database.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &faultTx{Tx: tx, f: f}, nil
}
func (f *faultDB) Exec(ctx context.Context, q string, a ...any) (pgconn.CommandTag, error) {
	if f.fail() {
		return pgconn.CommandTag{}, errInjected
	}
	return f.Database.Exec(ctx, q, a...)
}
func (f *faultDB) Query(ctx context.Context, q string, a ...any) (pgx.Rows, error) {
	if f.fail() {
		return nil, errInjected
	}
	return f.Database.Query(ctx, q, a...)
}
func (f *faultDB) QueryRow(ctx context.Context, q string, a ...any) pgx.Row {
	if f.fail() {
		return failedRow{errInjected}
	}
	return f.Database.QueryRow(ctx, q, a...)
}

type failedRow struct{ err error }

func (r failedRow) Scan(...any) error { return r.err }

type faultTx struct {
	pgx.Tx
	f *faultDB
}

func (t *faultTx) Exec(ctx context.Context, q string, a ...any) (pgconn.CommandTag, error) {
	if t.f.fail() {
		return pgconn.CommandTag{}, errInjected
	}
	return t.Tx.Exec(ctx, q, a...)
}
func (t *faultTx) QueryRow(ctx context.Context, q string, a ...any) pgx.Row {
	if t.f.fail() {
		return failedRow{errInjected}
	}
	return t.Tx.QueryRow(ctx, q, a...)
}
func (t *faultTx) Commit(ctx context.Context) error {
	if t.f.fail() {
		return errInjected
	}
	return t.Tx.Commit(ctx)
}

func TestStoreTransactionsAndFailuresIntegration(t *testing.T) {
	pool, ctx, key := openInstallerIntegrationPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	const probeID = "00000000-0000-4000-8000-000000000901"
	const jobID = "00000000-0000-4000-8000-000000000902"
	const cursorID = "00000000-0000-4000-8000-000000000903"
	const missingID = "00000000-0000-4000-8000-000000000904"
	fingerprint := "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	probe := SSHProbe{ID: probeID, Fingerprint: fingerprint, ExpiresAt: time.Now().UTC().Add(time.Hour)}
	target := ProbeInput{Host: "node.test", Port: 22}
	input := InstallInput{ProbeID: probeID, Fingerprint: fingerprint, SSH: SSHInput{Host: target.Host, Port: 22, User: "root", Auth: "password", Secret: "test-only"}}
	execSQL := func(q string, a ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, a...); err != nil {
			t.Fatal(err)
		}
	}
	fixture := func(job bool) {
		execSQL("DELETE FROM installer_jobs")
		execSQL("DELETE FROM installer_probes")
		execSQL("INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES($1,'fixture',100,100) ON CONFLICT DO NOTHING", installerIntegrationNode)
		execSQL("INSERT INTO installer_probes(id,host,port,fingerprint,expires_at) VALUES($1,$2,22,$3,$4)", probeID, target.Host, fingerprint, probe.ExpiresAt)
		if job {
			execSQL("INSERT INTO installer_jobs(id,node_id,principal,status,step) VALUES($1,$2,'owner','running','connect')", jobID, installerIntegrationNode)
			execSQL("INSERT INTO installer_events(id,installation_id,step,message,pct,status) VALUES($1,$2,'connect','Connecting',50,'running')", cursorID, jobID)
		}
	}
	actions := []struct {
		name string
		job  bool
		run  func(*PostgresStore) error
	}{
		{"saveProbe", false, func(s *PostgresStore) error {
			p := probe
			p.ID = missingID
			_, err := s.saveProbe(ctx, "owner", "new-probe", target, p)
			return err
		}},
		{"saveProbeReplay", false, func(s *PostgresStore) error {
			_, err := NewPostgresStore(pool).saveProbe(ctx, "owner", "replay", target, SSHProbe{ID: missingID, Fingerprint: fingerprint, ExpiresAt: probe.ExpiresAt})
			if err != nil {
				return err
			}
			_, err = s.saveProbe(ctx, "owner", "replay", target, probe)
			return err
		}},
		{"createInstallation", false, func(s *PostgresStore) error {
			_, _, err := s.createInstallation(ctx, "owner", "install", jobID, installerIntegrationNode, input)
			return err
		}},
		{"installationReplay", false, func(s *PostgresStore) error {
			_, _, err := NewPostgresStoreWithKey(pool, key).createInstallation(ctx, "owner", "install", jobID, installerIntegrationNode, input)
			if err != nil {
				return err
			}
			_, _, err = s.createInstallation(ctx, "owner", "install", jobID, installerIntegrationNode, input)
			return err
		}},
		{"appendEvent", true, func(s *PostgresStore) error {
			return s.appendEvent(ctx, jobID, "verify", "Verifying", 10, "running", nil)
		}},
		{"events", true, func(s *PostgresStore) error { _, _, err := s.events(ctx, jobID, cursorID); return err }},
		{"recoverInterrupted", true, func(s *PostgresStore) error { return s.recoverInterrupted(ctx) }},
		{"heartbeat", false, func(s *PostgresStore) error { _, err := s.lastHeartbeat(ctx, installerIntegrationNode); return err }},
	}
	for _, action := range actions {
		t.Run(action.name, func(t *testing.T) {
			fixture(action.job)
			counted := &faultDB{Database: pool}
			if err := action.run(NewPostgresStoreWithKey(counted, key)); err != nil {
				t.Fatal(err)
			}
			count := counted.calls
			for n := 1; n <= count; n++ {
				t.Run(fmt.Sprint(n), func(t *testing.T) {
					fixture(action.job)
					faulty := &faultDB{Database: pool, at: n}
					if err := action.run(NewPostgresStoreWithKey(faulty, key)); err == nil {
						t.Fatalf("database failure at operation %d was ignored", n)
					}
				})
			}
		})
	}
	fixture(true)
	s := NewPostgresStoreWithKey(pool, key)
	if _, err := s.installation(ctx, "invalid"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.installation(ctx, missingID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.lastHeartbeat(ctx, missingID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, _, err := s.events(ctx, jobID, "invalid"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, _, err := s.events(ctx, jobID, missingID); !errors.Is(err, ErrCursorExpired) {
		t.Fatal(err)
	}
	for _, status := range []string{"bad", "running"} {
		pct := float64(0)
		if status == "running" {
			pct = -1
		}
		if err := s.appendEvent(ctx, jobID, "verify", "message", pct, status, nil); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
	if err := s.appendEvent(ctx, missingID, "verify", "message", 50, "running", nil); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.appendEvent(ctx, jobID, "complete", "done", 100, "succeeded", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.appendEvent(ctx, jobID, "verify", "late event", 10, "running", nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.installation(ctx, jobID)
	if err != nil || got.Status != "succeeded" {
		t.Fatalf("terminal state changed: %+v %v", got, err)
	}
	fixture(false)
	_, _, err = s.createInstallation(ctx, "owner", "bound-node", jobID, installerIntegrationNode, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.createInstallation(ctx, "owner", "bound-node", jobID, missingID, input); !errors.Is(err, ErrConflict) {
		t.Fatalf("idempotency key crossed node boundary: %v", err)
	}
	fixture(false)
	_, _, err = s.createInstallation(ctx, "owner", "missing-node", jobID, missingID, input)
	if !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	bad := input
	bad.ProbeID = missingID
	if _, _, err = s.createInstallation(ctx, "owner", "missing-probe", jobID, installerIntegrationNode, bad); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	bad = input
	bad.Fingerprint = "different"
	if _, _, err = s.createInstallation(ctx, "owner", "different-key", jobID, installerIntegrationNode, bad); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	// An active job prevents two independent requests from changing the same node.
	execSQL("INSERT INTO installer_jobs(id,node_id,principal,status,step) VALUES($1,$2,'owner','running','connect')", jobID, installerIntegrationNode)
	if _, _, err = s.createInstallation(ctx, "owner", "busy", missingID, installerIntegrationNode, input); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}

	// Rehearse rollback with every new table populated and preserve the node.
	another := probe
	another.ID = missingID
	if _, err := s.saveProbe(ctx, "owner", "populated", target, another); err != nil {
		t.Fatal(err)
	}
	execSQL("INSERT INTO installer_events(id,installation_id,step,message,pct,status) VALUES($1,$2,'connect','Connecting',1,'running')", cursorID, jobID)
	execSQL("INSERT INTO installer_idempotency(principal,key_hash,request_hash,installation_id,expires_at) VALUES('owner',$1,$2,$3,now()+interval '1 hour')", digest("populated"), digest("fixture"), jobID)
	for range 3 { // Engine schedules and audit log sit above the installer migration.
		if err := db.Migrate(ctx, pool, "down"); err != nil {
			t.Fatal(err)
		}
	}
	var removed, preserved bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('installer_jobs') IS NULL,EXISTS(SELECT 1 FROM nodes WHERE id=$1)", installerIntegrationNode).Scan(&removed, &preserved); err != nil || !removed || !preserved {
		t.Fatalf("installer rollback lost isolation: %v", err)
	}
	if err := db.Migrate(ctx, pool, "up"); err != nil {
		t.Fatal(err)
	}
}
