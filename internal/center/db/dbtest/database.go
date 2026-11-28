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
	"github.com/jooservices/speedtest/internal/shared/config"
	"github.com/jooservices/speedtest/migrations"
)

type testReporter interface {
	Helper()
	Fatal(...any)
	Log(...any)
}

func verify(t testReporter, _ context.Context, _ *pgxpool.Pool) {
	t.Helper()
	if err := db.CheckDestructiveAllowed(config.AppEnv()); err != nil {
		t.Fatal(err)
	}
}

// Reset removes only Speedtest tables and its migration journal, without CASCADE.
// It must never run concurrently with another suite against the same database.
func Reset(t testReporter, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	verify(t, ctx, pool)
	// The Speedtest tables are the ones the embedded migrations create (plus the goose journal), so a new migration needs no change
	// here. Tables that no migration creates are unrelated: they are never removed (the count check below then fails).
	known := speedtestTables()
	var names []string
	rows, err := pool.Query(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema='public' AND table_type='BASE TABLE'`)
	if err != nil {
		t.Fatal("could not list test database tables")
	}
	for rows.Next() {
		var name string
		if rows.Scan(&name) != nil {
			t.Fatal("could not read test database tables")
		}
		if known[name] {
			names = append(names, "public."+name)
		}
	}
	rows.Close()
	if len(names) > 0 {
		// One statement, without CASCADE: PostgreSQL orders the foreign keys, and a dependent object outside the list stops the reset.
		if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS "+strings.Join(names, ", ")); err != nil {
			t.Fatal("could not reset test database")
		}
	}
	if _, err := pool.Exec(ctx, `DROP FUNCTION IF EXISTS speedtest_record_monthly_usage()`); err != nil {
		t.Fatal("could not reset usage trigger function")
	}
	if _, err := pool.Exec(ctx, `DROP FUNCTION IF EXISTS speedtest_lock_usage_budget_changes()`); err != nil {
		t.Fatal("could not reset usage lifecycle lock function")
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
