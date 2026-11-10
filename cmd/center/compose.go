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
	"github.com/jooservices/speedtest/internal/center/geo/budget"
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
	operational := telemetry.New(version)
	store, err := auth.NewPostgresStore(pool)
	if err != nil {
		return services{}, nil, err
	}
	authService, err := newAuthService(store, cfg)
	if err != nil {
		return services{}, nil, err
	}
	watcher, closeGeo, err := ipwatch.OpenObserved(logger, pool, operational.GeoProvider)
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
	backfillConfig, err := geo.BackfillConfigFromEnv()
	if err != nil {
		cleanup()
		return services{}, nil, err
	}
	var batch geo.BatchLookup
	if watcher.Providers.IPAPIEnabled {
		batch, err = geo.NewIPAPIProvider(watcher.Providers)
		if err != nil {
			cleanup()
			return services{}, nil, err
		}
	}
	backfillFallbacks := make([]geo.Fallback, 0, len(watcher.Fallbacks))
	for _, provider := range watcher.Fallbacks {
		if provider.Source() != "ip-api" {
			backfillFallbacks = append(backfillFallbacks, provider)
		}
	}
	backfill := &geo.Backfill{DB: pool, Cache: geo.NewPostgresCache(pool),
		Resolver: geo.NewResolver(geo.NewPostgresCache(pool), watcher.Offline, backfillFallbacks...).WithPurpose(budget.Background),
		Batch:    batch, MaxPerRun: backfillConfig.MaxPerRun, BatchSize: backfillConfig.BatchSize, Pause: backfillConfig.Pause,
		HourUTC: backfillConfig.HourUTC, Logger: logger, ObserveRun: func(ok bool) { operational.Job("geo_backfill", ok) }}
	installerService, err := buildInstaller(ctx, pool, cfg, nodeService, logger)
	if err != nil {
		cleanup()
		return services{}, nil, err
	}
	var notificationAPI server.OwnerNotificationsStore
	if notifications != nil {
		notificationAPI = notifications
	}
	return services{
		Telemetry:          operational,
		OwnerNotifications: notificationAPI,
		Auth:               authService,
		NodeCredential:     nodeService,
		Pinger:             poolPinger{pool: pool},
		Metrics:            metrics.Collector{DB: pool},
		Agent:              &agentapi.Postgres{Pool: pool, Releases: releases, Hook: agentapi.ResultHookChain{watcher, alertStore}},
		OwnerNodes:         owner,
		OwnerResults:       owner,
		OwnerSchedules:     owner,
		OwnerStats:         statsStore,
		Settings:           preferences,
		OwnerIP:            &ipapi.Store{DB: pool, Resolver: geo.NewResolver(geo.NewPostgresCache(pool), watcher.Offline, watcher.Fallbacks...), ProviderSettings: &watcher.Providers},
		OwnerDashboard:     &dashboard.Cached{Source: &dashboard.Store{DB: pool, Nodes: owner, Stats: statsStore, Results: owner, Alerts: alertStore}, TTL: 5 * time.Second},
		OwnerAlerts:        alertStore,
		OwnerJobs:          &jobs.Registry{DB: pool},
		Audit:              &audit.Store{DB: pool},
		Installer:          installerService,
		Background: func(ctx context.Context) {
			if notifications != nil {
				go notifications.Loop(ctx)
			}
			go retention.Loop(ctx)
			go backfill.Loop(ctx)
			if watcher.Reader != nil {
				// GeoLite2 (weekly, needs a licence key) or DB-IP Lite (monthly, free): the first one configured wins.
				if os.Getenv("SPEEDTEST_GEOLITE2_LICENSE_KEY") != "" {
					go watcher.Reader.Run(ctx, geo.EnvironmentSettings{})
				}
				go watcher.Reader.RunDBIP(ctx, dbip)
				if os.Getenv("SPEEDTEST_GEO_IPINFO_TOKEN") != "" {
					go watcher.Reader.RunIPInfo(ctx)
				}
			}
			alertStore.Loop(ctx)
		},
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
