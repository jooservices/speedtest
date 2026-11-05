//go:build integration

package settings

import (
	"errors"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestNotificationCredentialsAndTransactionalConfig(t *testing.T) {
	p := settingsPool(t)
	s := NewPostgresStore(p, integrationCipher(t), Defaults(""))
	ctx := t.Context()
	if _, err := s.NotificationSecret(ctx, "unknown"); err == nil {
		t.Fatal("unknown channel accepted")
	}
	for _, channel := range []string{"email", "telegram", "slack", "webhook"} {
		secret, err := s.NotificationSecret(ctx, channel)
		if err != nil || secret != "" {
			t.Fatalf("missing channel: %q %v", secret, err)
		}
	}
	_, tag, err := s.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Replace(ctx, tag, validInputWithSecrets()); err != nil {
		t.Fatal(err)
	}
	view, secret, err := s.NotificationConfig(ctx, nil, "webhook")
	if err != nil || secret != "https://hooks.example.com/abc123" || view.Theme != "light" {
		t.Fatalf("config: theme=%s secret matches=%t err=%v", view.Theme, secret == "https://hooks.example.com/abc123", err)
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE settings SET secret_ciphertext='broken'::bytea WHERE key='notify.webhook.secret'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.NotificationConfig(ctx, tx, "webhook"); err == nil {
		t.Fatal("transaction-local corrupt credential accepted")
	}
	if _, err := s.NotificationSecret(ctx, "webhook"); err != nil {
		t.Fatal("uncommitted credential escaped transaction", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	failed := *s
	failed.pool = failingPool{pool: p, calls: &atomic.Int64{}, failAt: 1}
	if _, err := failed.NotificationSecret(ctx, "email"); err == nil {
		t.Fatal("read failure swallowed")
	}
	if _, _, err := s.NotificationConfig(ctx, tx, "email"); err == nil || errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("closed transaction read must fail", err)
	}
}
