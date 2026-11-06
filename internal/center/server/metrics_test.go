package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jooservices/speedtest/internal/center/telemetry"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/jooservices/speedtest/internal/center/metrics"
)

type fakeMetrics struct{}

func (fakeMetrics) Collect(context.Context) (metrics.Snapshot, error) {
	return metrics.Snapshot{NodesEnabled: 3, NodesOffline: 1, LastResultAgeSeconds: 12, ResultsTotal: 42, OutboxPending: 2, DeliveriesPending: 4, DeliveriesFailed: 5, OldestPendingDeliveryAgeSeconds: 60, OpenAlerts: 6}, nil
}

func TestMetricsAccess(t *testing.T) {
	cases := []struct {
		name, token, remote, auth, xff string
		want                           int
	}{
		{"loopback no token", "", "127.0.0.1:5000", "", "", 200},
		{"remote no token", "", "10.0.0.5:5000", "", "", 403},
		{"proxied loopback", "", "127.0.0.1:5000", "", "203.0.113.1", 403},
		{"token ok", "s3cret-token", "10.0.0.5:5000", "Bearer s3cret-token", "", 200},
		{"token wrong", "s3cret-token", "127.0.0.1:5000", "Bearer nope", "", 403},
		{"token missing", "s3cret-token", "127.0.0.1:5000", "", "", 403},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			req.RemoteAddr = c.remote
			if c.auth != "" {
				req.Header.Set("Authorization", c.auth)
			}
			if c.xff != "" {
				req.Header.Set("X-Forwarded-For", c.xff)
			}
			rec := httptest.NewRecorder()
			metricsHandler(fakeMetrics{}, c.token).ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Fatalf("status %d want %d", rec.Code, c.want)
			}
			if c.want == 200 && !strings.Contains(rec.Body.String(), "speedtest_nodes_offline 1") {
				t.Fatalf("body: %s", rec.Body)
			}
		})
	}
}

type failingMetrics struct{ panic bool }

func (f failingMetrics) Collect(context.Context) (metrics.Snapshot, error) {
	if f.panic {
		panic("failure")
	}
	return metrics.Snapshot{}, errors.New("private error")
}
func TestOperationalMetricsCombinedAndOutermost(t *testing.T) {
	meter := telemetry.New("test")
	handler := NewHandler(Options{Metrics: fakeMetrics{}, Telemetry: meter})
	for _, path := range []string{"/healthz", "/readyz", "/unknown"} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
	}
	r := httptest.NewRequest("GET", "/metrics", nil)
	r.RemoteAddr = "127.0.0.1:9000"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatal(w.Code, w.Header())
	}
	parser := expfmt.NewTextParser(model.LegacyValidation)
	f, err := parser.TextToMetricFamilies(strings.NewReader(w.Body.String()))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]float64{
		"speedtest_nodes_enabled": 3, "speedtest_nodes_offline": 1, "speedtest_last_result_age_seconds": 12,
		"speedtest_results_total": 42, "speedtest_notification_outbox_pending": 2,
		"speedtest_notification_deliveries_pending": 4, "speedtest_notification_deliveries_failed": 5,
		"speedtest_notification_oldest_pending_age_seconds": 60, "speedtest_alerts_open": 6,
	} {
		if f[name] == nil || f[name].Metric[0].GetGauge().GetValue() != want {
			t.Fatalf("legacy gauge changed: %s", name)
		}
	}

	if len(f["http_requests_total"].Metric) != 3 || f["speedtest_build_info"] == nil {
		t.Fatal("missing combined telemetry")
	}
	for _, panicSource := range []bool{false, true} {
		h := NewHandler(Options{Metrics: failingMetrics{panic: panicSource}, Telemetry: meter})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := 503
		if panicSource {
			want = 500
		}
		if w.Code != want || strings.Contains(w.Body.String(), "private") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	families, err := meter.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]bool{}
	for _, family := range families {
		if family.GetName() == "http_requests_total" {
			for _, m := range family.Metric {
				for _, l := range m.Label {
					if l.GetName() == "status_class" {
						statuses[l.GetValue()] = true
					}
				}
			}
		}
	}
	if !statuses["5xx"] {
		t.Fatal("recovered panic not observed")
	}
	for _, remote := range []string{"invalid", "[::1]:9000"} {
		r := httptest.NewRequest("GET", "/metrics", nil)
		r.RemoteAddr = remote
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		want := 403
		if remote != "invalid" {
			want = 200
		}
		if w.Code != want {
			t.Fatal(remote, w.Code)
		}
	}
}

type failingOperationalCollector struct{}

func (failingOperationalCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- prometheus.NewDesc("unavailable_test", "Unavailable", nil, nil)
}
func (failingOperationalCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.NewInvalidMetric(prometheus.NewDesc("unavailable_test", "Unavailable", nil, nil), errors.New("private collector error"))
}
func TestMetricsOperationalCollectionFailureIsAtomic(t *testing.T) {
	meter := telemetry.New("test")
	meter.Registry.MustRegister(failingOperationalCollector{})
	r := httptest.NewRequest("GET", "/metrics", nil)
	r.RemoteAddr = "127.0.0.1:9000"
	w := httptest.NewRecorder()
	metricsHandler(fakeMetrics{}, "", meter).ServeHTTP(w, r)
	if w.Code != 503 || strings.Contains(w.Body.String(), "private") || strings.Contains(w.Body.String(), "speedtest_") {
		t.Fatal(w.Code, w.Body.String())
	}
}
