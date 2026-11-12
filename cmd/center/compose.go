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
	"github.com/jooservices/speedtest/internal/center/apitoken"
	"github.com/jooservices/speedtest/internal/center/audit"
	"github.com/jooservices/speedtest/internal/center/auth"
	"github.com/jooservices/speedtest/internal/center/dashboard"
	"github.com/jooservices/speedtest/internal/center/geo"
	"github.com/jooservices/speedtest/internal/center/installer"
	"github.com/jooservices/speedtest/internal/center/ipapi"
	"github.com/jooservices/speedtest/internal/center/ipwatch"
	"github.com/jooservices/speedtest/internal/center/isp"
	"github.com/jooservices/speedtest/internal/center/jobs"
	"github.com/jooservices/speedtest/internal/center/metrics"
	"github.com/jooservices/speedtest/internal/center/nodes"
	"github.com/jooservices/speedtest/internal/center/notify"
	"github.com/jooservices/speedtest/internal/center/ownerapi"
	"github.com/jooservices/speedtest/internal/center/release"
	"github.com/jooservices/speedtest/internal/center/report"
	"github.com/jooservices/speedtest/internal/center/server"
	"github.com/jooservices/speedtest/internal/center/settings"
	"github.com/jooservices/speedtest/internal/center/stats"
	"github.com/jooservices/speedtest/internal/center/telemetry"
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
	OwnerServers       server.OwnerServersStore
	OwnerResults       server.OwnerResultsStore
	OwnerSchedules     server.OwnerSchedulesStore
	OwnerStats         server.OwnerStatsStore
	OwnerISP           server.OwnerISPStore
	OwnerReport        server.OwnerReportStore
	Settings           settings.Store
	OwnerIP            server.OwnerIPStore
	OwnerNotifications server.OwnerNotificationsStore
	NotificationEvents server.NotificationEvents
	OwnerDashboard     server.OwnerDashboardStore
	OwnerAlerts        server.OwnerAlertsStore
	OwnerJobs          server.OwnerJobsStore
	Audit              server.AuditStore
	AccessTokens       server.AccessTokenStore
	Installer          *installer.Service
	// Background runs until the context ends, for example the daily retention job.
	Background func(context.Context)
}

func newAuthService(store auth.Store, cfg config.CenterConfig) (*auth.Service, error) {
	return auth.NewService(store, auth.Config{
		SessionTTL:       cfg.SessionTTL,
		LoginMaxAttempts: cfg.LoginMaxAttempts,
		LoginWindow:      cfg.LoginWindow,
		AppKey:           cfg.AppKey,
	})
}

func newOperationalStores(pool *pgxpool.Pool, cfg config.CenterConfig, logger *slog.Logger, notifications *notify.Store) (*telemetry.Telemetry, *alerts.Store, *jobs.Retention) {
	operational := telemetry.New(version)
	operational.RegisterDatabase(pool)
	if notifications != nil {
		notifications.ObserveRun = func(ok bool) { operational.Job("notification", ok) }
	}
	alertStore := &alerts.Store{DB: pool, AppKey: cfg.AppKey, Logger: logger, ObserveRun: func(ok bool) { operational.Job("alerts", ok) }}
	retention := &jobs.Retention{DB: pool, Logger: logger, ObserveRun: func(ok bool) { operational.Job("retention", ok) }}
	return operational, alertStore, retention
}

func backgroundServices(notifications *notify.Store, retention *jobs.Retention, mailer *report.Mailer, watcher *ipwatch.Watcher, dbip geo.DBIPConfig, alertStore *alerts.Store) func(context.Context) {
	return func(ctx context.Context) {
		if notifications != nil {
			go notifications.Loop(ctx)
		}
		go retention.Loop(ctx)
		if mailer != nil {
			go mailer.Loop(ctx)
		}
		if watcher.Reader != nil {
			// GeoLite2 (weekly, needs a licence key) or DB-IP Lite (monthly, free): the first one configured wins.
			if os.Getenv("SPEEDTEST_GEOLITE2_LICENSE_KEY") != "" {
				go watcher.Reader.Run(ctx, geo.EnvironmentSettings{})
			}
			go watcher.Reader.RunDBIP(ctx, dbip)
		}
		alertStore.Loop(ctx)
	}
}

