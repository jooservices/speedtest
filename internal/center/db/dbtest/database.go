//go:build integration

// Package dbtest provides destructive fixtures for serial database integration tests.
package dbtest

import (
	"context"
	"io/fs"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/migrations"
)

type testReporter interface {
	Helper()
	Fatal(...any)
	Log(...any)
}

func verify(t testReporter, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if err := db.CheckTestDatabase(pool.Config().ConnConfig.Database); err != nil {
		t.Fatal(err)
	}
	var name, marker string
	if err := pool.QueryRow(ctx, "SELECT current_database(), shobj_description(oid, 'pg_database') FROM pg_database WHERE datname=current_database()").Scan(&name, &marker); err != nil || name != pool.Config().ConnConfig.Database || marker != "workspaces-project:speedtest" {
		t.Fatal("test database identity/ownership verification failed")
	}
}

// Reset removes only Speedtest tables and its migration journal, without CASCADE.
// It must never run concurrently with another suite against the same database.
func Reset(t testReporter, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	verify(t, ctx, pool)
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS
	 public.api_access_tokens, public.user_two_factor, public.audit_log, public.installer_idempotency, public.installer_events, public.installer_jobs, public.installer_probe_idempotency, public.installer_probes,
	 public.notification_event_node_settings, public.notification_templates, public.notification_event_settings,
 public.notification_deliveries, public.ip_change_signals, public.alert_notification_outbox, public.maintenance_jobs, public.node_ip_addresses, public.agent_commands, public.alert_state, public.alert_events, public.alert_rules,
 public.engine_schedules, public.schedules, public.node_ip_history, public.results, public.node_credentials,
 public.sessions, public.users, public.nodes, public.ip_addresses, public.settings,
 public.api_idempotency, public.goose_db_version`); err != nil {
		t.Fatal("could not reset test database")
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_type='BASE TABLE'").Scan(&count); err != nil || count != 0 {
		t.Fatal("test database reset did not leave an empty schema")
	}
	t.Log("PASS: test database reset; project tables and goose journal absent")
}

// MigrateUp verifies ownership before applying the embedded test schema.
func MigrateUp(t testReporter, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	verify(t, ctx, pool)
	if err := db.Migrate(ctx, pool, "up"); err != nil {
		t.Fatal(err)
	}
}

var createTable = regexp.MustCompile(`(?im)^\s*CREATE TABLE (?:IF NOT EXISTS )?(?:public\.)?([a-z_][a-z0-9_]*)`)

// speedtestTables lists every table the embedded migrations create, plus the goose journal.
func speedtestTables() map[string]bool {
	known := map[string]bool{"goose_db_version": true}
	files, err := fs.Glob(migrations.Files, "*.sql")
	if err != nil {
		return known
	}
	for _, name := range files {
		data, err := fs.ReadFile(migrations.Files, name)
		if err != nil {
			continue
		}
		for _, m := range createTable.FindAllStringSubmatch(string(data), -1) {
			known[m[1]] = true
		}
	}
	return known
}
