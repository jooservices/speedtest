package alerts

import (
	"bytes"
	"io"
	"log/slog"
	"testing"

	"github.com/jooservices/speedtest/internal/center/agentapi"
)

func TestReplayEncryptionRejectsTamperingAndWrongIdentity(t *testing.T) {
	s := &Store{AppKey: bytes.Repeat([]byte{7}, 32)}
	ciphertext, err := s.encryptReplay([]byte(`{"id":"example"}`), "owner", "key")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := s.decryptReplay(ciphertext, "owner", "key")
	if err != nil || string(plain) != `{"id":"example"}` {
		t.Fatalf("round trip %s %v", plain, err)
	}
	for _, tc := range []struct {
		data       []byte
		owner, key string
	}{
		{nil, "owner", "key"}, {ciphertext[:10], "owner", "key"}, {ciphertext, "other", "key"}, {ciphertext, "owner", "other"},
	} {
		if _, err := s.decryptReplay(tc.data, tc.owner, tc.key); err == nil {
			t.Fatal("invalid replay accepted")
		}
	}
	ciphertext[len(ciphertext)-1] ^= 1
	if _, err := s.decryptReplay(ciphertext, "owner", "key"); err == nil {
		t.Fatal("tampered replay accepted")
	}
	s.Random = bytes.NewReader(nil)
	if _, err := s.encryptReplay(nil, "owner", "key"); err == nil {
		t.Fatal("entropy failure swallowed")
	}
	if _, err := s.newUUID(); err == nil {
		t.Fatal("UUID entropy failure swallowed")
	}
	s.AppKey = nil
	if _, err := s.encryptReplay(nil, "owner", "key"); err == nil {
		t.Fatal("invalid encryption key accepted")
	}
	if _, err := s.decryptReplay(nil, "owner", "key"); err == nil {
		t.Fatal("invalid decryption key accepted")
	}
}

func TestLoggerSelection(t *testing.T) {
	s := &Store{}
	if s.logger() != slog.Default() {
		t.Fatal("default logger not selected")
	}
	s.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if s.logger() != s.Logger {
		t.Fatal("configured logger not selected")
	}
}

func TestInvalidResourceIdentifiersNeverReachDatabase(t *testing.T) {
	s := &Store{}
	if _, _, err := s.GetRule(t.Context(), "invalid"); err != ErrNotFound {
		t.Fatal(err)
	}
	if _, _, err := s.GetEvent(t.Context(), "invalid"); err != ErrNotFound {
		t.Fatal(err)
	}
	if _, _, err := s.ReplaceRule(t.Context(), "invalid", RuleInput{}, ""); err != ErrNotFound {
		t.Fatal(err)
	}
	if err := s.DeleteRule(t.Context(), "invalid", ""); err != ErrNotFound {
		t.Fatal(err)
	}
	if _, _, err := s.Acknowledge(t.Context(), "invalid", true, ""); err != ErrNotFound {
		t.Fatal(err)
	}
	for _, metric := range []string{"down_pct", "up_pct", "ping", "jitter", "loss"} {
		if metricMatches(Rule{Metric: metric, Op: "<", Value: 100}, agentapi.Result{Status: "ok"}, 0, 0) {
			t.Fatalf("missing %s matched", metric)
		}
	}
}
