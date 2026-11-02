package db

import (
	"context"
	"embed"
	"strings"
	"testing"

	"github.com/jooservices/speedtest/internal/shared/config"
	"github.com/jooservices/speedtest/migrations"
)

func TestOpenRejectsInvalidConfigurationWithoutSecrets(t *testing.T) {
	_, err := Open(context.Background(), config.DatabaseConfig{Password: "private-test-value", SSLMode: "invalid"})
	if err == nil || strings.Contains(err.Error(), "private-test-value") {
		t.Fatal("expected a safe configuration error")
	}
}

func TestOpenCancelledConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Open(ctx, config.DatabaseConfig{Host: "127.0.0.1", Port: 1, Name: "speedtest_test", SSLMode: "disable"})
	if err == nil || err.Error() != "cannot connect to PostgreSQL" {
		t.Fatal("expected a safe connection error")
	}
}

func TestMigrateRejectsInvalidDirection(t *testing.T) {
	if err := Migrate(context.Background(), nil, "invalid"); err == nil {
		t.Fatal("expected direction validation before database access")
	}
}

// A broken/missing embedded bundle must fail before any database access.
func TestMigrateRejectsMissingEmbeddedBundle(t *testing.T) {
	original := migrations.Files
	migrations.Files = embed.FS{}
	t.Cleanup(func() { migrations.Files = original })
	err := Migrate(context.Background(), nil, "up")
	if err == nil || err.Error() != "cannot initialize embedded migrations" {
		t.Fatal("expected safe embedded migration initialization error")
	}
}
