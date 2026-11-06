//go:build integration

package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jooservices/speedtest/internal/center/agentapi"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/metrics"
	"github.com/jooservices/speedtest/internal/center/nodes"
	"github.com/jooservices/speedtest/internal/center/telemetry"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// This operator flow crosses a real HTTP listener, credential middleware, ingestion,
// PostgreSQL and scrape-time collectors, including replay and invalid item behavior.
func TestOperationalMetricsEndToEndIntegration(t *testing.T) {
	pool, ctx, key := openServerIntegrationPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	service := nodes.NewService(pool, key)
	_, credential := enrollIngestionNode(t, ctx, service, "metrics-node")
	meter := telemetry.New("e2e")
	meter.RegisterDatabase(pool)
	handler := NewHandler(Options{NodeCredentials: service, AgentAPI: &agentapi.Postgres{Pool: pool}, AgentRateLimit: 10000, Metrics: metrics.Collector{DB: pool}, MetricsToken: "test-scrape-token", Telemetry: meter, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	server := httptest.NewServer(handler)
	defer server.Close()
	client := server.Client()
	call := func(path, body, token string) (int, string, http.Header) {
		t.Helper()
		method := "GET"
		if body != "" {
			method = "POST"
		}
		req, err := http.NewRequestWithContext(ctx, method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(data), resp.Header
	}
	if code, _, _ := call("/metrics", "", ""); code != 403 {
		t.Fatal("scrape auth bypass", code)
	}
	for _, body := range []string{`{"results":[` + ingestionResult + `,{}]}`, `{"results":[` + ingestionResult + `]}`} {
		if code, _, _ := call("/api/agent/results", body, credential); code != 207 {
			t.Fatal(code)
		}
	}
	if code, _, _ := call("/api/agent/heartbeat", `{"agent":"v1","uptime":10,"lastTest":null}`, credential); code != 200 {
		t.Fatal("heartbeat", code)
	}
	code, body, headers := call("/metrics", "", "test-scrape-token")
	if code != 200 || headers.Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatal(code, headers)
	}
	parser := expfmt.NewTextParser(model.LegacyValidation)
	f, err := parser.TextToMetricFamilies(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range f["speedtest_results_ingested_total"].Metric {
		if m.GetCounter().GetValue() != 1 {
			t.Fatal("expected accepted/duplicate/rejected exactly once", m)
		}
	}
	if f["speedtest_results_total"].Metric[0].GetGauge().GetValue() != 1 {
		t.Fatal("replay stored twice")
	}
	if age := f["speedtest_node_heartbeat_age_seconds"].Metric[0].GetGauge().GetValue(); age < 0 || age > 3 {
		t.Fatal("heartbeat age", age)
	}
	if f["speedtest_node_last_result_age_seconds"] == nil || f["http_request_duration_seconds"] == nil || f["go_goroutines"] == nil {
		t.Fatal("missing operational series")
	}
}