func ownerNotifications(notifications *notify.Store) server.OwnerNotificationsStore {
	if notifications == nil {
		return nil
	}
	return notifications
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
	preferences, notifications, err := buildPreferences(pool, cfg, logger)
	if err != nil {
		cleanup()
		return services{}, nil, err
	}
	owner.Events = notifications
	operational, alertStore, retention := newOperationalStores(pool, cfg, logger, notifications)
	mailer := buildReportMailer(pool, notifications, logger)
	dbip, err := geo.DBIPConfigFromEnv()
	if err != nil {
		cleanup()
		return services{}, nil, err
	}
	if notifications != nil {
		retention.OnComplete = func(ctx context.Context, result jobs.Result) error {
			return notifications.PublishEvent(ctx, "platform.retention_ran", "", map[string]string{
				"count": fmt.Sprint(result.Deleted + result.AuditDeleted), "duration": result.Duration.String(), "value": fmt.Sprint(result.RetentionDays),
			})
		}
	}
	installerService, err := buildInstaller(ctx, pool, cfg, nodeService, logger)
	if err != nil {
		cleanup()
		return services{}, nil, err
	}
	return services{
		Telemetry:          operational,
		OwnerNotifications: ownerNotifications(notifications),
		NotificationEvents: notifications,
		Auth:               authService,
		NodeCredential:     nodeService,
		Pinger:             poolPinger{pool: pool},
		Metrics:            metrics.Collector{DB: pool},
		Agent:              &agentapi.Postgres{Pool: pool, Releases: releases, Hook: agentapi.ResultHookChain{watcher, alertStore}, Events: notifications},
		OwnerNodes:         owner,
		OwnerServers:       owner,
		OwnerResults:       owner,
		OwnerSchedules:     owner,
		OwnerStats:         statsStore,
		OwnerISP:           &isp.Store{DB: pool},
		OwnerReport:        &report.Store{DB: pool},
		Settings:           preferences,
		OwnerIP:            &ipapi.Store{DB: pool, Resolver: geo.NewResolver(geo.NewPostgresCache(pool), watcher.Offline, watcher.Fallbacks...).WithConsensus(watcher.Consensus, logger)},
		OwnerDashboard:     &dashboard.Cached{Source: &dashboard.Store{DB: pool, Nodes: owner, Stats: statsStore, Results: owner, Alerts: alertStore}, TTL: 5 * time.Second},
		OwnerAlerts:        alertStore,
		OwnerJobs:          &jobs.Registry{DB: pool},
		Audit:              &audit.Store{DB: pool},
		AccessTokens:       &apitoken.Store{DB: pool},
		Installer:          installerService,
		Background:         backgroundServices(notifications, retention, mailer, watcher, dbip, alertStore),
	}, cleanup, nil
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

// buildReportMailer returns the scheduled monthly report e-mail, or nil unless the owner turned it on with
// SPEEDTEST_MONTHLY_REPORT_EMAIL=true (it needs the e-mail channel to be configured in the settings).
func buildReportMailer(pool *pgxpool.Pool, notifications *notify.Store, logger *slog.Logger) *report.Mailer {
	if notifications == nil || os.Getenv("SPEEDTEST_MONTHLY_REPORT_EMAIL") != "true" {
		return nil
	}
	return &report.Mailer{
		Reports: &report.Store{DB: pool},
		DB:      pool,
		Log:     logger,
		Send: func(ctx context.Context, build func(string) string) error {
			return notifications.SendText(ctx, "email", build)
		},
	}
}
