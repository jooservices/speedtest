package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jooservices/speedtest/internal/center/agentapi"
	"github.com/jooservices/speedtest/internal/center/alerts"
	"github.com/jooservices/speedtest/internal/center/audit"
	"github.com/jooservices/speedtest/internal/center/auth"
	"github.com/jooservices/speedtest/internal/center/dashboard"
	"github.com/jooservices/speedtest/internal/center/geo"
	"github.com/jooservices/speedtest/internal/center/installer"
	"github.com/jooservices/speedtest/internal/center/ipapi"
	"github.com/jooservices/speedtest/internal/center/ipwatch"
	"github.com/jooservices/speedtest/internal/center/jobs"
	"github.com/jooservices/speedtest/internal/center/metrics"
	"github.com/jooservices/speedtest/internal/center/nodes"
	"github.com/jooservices/speedtest/internal/center/notify"
	"github.com/jooservices/speedtest/internal/center/ownerapi"
	"github.com/jooservices/speedtest/internal/center/release"
	"github.com/jooservices/speedtest/internal/center/server"
	"github.com/jooservices/speedtest/internal/center/settings"
	"github.com/jooservices/speedtest/internal/center/stats"
	"github.com/jooservices/speedtest/internal/center/telemetry"
	"github.com/jooservices/speedtest/internal/center/usage"
	"github.com/jooservices/speedtest/internal/shared/config"
)

type poolPinger struct{ pool *pgxpool.Pool }

func (p poolPinger) PingContext(ctx context.Context) error { return p.pool.Ping(ctx) }

