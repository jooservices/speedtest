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

func TestPoolURLUsesTheConfiguredPoolSize(t *testing.T) {
	cfg := config.DatabaseConfig{Host: "db.example", Port: 5432, Name: "speedtest", User: "u", Password: "p@ss/word", SSLMode: "require"}
	for _, tc := range []struct {
		max  int
		want string
	}{{0, "pool_max_conns=10"}, {-3, "pool_max_conns=10"}, {25, "pool_max_conns=25"}} {
		cfg.MaxConns = tc.max
		if got := poolURL(cfg); !strings.Contains(got, tc.want) || !strings.Contains(got, "sslmode=require") || strings.Contains(got, "p@ss/word") {
			t.Errorf("poolURL(max=%d) = %q, want %q and an escaped password", tc.max, got, tc.want)
		}
	}
}
