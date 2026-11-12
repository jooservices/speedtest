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
	"net/http"
	"os"
	"time"

	"github.com/jooservices/speedtest"
	"github.com/jooservices/speedtest/internal/center/install"
	"github.com/jooservices/speedtest/internal/center/server"
	"github.com/jooservices/speedtest/internal/shared/config"
)

func run(ctx context.Context, args []string, logger *slog.Logger, stderr io.Writer) error {
	cfg, err := config.LoadCenter()
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	return serve(ctx, args, logger, stderr, cfg, services{})
}

// runWithDatabase starts the center against the configured PostgreSQL database.
func runWithDatabase(ctx context.Context, args []string, logger *slog.Logger, stderr io.Writer) error {
	pool, cfg, err := openCenterDatabase(ctx, func(cfg config.CenterConfig) error { return validateCenterArguments(args, cfg, stderr) })
	if err != nil {
		return err
	}
	defer pool.Close()
	svc, cleanup, err := buildServices(ctx, pool, cfg, logger)
	if err != nil {
		return err
	}
	defer cleanup()
	return serve(ctx, args, logger, stderr, cfg, svc)
}

// parseCenterFlags reads the command line shared by validation and serving; the result is the UI directory.
func parseCenterFlags(args []string, cfg config.CenterConfig, stderr io.Writer) (string, error) {
	flags := flag.NewFlagSet("center", flag.ContinueOnError)
	flags.SetOutput(stderr)
	uiDir := flags.String("ui-dir", cfg.UIDir, "serve UI files from this directory instead of the embedded UI")
	if err := flags.Parse(args); err != nil {
		return "", err
	}
	if flags.NArg() != 0 {
		return "", errors.New("unexpected positional arguments")
	}
	return *uiDir, nil
}

func validateCenterArguments(args []string, cfg config.CenterConfig, stderr io.Writer) error {
	uiDir, err := parseCenterFlags(args, cfg, stderr)
	if err != nil {
		return err
	}
	_, _, err = uiFilesystem(uiDir)
	return err
}

// serve runs the HTTP server until ctx ends.
func serve(ctx context.Context, args []string, logger *slog.Logger, stderr io.Writer, cfg config.CenterConfig, svc services) error {
	if svc.Installer != nil {
		defer func() {
			closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = svc.Installer.Close(closeCtx)
		}()
	}
	uiDir, err := parseCenterFlags(args, cfg, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	ui, uiSource, err := uiFilesystem(uiDir)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen on configured address: %w", err)
	}
	defer func() { _ = listener.Close() }()
	handler, err := buildHandler(cfg, svc, logger, ui)
	if err != nil {
		return err
	}
	if svc.Background != nil {
		go svc.Background(ctx)
	}
	logger.Info("command center listening", "address", listener.Addr().String(), "ui_source", uiSource)
	if err := server.Serve(ctx, listener, handler); err != nil {
		return fmt.Errorf("serve command center: %w", err)
	}
	logger.Info("command center stopped")
	return nil
}

// buildHandler maps the services and the configuration to the HTTP handler.
func buildHandler(cfg config.CenterConfig, svc services, logger *slog.Logger, ui fs.FS) (http.Handler, error) {
	var installHandler http.Handler
	if cfg.PublicURL != "" {
		var err error
		if installHandler, err = install.Handler(install.Config{PublicURL: cfg.PublicURL, ReleaseDir: cfg.ReleaseDir}); err != nil {
			return nil, err
		}
	}
	return server.NewHandler(server.Options{
		Install:              installHandler,
		OwnerNodes:           svc.OwnerNodes,
		OwnerResults:         svc.OwnerResults,
		OwnerSchedules:       svc.OwnerSchedules,
		OwnerStats:           svc.OwnerStats,
		OwnerISP:             svc.OwnerISP,
		OwnerReport:          svc.OwnerReport,
		Settings:             svc.Settings,
		OwnerIP:              svc.OwnerIP,
		OwnerNotifications:   svc.OwnerNotifications,
		NotificationEvents:   svc.NotificationEvents,
		OwnerDashboard:       svc.OwnerDashboard,
		OwnerAlerts:          svc.OwnerAlerts,
		OwnerJobs:            svc.OwnerJobs,
		Audit:                svc.Audit,
		AccessTokens:         svc.AccessTokens,
		Installer:            svc.Installer,
		AgentAPI:             svc.Agent,
		Logger:               logger,
		UI:                   ui,
		Pinger:               svc.Pinger,
		Metrics:              svc.Metrics,
		Telemetry:            svc.Telemetry,
		MetricsToken:         os.Getenv("SPEEDTEST_METRICS_TOKEN"),
		Auth:                 svc.Auth,
		RequireHTTPS:         cfg.AppEnv != "local" && cfg.AppEnv != "test",
		TrustedProxyCIDRs:    cfg.TrustedProxyCIDRs,
		NodeCredentials:      svc.NodeCredential,
		AgentRateLimit:       cfg.AgentRequestsPerMinute,
		AccessTokenRateLimit: cfg.APITokenRequestsPerMinute,
		AccessTokenBurst:     max(1, cfg.APITokenRequestsPerMinute/6),
	}), nil
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
