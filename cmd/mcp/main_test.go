package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jooservices/speedtest/internal/center/mcpserver"
	"github.com/jooservices/speedtest/internal/shared/config"
)

func TestMainLifecycle(t *testing.T) {
	previousRun, previousExit := runCommand, exitProcess
	t.Cleanup(func() { runCommand, exitProcess = previousRun, previousExit })
	t.Run("success returns normally", func(t *testing.T) {
		called := false
		runCommand = func(context.Context) error { called = true; return nil }
		exitProcess = func(int) { t.Fatal("successful run exited") }
		main()
		if !called {
			t.Fatal("MCP command was not started")
		}
	})
	t.Run("failure exits with status one", func(t *testing.T) {
		runCommand = func(context.Context) error { return errors.New("transport stopped") }
		status := 0
		exitProcess = func(code int) { status = code }
		main()
		if status != 1 {
			t.Fatalf("exit status = %d", status)
		}
	})
}

type identityRow struct {
	values []string
	err    error
}

func (r identityRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return errors.New("scan destination count mismatch")
	}
	for index, value := range r.values {
		*dest[index].(*string) = value
	}
	return nil
}

type identityPool struct {
	row    pgx.Row
	closed bool
}

func (p *identityPool) Begin(context.Context) (pgx.Tx, error) {
	return nil, errors.New("unexpected begin")
}
func (p *identityPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("unexpected exec")
}
func (p *identityPool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("unexpected query")
}
func (p *identityPool) QueryRow(context.Context, string, ...any) pgx.Row { return p.row }
func (p *identityPool) Close()                                           { p.closed = true }

func TestVerifyLocalDatabase(t *testing.T) {
	for _, tc := range []struct {
		name, expected string
		row            pgx.Row
		wantErr        bool
	}{
		{name: "verified", expected: "speedtest_test", row: identityRow{values: []string{"speedtest_test", "workspaces-project:speedtest"}}},
		{name: "wrong name", expected: "speedtest_test", row: identityRow{values: []string{"other", "workspaces-project:speedtest"}}, wantErr: true},
		{name: "wrong owner marker", expected: "speedtest_test", row: identityRow{values: []string{"speedtest_test", "other"}}, wantErr: true},
		{name: "query error", expected: "speedtest_test", row: identityRow{err: errors.New("query failed")}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyLocalDatabase(context.Background(), &identityPool{row: tc.row}, tc.expected)
			if (err != nil) != tc.wantErr {
				t.Fatalf("verification error = %v", err)
			}
		})
	}
}

func TestRunConfiguredLifecycle(t *testing.T) {
	ctx := context.Background()
	validConfig := config.CenterConfig{AppEnv: "local", Database: config.DatabaseConfig{Name: "speedtest_test"}}
	validPool := func() *identityPool {
		return &identityPool{row: identityRow{values: []string{"speedtest_test", "workspaces-project:speedtest"}}}
	}
	t.Run("serves and closes verified pool", func(t *testing.T) {
		pool := validPool()
		served := false
		err := runConfigured(ctx, validConfig, func(context.Context, config.DatabaseConfig) (databasePool, error) { return pool, nil },
			func(_ context.Context, reader mcpserver.UsageReader) error { served = reader != nil; return nil })
		if err != nil || !served || !pool.closed {
			t.Fatalf("run error=%v served=%t closed=%t", err, served, pool.closed)
		}
	})
	t.Run("hides open error", func(t *testing.T) {
		wantErr := errors.New("driver detail")
		err := runConfigured(ctx, validConfig, func(context.Context, config.DatabaseConfig) (databasePool, error) { return nil, wantErr },
			func(context.Context, mcpserver.UsageReader) error {
				t.Fatal("serve called after open failed")
				return nil
			})
		if err == nil || err.Error() != "cannot connect to configured database" {
			t.Fatalf("run error = %v", err)
		}
	})
	t.Run("rejects wrong database identity", func(t *testing.T) {
		pool := &identityPool{row: identityRow{values: []string{"other", "workspaces-project:speedtest"}}}
		err := runConfigured(ctx, validConfig, func(context.Context, config.DatabaseConfig) (databasePool, error) { return pool, nil },
			func(context.Context, mcpserver.UsageReader) error {
				t.Fatal("serve called after identity failure")
				return nil
			})
		if err == nil || !pool.closed {
			t.Fatalf("run error=%v closed=%t", err, pool.closed)
		}
	})
	t.Run("propagates serving error and closes pool", func(t *testing.T) {
		pool, wantErr := validPool(), errors.New("transport stopped")
		err := runConfigured(ctx, validConfig, func(context.Context, config.DatabaseConfig) (databasePool, error) { return pool, nil },
			func(context.Context, mcpserver.UsageReader) error { return wantErr })
		if !errors.Is(err, wantErr) || !pool.closed {
			t.Fatalf("run error=%v closed=%t", err, pool.closed)
		}
	})
	t.Run("production skips local identity query", func(t *testing.T) {
		pool := &identityPool{}
		production := config.CenterConfig{AppEnv: "production", Database: config.DatabaseConfig{Name: "speedtest"}}
		err := runConfigured(ctx, production, func(context.Context, config.DatabaseConfig) (databasePool, error) { return pool, nil },
			func(context.Context, mcpserver.UsageReader) error { return nil })
		if err != nil || !pool.closed {
			t.Fatalf("run error=%v closed=%t", err, pool.closed)
		}
	})
}

func TestRunRejectsUnverifiedLocalDatabaseBeforeConnecting(t *testing.T) {
	t.Setenv("APP_ENV", "local")
	t.Setenv("SPEEDTEST_DB_NAME", "unverified_speedtest_database")
	err := run(context.Background())
	if err == nil || err.Error() != "local/test MCP requires speedtest_test or a Speedtest worktree database" {
		t.Fatalf("run error = %v", err)
	}
}

func TestRunRejectsInvalidConfiguration(t *testing.T) {
	t.Setenv("APP_ENV", "local")
	t.Setenv("SPEEDTEST_DB_NAME", strings.Join([]string{"speedtest", "wt80"}, "_"))
	t.Setenv("SPEEDTEST_DB_MAX_CONNS", "invalid")
	err := run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid configuration") {
		t.Fatalf("run error = %v", err)
	}
}

func TestLoadEnvRejectsMalformedFile(t *testing.T) {
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, ".env"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(directory)
	if err := loadEnv(); err == nil || err.Error() != "cannot load .env" {
		t.Fatalf("load environment error = %v", err)
	}
}
