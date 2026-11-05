package main

import (
	"bufio"
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
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest"
	"github.com/jooservices/speedtest/internal/center/agentapi"
	"github.com/jooservices/speedtest/internal/center/alerts"
	"github.com/jooservices/speedtest/internal/center/audit"
	"github.com/jooservices/speedtest/internal/center/auth"
	"github.com/jooservices/speedtest/internal/center/dashboard"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/geo"
	"github.com/jooservices/speedtest/internal/center/install"
	"github.com/jooservices/speedtest/internal/center/installer"
	"github.com/jooservices/speedtest/internal/center/ipapi"
	"github.com/jooservices/speedtest/internal/center/ipwatch"
	"github.com/jooservices/speedtest/internal/center/jobs"
	"github.com/jooservices/speedtest/internal/center/metrics"
	"github.com/jooservices/speedtest/internal/center/nodes"
	"github.com/jooservices/speedtest/internal/center/notify"
	"github.com/jooservices/speedtest/internal/center/ownerapi"
	"github.com/jooservices/speedtest/internal/center/server"
	"github.com/jooservices/speedtest/internal/center/settings"
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
	case len(args) > 0 && args[0] == "rotate-app-key":
		err = runRotateAppKey(ctx, args[1:], os.Stdin, stdout)
	case len(args) > 0 && args[0] == "seed-admin":
		err = runSeedAdmin(ctx, args[1:], stdout)
	case len(args) > 0 && args[0] == "reset-owner-password":
		err = runResetOwnerPassword(ctx, args[1:], os.Stdin, stdout)
	case len(args) > 0 && args[0] == "create-owner":
		err = runCreateOwner(ctx, args[1:], os.Stdin, stdout)
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
	return serve(ctx, args, logger, stderr, cfg, services{})
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
	watcher, closeGeo, err := ipwatch.Open(logger)
	if err != nil {
		return err
	}
	defer func() { _ = closeGeo() }()
	nodeService := nodes.NewService(pool, cfg.AppKey)
	owner := &ownerapi.Store{DB: pool}
	statsStore := &stats.Store{DB: pool}
	// Without APP_KEY nothing secret can be stored, so the settings API stays off, like node issuance.
	var settingsStore settings.Store
	var notifications *notify.Store
	if len(cfg.AppKey) == 32 {
		cipher, err := settings.NewCipherFromKey(cfg.AppKey)
		if err != nil {
			return fmt.Errorf("invalid APP_KEY: %w", err)
		}
		preferences := settings.NewPostgresStore(pool, cipher, settings.Defaults(cfg.PublicURL))
		settingsStore = preferences
		notifications = &notify.Store{DB: pool, Settings: preferences, Logger: logger, Sender: &notify.Transport{SMTPAddr: cfg.SMTPAddr, SMTPFrom: cfg.SMTPFrom, SMTPUser: cfg.SMTPUser, TelegramURL: cfg.TelegramURL}}
	} else {
		logger.Warn("APP_KEY is not set; the settings and notification APIs are disabled")
	}
	alertStore := &alerts.Store{DB: pool, AppKey: cfg.AppKey, Logger: logger}
	retention := &jobs.Retention{DB: pool, Logger: logger}
	var installerService *installer.Service
	if cfg.PublicURL != "" && cfg.ReleaseDir != "" && len(cfg.AppKey) == 32 {
		installerService, err = installer.NewService(ctx, installer.ServiceOptions{
			Database: pool, Enrollment: nodeService, CenterURL: cfg.PublicURL, AppKey: cfg.AppKey, Logger: logger,
		})
		if err != nil {
			return err
		}
	}
	var notificationAPI server.OwnerNotificationsStore
	if notifications != nil {
		notificationAPI = notifications
	}
	return serve(ctx, args, logger, stderr, cfg, services{
		OwnerNotifications: notificationAPI,
		Auth:               service,
		NodeCredential:     nodeService,
		Pinger:             poolPinger{pool: pool},
		Metrics:            metrics.Collector{DB: pool},
		Agent:              &agentapi.Postgres{Pool: pool, Hook: agentapi.ResultHookChain{watcher, alertStore}},
		OwnerNodes:         owner,
		OwnerResults:       owner,
		OwnerSchedules:     owner,
		OwnerStats:         statsStore,
		Settings:           settingsStore,
		OwnerIP:            &ipapi.Store{DB: pool, Resolver: geo.NewResolver(geo.NewPostgresCache(pool), watcher.Offline, watcher.Fallbacks...)},
		OwnerDashboard:     &dashboard.Cached{Source: &dashboard.Store{DB: pool, Nodes: owner, Stats: statsStore, Results: owner, Alerts: alertStore}, TTL: 5 * time.Second},
		OwnerAlerts:        alertStore,
		OwnerJobs:          &jobs.Registry{DB: pool},
		Audit:              &audit.Store{DB: pool},
		Background: func(ctx context.Context) {
			if notifications != nil {
				go notifications.Loop(ctx)
			}
			go retention.Loop(ctx)
			alertStore.Loop(ctx)
		},
		Installer: installerService,
	})
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

// services are the database-backed collaborators of the server; any may be nil in tests.
type services struct {
	Auth               *auth.Service
	NodeCredential     server.NodeCredentialService
	Pinger             server.Pinger
	Metrics            server.MetricsSource
	Agent              agentapi.Store
	OwnerNodes         server.OwnerNodesStore
	OwnerResults       server.OwnerResultsStore
	OwnerSchedules     server.OwnerSchedulesStore
	OwnerStats         server.OwnerStatsStore
	Settings           settings.Store
	OwnerIP            server.OwnerIPStore
	OwnerNotifications server.OwnerNotificationsStore
	OwnerDashboard     server.OwnerDashboardStore
	OwnerAlerts        server.OwnerAlertsStore
	OwnerJobs          server.OwnerJobsStore
	Audit              server.AuditStore
	Installer          *installer.Service
	// Background runs until the context ends, for example the daily retention job.
	Background func(context.Context)
}

func serve(ctx context.Context, args []string, logger *slog.Logger, stderr io.Writer, cfg config.CenterConfig, svc services) error {
	if svc.Installer != nil {
		defer func() {
			closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = svc.Installer.Close(closeCtx)
		}()
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

	defer func() { _ = listener.Close() }()

	var installHandler http.Handler
	if cfg.PublicURL != "" {
		installHandler, err = install.Handler(install.Config{PublicURL: cfg.PublicURL, ReleaseDir: cfg.ReleaseDir})
		if err != nil {
			return err
		}
	}

	handler := server.NewHandler(server.Options{
		Install:            installHandler,
		OwnerNodes:         svc.OwnerNodes,
		OwnerResults:       svc.OwnerResults,
		OwnerSchedules:     svc.OwnerSchedules,
		OwnerStats:         svc.OwnerStats,
		Settings:           svc.Settings,
		OwnerIP:            svc.OwnerIP,
		OwnerNotifications: svc.OwnerNotifications,
		OwnerDashboard:     svc.OwnerDashboard,
		OwnerAlerts:        svc.OwnerAlerts,
		OwnerJobs:          svc.OwnerJobs,
		Audit:              svc.Audit,
		Installer:          svc.Installer,
		AgentAPI:           svc.Agent,
		Logger:             logger,
		UI:                 ui,
		Pinger:             svc.Pinger,
		Metrics:            svc.Metrics,
		MetricsToken:       os.Getenv("SPEEDTEST_METRICS_TOKEN"),
		Auth:               svc.Auth,
		RequireHTTPS:       cfg.AppEnv != "local" && cfg.AppEnv != "test",
		TrustedProxyCIDRs:  cfg.TrustedProxyCIDRs,
		NodeCredentials:    svc.NodeCredential,
		AgentRateLimit:     cfg.AgentRequestsPerMinute,
	})
	if svc.Background != nil {
		go svc.Background(ctx)
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

// runCreateOwner creates the single owner account of a production install. The email comes from
// SPEEDTEST_OWNER_EMAIL and the password from the first line of standard input, so it never appears
// in the process list, shell history or logs. It refuses to run when a user already exists.
func runCreateOwner(ctx context.Context, args []string, input io.Reader, stdout io.Writer) error {
	if len(args) != 0 {
		return errors.New("usage: SPEEDTEST_OWNER_EMAIL=you@example.com center create-owner < password-file")
	}
	email := strings.ToLower(strings.TrimSpace(os.Getenv("SPEEDTEST_OWNER_EMAIL")))
	if email == "" || !strings.Contains(email, "@") || strings.ContainsAny(email, " \t\r\n") {
		return errors.New("SPEEDTEST_OWNER_EMAIL must be a valid email address")
	}
	line, _ := bufio.NewReader(io.LimitReader(input, 4096)).ReadString('\n')
	password := strings.TrimRight(line, "\r\n")
	if password == auth.DefaultAdminPassword {
		return errors.New("the default password is not allowed")
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return errors.New("the password does not meet the password policy (read it from the first line of standard input)")
	}
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
	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := verifyLocalDatabaseIdentity(ctx, pool, cfg); err != nil {
		return err
	}
	var users int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&users); err != nil {
		return errors.New("could not check for existing users (are migrations applied?)")
	}
	if users > 0 {
		return errors.New("an owner account already exists; nothing was changed")
	}
	store, err := auth.NewPostgresStore(pool)
	if err != nil {
		return err
	}
	if err := store.SeedAdmin(ctx, email, hash); err != nil {
		return errors.New("could not create the owner account")
	}
	_, _ = fmt.Fprintln(stdout, "owner account created")
	return nil
}

// runResetOwnerPassword sets a new owner password and signs every session out. It is the recovery path for a
// forgotten password: whoever can run it already has shell and database access to the server. The new password
// is the first line of standard input; the account is SPEEDTEST_OWNER_EMAIL, or the only account when unset.
func runResetOwnerPassword(ctx context.Context, args []string, input io.Reader, stdout io.Writer) error {
	if len(args) != 0 {
		return errors.New("usage: [SPEEDTEST_OWNER_EMAIL=you@example.com] center reset-owner-password < password-file")
	}
	line, _ := bufio.NewReader(io.LimitReader(input, 4096)).ReadString('\n')
	password := strings.TrimRight(line, "\r\n")
	if password == auth.DefaultAdminPassword {
		return errors.New("the default password is not allowed")
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return errors.New("the password does not meet the password policy (read it from the first line of standard input)")
	}
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
	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := verifyLocalDatabaseIdentity(ctx, pool, cfg); err != nil {
		return err
	}
	email := strings.ToLower(strings.TrimSpace(os.Getenv("SPEEDTEST_OWNER_EMAIL")))
	tx, err := pool.Begin(ctx)
	if err != nil {
		return errors.New("could not start the password reset")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id string
	if email == "" {
		var users int
		if err := tx.QueryRow(ctx, `SELECT count(*), coalesce(min(id::text), '') FROM users`).Scan(&users, &id); err != nil || users != 1 {
			return errors.New("set SPEEDTEST_OWNER_EMAIL: there is not exactly one account")
		}
	} else if err := tx.QueryRow(ctx, `SELECT id::text FROM users WHERE email=$1`, email).Scan(&id); err != nil {
		return errors.New("no account has that email address")
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET password_hash=$2, updated_at=now() WHERE id=$1`, id, hash); err != nil {
		return errors.New("could not set the password")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sessions`); err != nil {
		return errors.New("could not sign existing sessions out")
	}
	if err := tx.Commit(ctx); err != nil {
		return errors.New("could not save the new password")
	}
	_, _ = fmt.Fprintln(stdout, "owner password reset; all sessions were signed out")
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