// services are the database-backed collaborators of the server; any may be nil in tests.
type services struct {
	Telemetry          *telemetry.Telemetry
	Auth               *auth.Service
	NodeCredential     server.NodeCredentialService
	Pinger             server.Pinger
	Metrics            server.MetricsSource
	Agent              agentapi.Store
	OwnerNodes         server.OwnerNodesStore
	OwnerResults       server.OwnerResultsStore
	OwnerSchedules     server.OwnerSchedulesStore
	OwnerStats         server.OwnerStatsStore
	OwnerUsage         *usage.Store
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

type serviceAssembly struct {
	services
	pool          *pgxpool.Pool
	owner         *ownerapi.Store
	stats         *stats.Store
	releases      *release.Catalog
	watcher       *ipwatch.Watcher
	alerts        *alerts.Store
	usage         *usage.Store
	notifications *notify.Store
	retention     *jobs.Retention
	dbip          geo.DBIPConfig
	logger        *slog.Logger
}

func newAuthService(store auth.Store, cfg config.CenterConfig) (*auth.Service, error) {
	return auth.NewService(store, auth.Config{
		SessionTTL:       cfg.SessionTTL,
		LoginMaxAttempts: cfg.LoginMaxAttempts,
		LoginWindow:      cfg.LoginWindow,
		AppKey:           cfg.AppKey,
	})
}

// buildServices wires every store and background job of the center. The returned function releases what was
// opened here (the offline location database).
func buildServices(ctx context.Context, pool *pgxpool.Pool, cfg config.CenterConfig, logger *slog.Logger) (services, func(), error) {
	store, err := auth.NewPostgresStore(pool)
	if err != nil {
		return services{}, nil, err
	}
	authService, err := newAuthService(store, cfg)
	if err != nil {
		return services{}, nil, err
	}
	watcher, closeGeo, err := ipwatch.Open(logger)
	if err != nil {
		return services{}, nil, err
	}
	cleanup := func() { _ = closeGeo() }
	nodeService := nodes.NewService(pool, cfg.AppKey)
	releases := &release.Catalog{Dir: cfg.ReleaseDir, PublicURL: cfg.PublicURL, Logger: logger}
	owner := &ownerapi.Store{DB: pool, Releases: releases}
	statsStore := &stats.Store{DB: pool}
	usageStore := &usage.Store{DB: pool}
	preferences, notifications, err := buildPreferences(pool, cfg, logger)
	if err != nil {
		cleanup()
		return services{}, nil, err
	}
	operational := telemetry.New(version)
	operational.RegisterDatabase(pool)
	if notifications != nil {
		notifications.ObserveRun = func(ok bool) { operational.Job("notification", ok) }
	}
	alertStore := &alerts.Store{DB: pool, AppKey: cfg.AppKey, Logger: logger, ObserveRun: func(ok bool) { operational.Job("alerts", ok) }}
	dbip, err := geo.DBIPConfigFromEnv()
	if err != nil {
		cleanup()
		return services{}, nil, err
	}
	retention := &jobs.Retention{DB: pool, Logger: logger, ObserveRun: func(ok bool) { operational.Job("retention", ok) }}
	installerService, err := buildInstaller(ctx, pool, cfg, nodeService, logger)
	if err != nil {
		cleanup()
		return services{}, nil, err
	}
	var notificationAPI server.OwnerNotificationsStore
	if notifications != nil {
		notificationAPI = notifications
	}
	assembled := serviceAssembly{
		services: baseServices(pool, operational, notificationAPI, authService, nodeService, owner, statsStore, usageStore, preferences, alertStore, installerService),
		pool:     pool, owner: owner, stats: statsStore, releases: releases, watcher: watcher,
		alerts: alertStore, usage: usageStore, notifications: notifications, retention: retention,
		dbip: dbip, logger: logger,
	}
	return assembleServices(assembled), cleanup, nil
}

func baseServices(pool *pgxpool.Pool, operational *telemetry.Telemetry, notifications server.OwnerNotificationsStore, authService *auth.Service, nodeService *nodes.Service, owner *ownerapi.Store, statsStore *stats.Store, usageStore *usage.Store, preferences settings.Store, alertStore *alerts.Store, installerService *installer.Service) services {
	return services{
		Telemetry: operational, OwnerNotifications: notifications, Auth: authService,
		NodeCredential: nodeService, Pinger: poolPinger{pool: pool}, Metrics: metrics.Collector{DB: pool},
		OwnerNodes: owner, OwnerResults: owner, OwnerSchedules: owner,
		OwnerStats: statsStore, OwnerUsage: usageStore, Settings: preferences,
		OwnerAlerts: alertStore, Installer: installerService,
	}
}

func assembleServices(d serviceAssembly) services {
	s := d.services
	s.Agent = &agentapi.Postgres{Pool: d.pool, Releases: d.releases, Hook: agentapi.ResultHookChain{d.watcher, d.alerts, d.usage}}
	s.OwnerIP = &ipapi.Store{DB: d.pool, Resolver: geo.NewResolver(geo.NewPostgresCache(d.pool), d.watcher.Offline, d.watcher.Fallbacks...).WithConsensus(d.watcher.Consensus, d.logger)}
	s.OwnerDashboard = &dashboard.Cached{Source: &dashboard.Store{DB: d.pool, Nodes: d.owner, Stats: d.stats, Results: d.owner, Alerts: d.alerts}, TTL: 5 * time.Second}
	s.OwnerJobs = &jobs.Registry{DB: d.pool}
	s.Audit = &audit.Store{DB: d.pool}
	s.Background = func(ctx context.Context) {
		go d.usage.Loop(ctx, d.logger)
		if d.notifications != nil {
			go d.notifications.Loop(ctx)
		}
		go d.retention.Loop(ctx)
		if d.watcher.Reader != nil {
			if os.Getenv("SPEEDTEST_GEOLITE2_LICENSE_KEY") != "" {
				go d.watcher.Reader.Run(ctx, geo.EnvironmentSettings{})
			}
			go d.watcher.Reader.RunDBIP(ctx, d.dbip)
		}
		d.alerts.Loop(ctx)
	}
	return s
}

// buildPreferences creates the settings store and the notification worker. Without APP_KEY nothing secret can be
// stored, so both stay off (like node issuance) and the center says so in its log.
func buildPreferences(pool *pgxpool.Pool, cfg config.CenterConfig, logger *slog.Logger) (settings.Store, *notify.Store, error) {
	if len(cfg.AppKey) != 32 {
		logger.Warn("APP_KEY is not set; the settings and notification APIs are disabled")
		return nil, nil, nil
	}
	cipher, err := settings.NewCipherFromKey(cfg.AppKey)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid APP_KEY: %w", err)
	}
	preferences := settings.NewPostgresStore(pool, cipher, settings.Defaults(cfg.PublicURL))
	sender := &notify.Transport{SMTPAddr: cfg.SMTPAddr, SMTPFrom: cfg.SMTPFrom, SMTPUser: cfg.SMTPUser, TelegramURL: cfg.TelegramURL}
	return preferences, &notify.Store{DB: pool, Settings: preferences, Logger: logger, Sender: sender}, nil
}

// buildInstaller creates the SSH installer when the deployment can serve the agent (public URL, release directory, APP_KEY).
func buildInstaller(ctx context.Context, pool *pgxpool.Pool, cfg config.CenterConfig, enrollment *nodes.Service, logger *slog.Logger) (*installer.Service, error) {
	if cfg.PublicURL == "" || cfg.ReleaseDir == "" || len(cfg.AppKey) != 32 {
		return nil, nil
	}
	return installer.NewService(ctx, installer.ServiceOptions{Database: pool, Enrollment: enrollment, CenterURL: cfg.PublicURL, AppKey: cfg.AppKey, Logger: logger})
}
