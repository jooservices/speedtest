package installer

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestCloseHonorsDeadlineWhileWorkerIsStillStopping(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{ctx: ctx, cancel: cancel}
	s.workers.Add(1)
	timeout, stop := context.WithCancel(context.Background())
	stop()
	if err := s.Close(timeout); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close=%v", err)
	}
	if !s.closed || ctx.Err() == nil {
		t.Fatal("shutdown state not set")
	}
	s.workers.Done()
	if err := s.Close(t.Context()); err != nil {
		t.Fatal("finished workers", err)
	}
}

func TestProbeDisconnectDoesNotPersistFingerprint(t *testing.T) {
	target := newFakeSSHServer(t, fakeSSHOptions{password: "fixture", sudo: true})
	sshInput := target.input()
	input := ProbeInput{Host: sshInput.Host, Port: sshInput.Port}
	if err := target.listener.Close(); err != nil {
		t.Fatal(err)
	}
	s := &Service{store: NewPostgresStoreWithKey(unavailableDB{}, make([]byte, 32))}
	if _, err := s.Probe(t.Context(), "owner", "probe-disconnected", input); !errors.Is(err, ErrDisconnect) {
		t.Fatalf("disconnected probe: %v", err)
	}
}

func TestUninstallConnectionAndSudoFailuresAreReportedSafely(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sudo     bool
		password string
	}{
		{"authentication", true, "wrong-password"}, {"sudo", false, "fixture-password"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := newFakeSSHServer(t, fakeSSHOptions{password: "fixture-password", sudo: tc.sudo})
			input := target.input()
			input.Secret = tc.password
			var logs bytes.Buffer
			s := &Service{ctx: t.Context(), store: NewPostgresStoreWithKey(unavailableDB{}, make([]byte, 32)), logger: slog.New(slog.NewTextHandler(&logs, nil))}
			s.runUninstall(jobRequest{installation: Installation{ID: "00000000-0000-4000-8000-000000000043"}, input: InstallInput{SSH: input, Fingerprint: target.fingerprint}, uninstall: true})
			if !strings.Contains(logs.String(), "SSH uninstallation failed") || strings.Contains(logs.String(), tc.password) {
				t.Fatalf("unsafe or missing failure log: %s", logs.String())
			}
		})
	}
}
