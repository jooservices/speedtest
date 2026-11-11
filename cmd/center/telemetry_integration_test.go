//go:build integration

package main

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jooservices/speedtest/internal/shared/config"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

func TestTelemetryCompositionIntegration(t *testing.T) {
	pool := rotationPool(t)
	t.Setenv("SPEEDTEST_METRICS_TOKEN", "")
	t.Setenv("SPEEDTEST_GEO_IPAPI_ENABLED", "false")
	t.Setenv("SPEEDTEST_GEO_DBIP_AUTO_UPDATE", "false")
	t.Setenv("SPEEDTEST_GEO_IPINFO_ENABLED", "false")
	for _, name := range []string{"IPWHO", "FREEIPAPI", "GEOJS", "RIPESTAT", "IPAPICO", "IPGEOLOCATION"} {
		t.Setenv("SPEEDTEST_GEO_"+name+"_ENABLED", "false")
	}
	t.Setenv("SPEEDTEST_GEO_IPWHO_ENABLED", "true")
	t.Setenv("SPEEDTEST_GEO_IPWHO_URL", "https://ipwho.is")
	t.Setenv("SPEEDTEST_GEO_CONSENSUS", "false")
	t.Setenv("SPEEDTEST_GEOLITE2_LICENSE_KEY", "")
	t.Setenv("SPEEDTEST_GEO_IPINFO_TOKEN", "")
	cfg, err := config.LoadCenter()
	if err != nil {
		t.Fatal(err)
	}
	cfg.AppKey = []byte(strings.Repeat("t", 32))
	cfg.PublicURL = "https://center.example"
	cfg.ReleaseDir = t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc, cleanup, err := buildServices(t.Context(), pool, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if svc.Telemetry == nil || svc.Installer == nil || svc.OwnerNotifications == nil || svc.AccessTokens == nil {
		t.Fatal("operational services not composed")
	}
	defer func() {
		if err := svc.Installer.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	handler, err := buildHandler(cfg, svc, logger, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/metrics", nil)
	r.RemoteAddr = "127.0.0.1:9000"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	parser := expfmt.NewTextParser(model.LegacyValidation)
	f, err := parser.TextToMetricFamilies(strings.NewReader(w.Body.String()))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"speedtest_build_info", "speedtest_db_pool_max_connections", "speedtest_nodes_enabled", "speedtest_job_runs_total"} {
		if f[name] == nil {
			t.Fatal("missing composed collector", name)
		}
	}
	background, stop := context.WithCancel(t.Context())
	stop()
	svc.Background(background)
	t.Setenv("SPEEDTEST_GEO_DBIP_AUTO_UPDATE", "invalid")
	if _, _, err := buildServices(t.Context(), pool, cfg, logger); err == nil {
		t.Fatal("invalid DB-IP settings were accepted")
	}
	t.Setenv("SPEEDTEST_GEO_DBIP_AUTO_UPDATE", "false")
	t.Setenv("SPEEDTEST_GEO_BACKFILL_BATCH_SIZE", "101")
	if _, _, err := buildServices(t.Context(), pool, cfg, logger); err == nil {
		t.Fatal("invalid geolocation backfill settings were accepted")
	}
	t.Setenv("SPEEDTEST_GEO_BACKFILL_BATCH_SIZE", "100")
	cfg.PublicURL = "invalid-url"
	if _, _, err := buildServices(t.Context(), pool, cfg, logger); err == nil {
		t.Fatal("invalid installer composition accepted")
	}
}
