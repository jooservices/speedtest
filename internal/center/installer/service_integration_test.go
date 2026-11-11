//go:build integration

package installer

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/nodes"
	"github.com/jooservices/speedtest/internal/shared/config"
	"github.com/jooservices/speedtest/internal/shared/testenv"
)

const installerIntegrationNode = "00000000-0000-4000-8000-000000000943"

func openInstallerIntegrationPool(t *testing.T) (*pgxpool.Pool, context.Context, []byte) {
	t.Helper()
	values, err := godotenv.Read(filepath.Join("..", "..", "..", ".env"))
	if err != nil {
		t.Fatal("a configured worktree .env is required")
	}
	testenv.Apply(t, values)
	cfg, err := config.LoadCenter()
	if err != nil {
		t.Fatal("invalid worktree integration configuration")
	}
	if err := db.CheckTestDatabase(cfg.Database.Name); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var name, marker string
	if err := pool.QueryRow(ctx, `SELECT current_database(),shobj_description(oid,'pg_database') FROM pg_database WHERE datname=current_database()`).Scan(&name, &marker); err != nil || name != cfg.Database.Name || marker != "workspaces-project:speedtest" {
		t.Fatal("integration database identity and ownership marker verification failed")
	}
	t.Log("PASS: installer integration uses a guarded test database with the Workspaces ownership marker")
	return pool, ctx, []byte("0123456789abcdef0123456789abcdef")
}

