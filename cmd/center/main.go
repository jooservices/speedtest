package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/jooservices/speedtest"
	"github.com/jooservices/speedtest/internal/center/server"
	"github.com/jooservices/speedtest/internal/shared/config"
)

func main() {
	os.Exit(mainRun(os.Args[1:], os.Stdout, os.Stderr))
}

func mainRun(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewJSONHandler(stdout, nil))
	if err := run(ctx, args, logger, stderr); err != nil {
		logger.Error("command center exited", "error", err)
		return 1
	}
	return 0
}

func run(ctx context.Context, args []string, logger *slog.Logger, stderr io.Writer) error {
	cfg, err := config.LoadCenter()
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	flags := flag.NewFlagSet("center", flag.ContinueOnError)
	flags.SetOutput(stderr)
	uiDir := flags.String("ui-dir", cfg.UIDir, "serve UI files from this directory instead of the embedded UI")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}

	ui, uiSource, err := uiFilesystem(*uiDir)
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen on configured address: %w", err)
	}

	handler := server.NewHandler(server.Options{
		Logger: logger,
		UI:     ui,
	})
	logger.Info("command center listening",
		"address", listener.Addr().String(),
		"ui_source", uiSource,
	)
	if err := server.Serve(ctx, listener, handler); err != nil {
		return fmt.Errorf("serve command center: %w", err)
	}
	logger.Info("command center stopped")
	return nil
}

func uiFilesystem(directory string) (fs.FS, string, error) {
	if directory != "" {
		info, err := os.Stat(directory)
		if err != nil {
			return nil, "", fmt.Errorf("read UI directory: %w", err)
		}
		if !info.IsDir() {
			return nil, "", errors.New("UI path must be a directory")
		}
		return os.DirFS(directory), "disk", nil
	}

	ui, err := fs.Sub(speedtest.UI, "ui")
	if err != nil {
		return nil, "", fmt.Errorf("open embedded UI: %w", err)
	}
	return ui, "embedded", nil
}
