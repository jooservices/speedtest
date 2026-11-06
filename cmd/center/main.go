// Command center runs the Speedtest command center: the HTTP API, the UI and the background jobs. Sub-commands
// (seed-admin, create-owner, reset-owner-password, rotate-app-key) are in commands.go and rotate_app_key.go; the
// wiring of the services is in compose.go and the server lifecycle in app.go.
package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

// version is stamped by the release build (-ldflags "-X main.version=...").
var version = "dev"

func main() {
	os.Exit(mainRun(os.Args[1:], os.Stdout, os.Stderr))
}

// subcommands are the maintenance commands, looked up by the first argument.
var subcommands = map[string]func(ctx context.Context, args []string, stdout io.Writer) error{
	"rotate-app-key": func(ctx context.Context, args []string, stdout io.Writer) error {
		return runRotateAppKey(ctx, args, os.Stdin, stdout)
	},
	"seed-admin": runSeedAdmin,
	"reset-owner-password": func(ctx context.Context, args []string, stdout io.Writer) error {
		return runResetOwnerPassword(ctx, args, os.Stdin, stdout)
	},
	"create-owner": func(ctx context.Context, args []string, stdout io.Writer) error {
		return runCreateOwner(ctx, args, os.Stdin, stdout)
	},
}

func mainRun(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewJSONHandler(stdout, nil))
	flush := startErrorTracking("center", logger)
	defer flush()
	var err error
	switch {
	case len(args) > 0 && subcommands[args[0]] != nil:
		err = subcommands[args[0]](ctx, args[1:], stdout)
	case containsHelp(args):
		err = run(ctx, args, logger, stderr)
	default:
		err = runWithDatabase(ctx, args, logger, stderr)
	}
	if err != nil {
		logger.Error("command center exited", "error", err)
		return 1
	}
	return 0
}