func TestInstallWorkflowEndToEndIntegration(t *testing.T) {
	pool, ctx, appKey := openInstallerIntegrationPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES($1,'installer-integration-node',300,100)`, installerIntegrationNode); err != nil {
		t.Fatal("could not create the worktree node fixture")
	}

	successTarget := newFakeSSHServer(t, fakeSSHOptions{
		password: "integration-ssh-password", sudo: true,
		onStart: func() {
			time.Sleep(150 * time.Millisecond)
			if _, err := pool.Exec(ctx, `UPDATE nodes SET last_seen_at=clock_timestamp() WHERE id=$1`, installerIntegrationNode); err != nil {
				t.Errorf("record fake agent heartbeat: %v", err)
			}
		},
	})
	var logs lockedTestLog
	service, err := NewService(ctx, ServiceOptions{
		Database: pool, Enrollment: nodes.NewService(pool, appKey),
		CenterURL: "https://center.example.test", AppKey: appKey, Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := service.Close(closeCtx); err != nil {
			t.Errorf("close installer service: %v", err)
		}
	}()

	probeInput := targetProbe(successTarget)
	probe, err := service.Probe(ctx, "owner-integration", "probe-success", probeInput)
	if err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	sshInput := successTarget.input()
	installInput := InstallInput{ProbeID: probe.ID, Fingerprint: probe.Fingerprint, SSH: sshInput}
	installInput.SSH.SudoPassword = "integration-sudo-password"
	job, err := service.Start(ctx, "owner-integration", "install-success", installerIntegrationNode, installInput)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	replay, err := service.Start(ctx, "owner-integration", "install-success", installerIntegrationNode, installInput)
	if err != nil || replay.ID != job.ID {
		t.Fatalf("idempotent Start() = %+v, %v; want job %s", replay, err, job.ID)
	}
	changed := installInput
	changed.SSH.Secret = "different-integration-password"
	if _, err := service.Start(ctx, "owner-integration", "install-success", installerIntegrationNode, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("reusing idempotency key with changed secret error = %v, want ErrConflict", err)
	}

	completed := waitForInstallerState(t, ctx, service, job.ID, "succeeded")
	if completed.Error != nil {
		t.Fatalf("successful installation returned an error: %q", *completed.Error)
	}
	events, _, err := service.Events(ctx, job.ID, "")
	if err != nil {
		t.Fatalf("Events() error = %v", err)
	}
	var sawDetect, sawVerify, sawEnroll, sawHeartbeat, sawComplete bool
	for _, event := range events {
		switch event.Step {
		case "detect":
			sawDetect = true
		case "verify":
			sawVerify = true
		case "enroll":
			sawEnroll = true
		case "heartbeat":
			sawHeartbeat = true
		case "complete":
			sawComplete = true
		}
		if strings.Contains(event.Message, sshInput.Secret) || strings.Contains(event.Message, installInput.SSH.SudoPassword) {
			t.Fatal("installer event contains a credential")
		}
	}
	if !sawDetect || !sawVerify || !sawEnroll || !sawHeartbeat || !sawComplete {
		t.Fatalf("success events missed required stages: detect=%t verify=%t enroll=%t heartbeat=%t complete=%t", sawDetect, sawVerify, sawEnroll, sawHeartbeat, sawComplete)
	}
	var requestDigest string
	if err := pool.QueryRow(ctx, `SELECT request_hash FROM installer_idempotency WHERE principal='owner-integration'`).Scan(&requestDigest); err != nil {
		t.Fatal("could not read the persisted install request digest")
	}
	if strings.Contains(requestDigest, sshInput.Secret) || strings.Contains(requestDigest, installInput.SSH.SudoPassword) {
		t.Fatal("persisted install request digest contains a credential")
	}

	unsupportedTarget := newFakeSSHServer(t, fakeSSHOptions{password: "integration-ssh-password", sudo: true, mode: "unsupported"})
	unsupportedProbe, err := service.Probe(ctx, "owner-integration", "probe-unsupported", targetProbe(unsupportedTarget))
	if err != nil {
		t.Fatalf("unsupported Probe() error = %v", err)
	}
	unsupportedInput := InstallInput{ProbeID: unsupportedProbe.ID, Fingerprint: unsupportedProbe.Fingerprint, SSH: unsupportedTarget.input()}
	unsupportedInput.SSH.SudoPassword = installInput.SSH.SudoPassword
	failed, err := service.Start(ctx, "owner-integration", "install-unsupported", installerIntegrationNode, unsupportedInput)
	if err != nil {
		t.Fatalf("unsupported Start() error = %v", err)
	}
	failed = waitForInstallerState(t, ctx, service, failed.ID, "failed")
	if failed.Error == nil || !strings.Contains(*failed.Error, "Linux on amd64 or arm64") {
		t.Fatalf("unsupported architecture failure = %+v", failed)
	}

	for _, scenario := range []struct {
		name, mode    string
		sudo          bool
		wrongPassword bool
	}{
		{"wrong-password", "", true, true}, {"no-sudo", "", false, false}, {"disconnect", "disconnect", true, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			target := newFakeSSHServer(t, fakeSSHOptions{password: "integration-ssh-password", sudo: scenario.sudo, mode: scenario.mode})
			probe, err := service.Probe(ctx, "owner-integration", "probe-"+scenario.name, targetProbe(target))
			if err != nil {
				t.Fatal(err)
			}
			input := InstallInput{ProbeID: probe.ID, Fingerprint: probe.Fingerprint, SSH: target.input()}
			if scenario.wrongPassword {
				input.SSH.Secret = "wrong-test-password"
			}
			job, err := service.Start(ctx, "owner-integration", "install-"+scenario.name, installerIntegrationNode, input)
			if err != nil {
				t.Fatal(err)
			}
			failed := waitForInstallerState(t, ctx, service, job.ID, "failed")
			if scenario.mode == "disconnect" && target.authAttempts.Load() < 2 {
				t.Fatal("rollback did not reconnect after transport disconnect")
			}
			if failed.Error == nil || strings.Contains(*failed.Error, input.SSH.Secret) {
				t.Fatal("unsafe or missing error")
			}
		})
	}
	// Shutdown rejects queued work safely without retaining SSH credentials.
	closeCtx, cancelClose := context.WithTimeout(context.Background(), time.Second)
	defer cancelClose()
	if err := service.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	finalProbe, err := service.Probe(ctx, "owner-integration", "probe-closed", targetProbe(successTarget))
	if err != nil {
		t.Fatal(err)
	}
	finalJob, err := service.Start(ctx, "owner-integration", "install-closed", installerIntegrationNode, InstallInput{ProbeID: finalProbe.ID, Fingerprint: finalProbe.Fingerprint, SSH: successTarget.input()})
	if err != nil {
		t.Fatal(err)
	}
	waitForInstallerState(t, ctx, service, finalJob.ID, "failed")
	token := <-successTarget.tokens
	for _, secret := range []string{sshInput.Secret, installInput.SSH.SudoPassword, token} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("installer log contains a credential")
		}
	}
	var leakedCredential bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM installer_jobs WHERE error LIKE '%'||$1||'%' OR error LIKE '%'||$2||'%')`, sshInput.Secret, installInput.SSH.SudoPassword).Scan(&leakedCredential); err != nil || leakedCredential {
		t.Fatal("installer job error persisted a credential")
	}
	t.Log("PASS: owner-independent install service completed a fake SSH install with checksum/enroll/heartbeat stages, replayed idempotently, rejected changed credentials, and failed unsupported architecture safely")
}

func targetProbe(server *fakeSSHServer) ProbeInput {
	host, portText, _ := net.SplitHostPort(server.address)
	port, _ := strconv.Atoi(portText)
	return ProbeInput{Host: host, Port: port}
}

func waitForInstallerState(t *testing.T, ctx context.Context, service *Service, id, want string) Installation {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		job, err := service.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if job.Status == want {
			return job
		}
		if job.Status == "failed" && want != "failed" {
			t.Fatalf("installation failed early: %+v", job)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for installation status: %v", ctx.Err())
		case <-deadline.C:
			t.Fatalf("installation status did not reach %q", want)
		case <-ticker.C:
		}
	}
}

type lockedTestLog struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (l *lockedTestLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buffer.Write(p)
}
func (l *lockedTestLog) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.buffer.String() }

