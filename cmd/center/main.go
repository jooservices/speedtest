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
	"os/signal"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest"
	"github.com/jooservices/speedtest/internal/center/agentapi"
	"github.com/jooservices/speedtest/internal/center/auth"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/install"
	"github.com/jooservices/speedtest/internal/center/jobs"
	"github.com/jooservices/speedtest/internal/center/nodes"
	"github.com/jooservices/speedtest/internal/center/ownerapi"
	"github.com/jooservices/speedtest/internal/center/server"
	"github.com/jooservices/speedtest/internal/center/stats"
	"github.com/jooservices/speedtest/internal/shared/config"
)

func main() {
	os.Exit(mainRun(os.Args[1:], os.Stdout, os.Stderr))
}

func mainRun(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewJSONHandler(stdout, nil))
	var err error
	switch {
	case len(args) > 0 && args[0] == "seed-admin":
		err = runSeedAdmin(ctx, args[1:], stdout)
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

func run(ctx context.Context, args []string, logger *slog.Logger, stderr io.Writer) error {
	cfg, err := config.LoadCenter()
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	return serve(ctx, args, logger, stderr, cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil)
}

func runWithDatabase(ctx context.Context, args []string, logger *slog.Logger, stderr io.Writer) error {
	if err := loadDotEnv(); err != nil {
		return err
	}
	cfg, err := config.LoadCenter()
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	if err := validateLocalDatabaseName(cfg); err != nil {
		return err
	}
	if err := validateCenterArguments(args, cfg, stderr); err != nil {
		return err
	}
	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := verifyLocalDatabaseIdentity(ctx, pool, cfg); err != nil {
		return err
	}
	store, err := auth.NewPostgresStore(pool)
	if err != nil {
		return err
	}
	service, err := newAuthService(store, cfg)
	if err != nil {
		return err
	}
	nodeService := nodes.NewService(pool, cfg.AppKey)
	owner := &ownerapi.Store{DB: pool}
	return serve(ctx, args, logger, stderr, cfg, service, nodeService, poolPinger{pool: pool}, &agentapi.Postgres{Pool: pool}, owner, owner, owner, &stats.Store{DB: pool}, (&jobs.Retention{DB: pool, Logger: logger}).Loop)
}

func validateCenterArguments(args []string, cfg config.CenterConfig, stderr io.Writer) error {
	flags := flag.NewFlagSet("center", flag.ContinueOnError)
	flags.SetOutput(stderr)
	uiDir := flags.String("ui-dir", cfg.UIDir, "serve UI files from this directory instead of the embedded UI")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	_, _, err := uiFilesystem(*uiDir)
	return err
}

type poolPinger struct{ pool *pgxpool.Pool }

func (p poolPinger) PingContext(ctx context.Context) error { return p.pool.Ping(ctx) }

func serve(ctx context.Context, args []string, logger *slog.Logger, stderr io.Writer, cfg config.CenterConfig, authService *auth.Service, nodeService server.NodeCredentialService, pinger server.Pinger, agentStore agentapi.Store, ownerNodes server.OwnerNodesStore, ownerResults server.OwnerResultsStore, ownerSchedules server.OwnerSchedulesStore, ownerStats server.OwnerStatsStore, background func(context.Context)) error {

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

	var installHandler http.Handler
	if cfg.PublicURL != "" {
		installHandler, err = install.Handler(install.Config{PublicURL: cfg.PublicURL, ReleaseDir: cfg.ReleaseDir})
		if err != nil {
			return err
		}
	}

	handler := server.NewHandler(server.Options{
		Install:           installHandler,
		OwnerNodes:        ownerNodes,
		OwnerResults:      ownerResults,
		OwnerSchedules:    ownerSchedules,
		OwnerStats:        ownerStats,
		AgentAPI:          agentStore,
		Logger:            logger,
		UI:                ui,
		Pinger:            pinger,
		Auth:              authService,
		RequireHTTPS:      cfg.AppEnv != "local" && cfg.AppEnv != "test",
		TrustedProxyCIDRs: cfg.TrustedProxyCIDRs,
		NodeCredentials:   nodeService,
		AgentRateLimit:    cfg.AgentRequestsPerMinute,
	})
	if background != nil {
		go background(ctx)
	}
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

func newAuthService(store auth.Store, cfg config.CenterConfig) (*auth.Service, error) {
	return auth.NewService(store, auth.Config{
		SessionTTL:       cfg.SessionTTL,
		LoginMaxAttempts: cfg.LoginMaxAttempts,
		LoginWindow:      cfg.LoginWindow,
	})
}

func runSeedAdmin(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) != 0 {
		return errors.New("usage: center seed-admin")
	}
	if err := loadDotEnv(); err != nil {
		return err
	}
	cfg, err := config.LoadCenter()
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	appEnv, configured := os.LookupEnv("APP_ENV")
	if err := validateExplicitDefaultSeedEnvironment(appEnv, configured); err != nil {
		return err
	}
	if err := validateLocalDatabaseName(cfg); err != nil {
		return err
	}
	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := verifyLocalDatabaseIdentity(ctx, pool, cfg); err != nil {
		return err
	}
	store, err := auth.NewPostgresStore(pool)
	if err != nil {
		return err
	}
	service, err := newAuthService(store, cfg)
	if err != nil {
		return err
	}
	if err := service.EnsureDefaultAdmin(ctx); err != nil {
		return errors.New("could not seed the default administrator")
	}
	_, _ = fmt.Fprintln(stdout, "default administrator ensured")
	return nil
}

func validateDefaultSeedEnvironment(appEnv string) error {
	if appEnv != "local" && appEnv != "test" {
		return errors.New("default administrator seed is allowed only when APP_ENV is local or test")
	}
	return nil
}

func validateExplicitDefaultSeedEnvironment(appEnv string, configured bool) error {
	if !configured || strings.TrimSpace(appEnv) == "" {
		return errors.New("APP_ENV must be explicitly set to local or test to seed the default administrator")
	}
	return validateDefaultSeedEnvironment(strings.ToLower(strings.TrimSpace(appEnv)))
}

func validateLocalDatabaseName(cfg config.CenterConfig) error {
	if cfg.AppEnv == "local" || cfg.AppEnv == "test" {
		if err := db.CheckTestDatabase(cfg.Database.Name); err != nil {
			return errors.New("local and test environments require speedtest_test or a Speedtest worktree database")
		}
	}
	return nil
}

func verifyLocalDatabaseIdentity(ctx context.Context, pool *pgxpool.Pool, cfg config.CenterConfig) error {
	if cfg.AppEnv != "local" && cfg.AppEnv != "test" {
		return nil
	}
	var name, marker string
	err := pool.QueryRow(ctx, `SELECT current_database(),shobj_description(oid,'pg_database')
		FROM pg_database WHERE datname=current_database()`).Scan(&name, &marker)
	if err != nil || name != cfg.Database.Name || marker != "workspaces-project:speedtest" {
		return errors.New("local/test Speedtest database identity verification failed")
	}
	return nil
}

func loadDotEnv() error {
	if err := godotenv.Load(".env"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot load .env")
	}
	return nil
}

func containsHelp(args []string) bool {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
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
