// Command mcp exposes read-only Speedtest tools over stdio.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5"
	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/mcpserver"
	"github.com/jooservices/speedtest/internal/center/usage"
	"github.com/jooservices/speedtest/internal/shared/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var runCommand = run
var exitProcess = os.Exit

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := runCommand(ctx)
	stop()
	if err != nil {
		slog.Error("MCP server stopped", "error", err)
		exitProcess(1)
	}
}

func run(ctx context.Context) error {
	if err := loadEnv(); err != nil {
		return err
	}
	cfg, err := config.LoadCenter()
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	return runConfigured(ctx, cfg, func(ctx context.Context, database config.DatabaseConfig) (databasePool, error) {
		return db.Open(ctx, database)
	}, func(ctx context.Context, reader mcpserver.UsageReader) error {
		return mcpserver.New(reader).Run(ctx, &mcp.StdioTransport{})
	})
}

type databasePool interface {
	usage.DB
	Close()
}

func runConfigured(ctx context.Context, cfg config.CenterConfig, open func(context.Context, config.DatabaseConfig) (databasePool, error), serve func(context.Context, mcpserver.UsageReader) error) error {
	if cfg.AppEnv == "local" || cfg.AppEnv == "test" {
		if err := db.CheckTestDatabase(cfg.Database.Name); err != nil {
			return errors.New("local/test MCP requires speedtest_test or a Speedtest worktree database")
		}
	}
	pool, err := open(ctx, cfg.Database)
	if err != nil {
		return errors.New("cannot connect to configured database")
	}
	defer pool.Close()
	if cfg.AppEnv == "local" || cfg.AppEnv == "test" {
		if err := verifyLocalDatabase(ctx, pool, cfg.Database.Name); err != nil {
			return err
		}
	}
	return serve(ctx, &usage.Store{DB: pool})
}

func loadEnv() error {
	if err := godotenv.Load(".env"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot load .env")
	}
	return nil
}

func verifyLocalDatabase(ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, expected string) error {
	var name, marker string
	err := pool.QueryRow(ctx, `SELECT current_database(),shobj_description(oid,'pg_database') FROM pg_database WHERE datname=current_database()`).Scan(&name, &marker)
	if err != nil || name != expected || marker != "workspaces-project:speedtest" {
		return errors.New("local/test Speedtest database identity verification failed")
	}
	return nil
}