func TestUninstallWorkflowIntegration(t *testing.T) {
	pool, ctx, appKey := openInstallerIntegrationPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES($1,'uninstall-node',300,100)`, installerIntegrationNode); err != nil {
		t.Fatal(err)
	}
	var logs lockedTestLog
	service, err := NewService(ctx, ServiceOptions{Database: pool, Enrollment: nodes.NewService(pool, appKey), CenterURL: "https://center.example.test", AppKey: appKey,
		Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = service.Close(closeCtx)
	}()

	target := newFakeSSHServer(t, fakeSSHOptions{password: "uninstall-ssh-password", sudo: true})
	probe, err := service.Probe(ctx, "owner", "probe-1", targetProbe(target))
	if err != nil {
		t.Fatal(err)
	}
	input := InstallInput{ProbeID: probe.ID, Fingerprint: probe.Fingerprint, SSH: target.input()}
	job, err := service.StartUninstall(ctx, "owner", "key-1", installerIntegrationNode, input)
	if err != nil {
		t.Fatalf("StartUninstall: %v", err)
	}
	done := waitForInstallerState(t, ctx, service, job.ID, "succeeded")
	if done.Error != nil {
		t.Fatalf("error on success: %q", *done.Error)
	}
	events, _, err := service.Events(ctx, job.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	var messages []string
	for _, e := range events {
		messages = append(messages, e.Message)
	}
	joined := strings.Join(messages, "|")
	for _, want := range []string{"Stopping the agent service.", "Removing the agent files.", "Removing the agent account.", "The agent was removed from the node."} {
		if !strings.Contains(joined, want) {
			t.Errorf("progress lacks %q: %v", want, messages)
		}
	}
	if strings.Contains(joined+logs.String(), "uninstall-ssh-password") {
		t.Fatal("the SSH password must never reach events or logs")
	}
	// An idempotency key used for an uninstallation is separate from the same key used for an installation.
	probe2, err := service.Probe(ctx, "owner", "probe-2", targetProbe(target))
	if err != nil {
		t.Fatal(err)
	}
	input2 := InstallInput{ProbeID: probe2.ID, Fingerprint: probe2.Fingerprint, SSH: target.input()}
	const otherNode = "00000000-0000-4000-8000-0000000000b9"
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES($1,'uninstall-other-node',300,100)`, otherNode); err != nil {
		t.Fatal(err)
	}
	other, err := service.Start(ctx, "owner", "key-1", otherNode, input2)
	if err != nil || other.ID == job.ID {
		t.Fatalf("an install with the same key must not replay the uninstall: %+v %v", other, err)
	}

	// A removal that fails on the machine is reported plainly and never claims success.
	broken := newFakeSSHServer(t, fakeSSHOptions{password: "broken-password", sudo: true, mode: "uninstall-fails"})
	bp, err := service.Probe(ctx, "owner", "probe-3", targetProbe(broken))
	if err != nil {
		t.Fatal(err)
	}
	failed, err := service.StartUninstall(ctx, "owner", "key-3", installerIntegrationNode, InstallInput{ProbeID: bp.ID, Fingerprint: bp.Fingerprint, SSH: broken.input()})
	if err != nil {
		t.Fatal(err)
	}
	final := waitForInstallerState(t, ctx, service, failed.ID, "failed")
	if final.Error == nil || !strings.Contains(*final.Error, "could not be fully removed") {
		t.Fatalf("failure message = %v", final.Error)
	}
	// Invalid input is refused before anything is queued.
	if _, err := service.StartUninstall(ctx, "", "k", installerIntegrationNode, input); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty principal = %v", err)
	}
}

func TestInstallEnrollmentFailureStopsBeforeUploadIntegration(t *testing.T) {
	pool, ctx, appKey := openInstallerIntegrationPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES($1,'enrollment-failure-node',300,100)`, installerIntegrationNode); err != nil {
		t.Fatal(err)
	}
	target := newFakeSSHServer(t, fakeSSHOptions{password: "fixture-password", sudo: true})
	service, err := NewService(ctx, ServiceOptions{Database: pool, Enrollment: unavailableEnrollment{}, CenterURL: "https://center.example.test", AppKey: appKey})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := service.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	probe, err := service.Probe(ctx, "owner-failure", "probe-enrollment-failure", targetProbe(target))
	if err != nil {
		t.Fatal(err)
	}
	input := InstallInput{ProbeID: probe.ID, Fingerprint: probe.Fingerprint, SSH: target.input()}
	job, err := service.Start(ctx, "owner-failure", "install-enrollment-failure", installerIntegrationNode, input)
	if err != nil {
		t.Fatal(err)
	}
	failed := waitForInstallerState(t, ctx, service, job.ID, "failed")
	if failed.Error == nil || *failed.Error != "The command center could not prepare the node for enrollment." {
		t.Fatalf("unexpected enrollment error: %+v", failed)
	}
	events, _, err := service.Events(ctx, job.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Step != "queued" && event.Step != "connect" && event.Step != "failed" {
			t.Fatalf("installation continued after enrollment failure: %s", event.Step)
		}
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	for _, command := range target.commands {
		if strings.Contains(command, "base64") || strings.Contains(command, "SPEEDTEST_ENROLL_TOKEN") {
			t.Fatal("upload or installation started without an enrollment token")
		}
	}
}
