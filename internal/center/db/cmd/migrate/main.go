// Command migrate applies embedded migrations using the current directory's .env.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/shared/config"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout)) }

func run(args []string, out io.Writer) int {
	if len(args) != 1 || (args[0] != "up" && args[0] != "down") {
		_, _ = fmt.Fprintln(out, "usage: migrate up|down")
		return 1
	}
	// A missing or malformed file must not silently select a default/live database.
	if err := godotenv.Load(".env"); err != nil {
		_, _ = fmt.Fprintln(out, "cannot load .env")
		return 1
	}
	cfg, err := config.LoadCenter()
	if err != nil {
		_, _ = fmt.Fprintln(out, "invalid migration configuration")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		_, _ = fmt.Fprintln(out, err)
		return 1
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, args[0]); err != nil {
		_, _ = fmt.Fprintln(out, err)
		return 1
	}
	_, _ = fmt.Fprintln(out, "migration", args[0], "complete")
	return 0
}
