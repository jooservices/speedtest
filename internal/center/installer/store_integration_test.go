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
	"github.com/jackc/pgx/v5/pgxpool"
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

type installerFailureFixture struct {
	pool                                             *pgxpool.Pool
	ctx                                              context.Context
	key                                              []byte
	probe                                            SSHProbe
	target                                           ProbeInput
	input                                            InstallInput
	probeID, jobID, cursorID, missingID, fingerprint string
}

func newInstallerFailureFixture(t *testing.T) installerFailureFixture {
	t.Helper()
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
	return installerFailureFixture{pool, ctx, key, probe, target, input, probeID, jobID, cursorID, missingID, fingerprint}
}

func (f installerFailureFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

func (f installerFailureFixture) reset(t *testing.T, job bool) {
	t.Helper()
	f.exec(t, "DELETE FROM installer_jobs")
	f.exec(t, "DELETE FROM installer_probes")
	f.exec(t, "INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES($1,'fixture',100,100) ON CONFLICT DO NOTHING", installerIntegrationNode)
	f.exec(t, "INSERT INTO installer_probes(id,host,port,fingerprint,expires_at) VALUES($1,$2,22,$3,$4)", f.probeID, f.target.Host, f.fingerprint, f.probe.ExpiresAt)
	if job {
		f.exec(t, "INSERT INTO installer_jobs(id,node_id,principal,status,step) VALUES($1,$2,'owner','running','connect')", f.jobID, installerIntegrationNode)
		f.exec(t, "INSERT INTO installer_events(id,installation_id,step,message,pct,status) VALUES($1,$2,'connect','Connecting',50,'running')", f.cursorID, f.jobID)
	}
}

type installerFaultAction struct {
	name string
	job  bool
	run  func(*PostgresStore) error
}

func installerFaultActions(f installerFailureFixture) []installerFaultAction {
	ctx, pool := f.ctx, f.pool
	return []installerFaultAction{
		{"saveProbe", false, func(s *PostgresStore) error {
			probe := f.probe
			probe.ID = f.missingID
			_, err := s.saveProbe(ctx, "owner", "new-probe", f.target, probe)
			return err
		}},
		{"saveProbeReplay", false, func(s *PostgresStore) error {
			_, err := NewPostgresStore(pool).saveProbe(ctx, "owner", "replay", f.target, SSHProbe{ID: f.missingID, Fingerprint: f.fingerprint, ExpiresAt: f.probe.ExpiresAt})
			if err != nil {
				return err
			}
			_, err = s.saveProbe(ctx, "owner", "replay", f.target, f.probe)
			return err
		}},
		{"createInstallation", false, func(s *PostgresStore) error {
			_, _, err := s.createInstallation(ctx, "owner", "install", f.jobID, installerIntegrationNode, f.input)
			return err
		}},
		{"installationReplay", false, func(s *PostgresStore) error {
			_, _, err := NewPostgresStoreWithKey(pool, f.key).createInstallation(ctx, "owner", "install", f.jobID, installerIntegrationNode, f.input)
			if err != nil {
				return err
			}
			_, _, err = s.createInstallation(ctx, "owner", "install", f.jobID, installerIntegrationNode, f.input)
			return err
		}},
		{"appendEvent", true, func(s *PostgresStore) error {
			return s.appendEvent(ctx, f.jobID, "verify", "Verifying", 10, "running", nil)
		}},
		{"events", true, func(s *PostgresStore) error { _, _, err := s.events(ctx, f.jobID, f.cursorID); return err }},
		{"recoverInterrupted", true, func(s *PostgresStore) error { return s.recoverInterrupted(ctx) }},
		{"heartbeat", false, func(s *PostgresStore) error { _, err := s.lastHeartbeat(ctx, installerIntegrationNode); return err }},
	}
}

func TestStoreTransactionsAndFailuresIntegration(t *testing.T) {
	f := newInstallerFailureFixture(t)
	verifyInstallerFaults(t, f)
	store := NewPostgresStoreWithKey(f.pool, f.key)
	verifyInstallerReadAndEventErrors(t, f, store)
	verifyInstallerTerminalState(t, f, store)
	verifyInstallerIdempotencyBoundaries(t, f, store)
	verifyInstallerRollback(t, f, store)
}

func verifyInstallerFaults(t *testing.T, f installerFailureFixture) {
	t.Helper()
	for _, action := range installerFaultActions(f) {
		t.Run(action.name, func(t *testing.T) {
			f.reset(t, action.job)
			counted := &faultDB{Database: f.pool}
			if err := action.run(NewPostgresStoreWithKey(counted, f.key)); err != nil {
				t.Fatal(err)
			}
			for n := 1; n <= counted.calls; n++ {
				t.Run(fmt.Sprint(n), func(t *testing.T) {
					f.reset(t, action.job)
					faulty := &faultDB{Database: f.pool, at: n}
					if err := action.run(NewPostgresStoreWithKey(faulty, f.key)); err == nil {
						t.Fatalf("database failure at operation %d was ignored", n)
					}
				})
			}
		})
	}
}

func verifyInstallerReadAndEventErrors(t *testing.T, f installerFailureFixture, store *PostgresStore) {
	t.Helper()
	f.reset(t, true)
	for _, check := range []struct {
		name string
		err  error
	}{
		{"invalid installation", func() error { _, err := store.installation(f.ctx, "invalid"); return err }()},
		{"missing installation", func() error { _, err := store.installation(f.ctx, f.missingID); return err }()},
		{"missing heartbeat", func() error { _, err := store.lastHeartbeat(f.ctx, f.missingID); return err }()},
	} {
		if !errors.Is(check.err, ErrNotFound) {
			t.Fatalf("%s: %v", check.name, check.err)
		}
	}
	if _, _, err := store.events(f.ctx, f.jobID, "invalid"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, _, err := store.events(f.ctx, f.jobID, f.missingID); !errors.Is(err, ErrCursorExpired) {
		t.Fatal(err)
	}
	for _, status := range []string{"bad", "running"} {
		pct := float64(0)
		if status == "running" {
			pct = -1
		}
		if err := store.appendEvent(f.ctx, f.jobID, "verify", "message", pct, status, nil); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
	if err := store.appendEvent(f.ctx, f.missingID, "verify", "message", 50, "running", nil); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func verifyInstallerTerminalState(t *testing.T, f installerFailureFixture, store *PostgresStore) {
	t.Helper()
	if err := store.appendEvent(f.ctx, f.jobID, "complete", "done", 100, "succeeded", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.appendEvent(f.ctx, f.jobID, "verify", "late event", 10, "running", nil); err != nil {
		t.Fatal(err)
	}
	got, err := store.installation(f.ctx, f.jobID)
	if err != nil || got.Status != "succeeded" {
		t.Fatalf("terminal state changed: %+v %v", got, err)
	}
}

func verifyInstallerIdempotencyBoundaries(t *testing.T, f installerFailureFixture, store *PostgresStore) {
	t.Helper()
	f.reset(t, false)
	_, _, err := store.createInstallation(f.ctx, "owner", "bound-node", f.jobID, installerIntegrationNode, f.input)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.createInstallation(f.ctx, "owner", "bound-node", f.jobID, f.missingID, f.input); !errors.Is(err, ErrConflict) {
		t.Fatalf("idempotency key crossed node boundary: %v", err)
	}
	f.reset(t, false)
	_, _, err = store.createInstallation(f.ctx, "owner", "missing-node", f.jobID, f.missingID, f.input)
	if !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	bad := f.input
	bad.ProbeID = f.missingID
	if _, _, err = store.createInstallation(f.ctx, "owner", "missing-probe", f.jobID, installerIntegrationNode, bad); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	bad = f.input
	bad.Fingerprint = "different"
	if _, _, err = store.createInstallation(f.ctx, "owner", "different-key", f.jobID, installerIntegrationNode, bad); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	f.exec(t, "INSERT INTO installer_jobs(id,node_id,principal,status,step) VALUES($1,$2,'owner','running','connect')", f.jobID, installerIntegrationNode)
	if _, _, err = store.createInstallation(f.ctx, "owner", "busy", f.missingID, installerIntegrationNode, f.input); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
}

func verifyInstallerRollback(t *testing.T, f installerFailureFixture, store *PostgresStore) {
	t.Helper()
	probe := f.probe
	probe.ID = f.missingID
	if _, err := store.saveProbe(f.ctx, "owner", "populated", f.target, probe); err != nil {
		t.Fatal(err)
	}
	f.exec(t, "INSERT INTO installer_events(id,installation_id,step,message,pct,status) VALUES($1,$2,'connect','Connecting',1,'running')", f.cursorID, f.jobID)
	f.exec(t, "INSERT INTO installer_idempotency(principal,key_hash,request_hash,installation_id,expires_at) VALUES('owner',$1,$2,$3,now()+interval '1 hour')", digest("populated"), digest("fixture"), f.jobID)
	for range 8 { // Usage, API tokens, audit-source, 60-minute default, engine schedules, two-factor and audit migrations precede installer rollback.
		if err := db.Migrate(f.ctx, f.pool, "down"); err != nil {
			t.Fatal(err)
		}
	}
	var removed, preserved bool
	if err := f.pool.QueryRow(f.ctx, "SELECT to_regclass('installer_jobs') IS NULL,EXISTS(SELECT 1 FROM nodes WHERE id=$1)", installerIntegrationNode).Scan(&removed, &preserved); err != nil || !removed || !preserved {
		t.Fatalf("installer rollback lost isolation: %v", err)
	}
	if err := db.Migrate(f.ctx, f.pool, "up"); err != nil {
		t.Fatal(err)
	}
}
