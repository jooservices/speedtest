//go:build integration

package dbtest

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/shared/config"
	"github.com/jooservices/speedtest/internal/shared/testenv"
)

type fatalMessage string

type failureReporter struct{ *testing.T }

func (failureReporter) Fatal(args ...any) { panic(fatalMessage(fmt.Sprint(args...))) }

func expectFailure(t *testing.T, message string, action func(testReporter)) {
	t.Helper()
	defer func() {
		got := recover()
		if text, ok := got.(fatalMessage); !ok || !strings.Contains(string(text), message) {
			t.Fatalf("expected safe failure %q, got %v", message, got)
		}
	}()
	action(failureReporter{t})
}

func TestRejectUnsafeNameBeforeConnecting(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("")
	if err != nil {
		t.Fatal("cannot construct lazy pool")
	}
	cfg.ConnConfig.Database = "speedtest"
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal("cannot construct lazy pool")
	}
	defer pool.Close()
	expectFailure(t, "refusing database", func(r testReporter) { Reset(r, pool) })
	expectFailure(t, "refusing database", func(r testReporter) { MigrateUp(r, pool) })
}

func TestResetAndMigrateFailurePaths(t *testing.T) {
	values, err := godotenv.Read("../../../../.env")
	if err != nil {
		t.Fatal("worktree .env required")
	}
	testenv.Apply(t, values)
	cfg, err := config.LoadCenter()
	if err != nil {
		t.Fatal("invalid test configuration")
	}
	if err := db.CheckTestDatabase(cfg.Database.Name); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	Reset(t, pool)
	t.Cleanup(func() { Reset(t, pool) })
	exec := func(sql string) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal("fixture query failed")
		}
	}

	// Unrelated dependencies must survive an atomic reset failure (no CASCADE).
	MigrateUp(t, pool)
	exec("CREATE VIEW public.dbtest_dependency AS SELECT id FROM public.users")
	t.Cleanup(func() { exec("DROP VIEW IF EXISTS public.dbtest_dependency") })
	expectFailure(t, "could not reset", func(r testReporter) { Reset(r, pool) })
	exec("SELECT id FROM public.dbtest_dependency")
	exec("DROP VIEW public.dbtest_dependency")
	Reset(t, pool)
	Reset(t, pool) // Reset is idempotent, including journal removal.

	// Unrelated tables are detected, never silently removed.
	exec("CREATE TABLE public.dbtest_extra (id integer)")
	t.Cleanup(func() { exec("DROP TABLE IF EXISTS public.dbtest_extra") })
	expectFailure(t, "did not leave an empty schema", func(r testReporter) { Reset(r, pool) })
	exec("SELECT id FROM public.dbtest_extra")
	exec("DROP TABLE public.dbtest_extra")

	// A conflicting table prevents migration without exposing SQL or credentials.
	exec("CREATE TABLE public.users (id integer)")
	expectFailure(t, "migration failed", func(r testReporter) { MigrateUp(r, pool) })
	Reset(t, pool)

	// An unavailable identity query fails closed before either operation.
	closed, err := db.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	expectFailure(t, "identity/ownership", func(r testReporter) { Reset(r, closed) })
	expectFailure(t, "identity/ownership", func(r testReporter) { MigrateUp(r, closed) })
}
