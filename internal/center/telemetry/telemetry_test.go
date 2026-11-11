package telemetry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

func scrape(t *testing.T, meter *Telemetry) map[string]*dto.MetricFamily {
	t.Helper()
	var b bytes.Buffer
	if err := meter.Write(&b); err != nil {
		t.Fatal(err)
	}
	p := expfmt.NewTextParser(model.LegacyValidation)
	families, err := p.TextToMetricFamilies(&b)
	if err != nil {
		t.Fatal(err)
	}
	return families
}
func TestRegistryIsolationAndOutcomes(t *testing.T) {
	meter := New("test")
	other := New("other")
	for _, status := range []string{"accepted", "duplicate", "rejected", "scanner"} {
		meter.Ingest(status)
	}
	for _, job := range []string{"retention", "alerts", "notification", "geo_backfill"} {
		meter.Job(job, true)
		meter.Job(job, false)
	}
	meter.Job("scanner", true)
	f := scrape(t, meter)
	if len(f["speedtest_results_ingested_total"].Metric) != 3 {
		t.Fatal("unbounded outcomes")
	}
	for _, m := range f["speedtest_results_ingested_total"].Metric {
		if m.GetCounter().GetValue() != 1 {
			t.Fatal(m)
		}
	}
	if len(f["speedtest_job_runs_total"].Metric) != 8 {
		t.Fatal("unbounded jobs")
	}
	for _, m := range f["speedtest_job_runs_total"].Metric {
		if m.GetCounter().GetValue() != 1 {
			t.Fatal(m)
		}
	}
	for _, m := range f["speedtest_job_last_success_timestamp_seconds"].Metric {
		if m.GetGauge().GetValue() <= 0 {
			t.Fatal(m)
		}
	}
	for _, name := range []string{"go_goroutines", "process_cpu_seconds_total", "speedtest_build_info"} {
		if f[name] == nil {
			t.Fatal(name)
		}
	}
	for _, m := range scrape(t, other)["speedtest_results_ingested_total"].Metric {
		if m.GetCounter().GetValue() != 0 {
			t.Fatal("global registry leakage")
		}
	}
}

func TestGeoProviderUsesBoundedLabelsAndOutcomes(t *testing.T) {
	meter := New("test")
	meter.GeoProvider("ip-api", "success")
	meter.GeoProvider("ipwho", "unsupported")
	meter.GeoProvider("unknown-provider", "success")
	meter.GeoProvider("ripestat", "budget_exhausted")
	families := scrape(t, meter)
	metrics := families["speedtest_geo_lookups_total"].Metric
	recorded := 0
	seen := map[string]bool{}
	for _, metric := range metrics {
		if metric.GetCounter().GetValue() == 0 {
			continue
		}
		recorded++
		labels := metric.GetLabel()
		values := map[string]string{}
		for _, label := range labels {
			values[label.GetName()] = label.GetValue()
		}
		seen[values["provider"]+":"+values["outcome"]] = true
		if metric.GetCounter().GetValue() != 1 {
			t.Fatalf("geo counter = %v", metric)
		}
	}
	if recorded != 3 || !seen["ip-api:success"] || !seen["ipwho:error"] || !seen["ripestat:budget_exhausted"] {
		t.Fatalf("recorded geo outcomes = %v", seen)
	}
}

func TestMiddlewareStatusFlightAndStreaming(t *testing.T) {
	meter := New("test")
	cases := []struct {
		method, path string
		status       int
	}{{"GET", "/healthz", 204}, {"POST", "/api/nodes/42/run", 400}, {"SCANNER", "/random", 200}, {"HEAD", "/", 200}, {"OPTIONS", "/readyz", 0}}
	for _, tc := range cases {
		handler := meter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if scrape(t, meter)["http_requests_in_flight"].Metric[0].GetGauge().GetValue() != 1 {
				t.Fatal("not in flight")
			}
			if rw, ok := w.(interface{ Unwrap() http.ResponseWriter }); !ok || rw.Unwrap() == nil {
				t.Fatal("stream controller lost")
			}
			switch tc.status {
			case 0:
				return
			case 204:
				w.WriteHeader(103)
				w.WriteHeader(204)
				w.WriteHeader(500)
			case 400:
				w.WriteHeader(400)
			default:
				_, _ = w.Write([]byte("ok"))
			}
		}))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(tc.method, tc.path, nil))
	}
	f := scrape(t, meter)
	if f["http_requests_in_flight"].Metric[0].GetGauge().GetValue() != 0 {
		t.Fatal("flight leaked")
	}
	if len(f["http_requests_total"].Metric) != 5 {
		t.Fatal(f["http_requests_total"])
	}
	for _, m := range f["http_requests_total"].Metric {
		if m.GetCounter().GetValue() != 1 {
			t.Fatal(m)
		}
	}
	for _, m := range f["http_request_duration_seconds"].Metric {
		if m.GetHistogram().GetSampleCount() != 1 || m.GetHistogram().GetSampleSum() < 0 {
			t.Fatal(m)
		}
	}
}
func TestAllDocumentedRoutesAndAssets(t *testing.T) {
	data, err := os.ReadFile("../../../docs/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "  /api/") {
			continue
		}
		template := strings.TrimSuffix(strings.TrimSpace(line), ":")
		parts := strings.Split(template, "/")
		for i, p := range parts {
			if strings.HasPrefix(p, "{") {
				parts[i] = "{id}"
			}
		}
		want := strings.Join(parts, "/")
		for _, id := range []string{"123", "00000000-0000-4000-8000-000000000001"} {
			p := strings.ReplaceAll(want, "{id}", id)
			if got := Route(p); got != want {
				t.Fatalf("%s: %s != %s", p, got, want)
			}
		}
	}
	err = filepath.WalkDir("../../../ui", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel("../../../ui", path)
			if Route("/"+filepath.ToSlash(rel)) != "ui" {
				t.Fatal(path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/", "/index.html"} {
		if Route(p) != "ui" {
			t.Fatal(p)
		}
	}
	for _, p := range []string{"/healthz", "/readyz", "/metrics", "/mcp", "/api/access-tokens"} {
		if Route(p) != p {
			t.Fatal(p)
		}
	}
	for i := 0; i < 1000; i++ {
		for _, p := range []string{fmt.Sprintf("/scanner-%d", i), fmt.Sprintf("/api/scanner-%d/123", i), fmt.Sprintf("/api/nodes/%d/scanner-%d", i, i), "/api/nodes/not-an-id", "/assets/random.js", "/api/nodes/42/extra/42"} {
			if Route(p) != "other" {
				t.Fatal(p)
			}
		}
	}
}

type fakeDB struct {
	pool *pgxpool.Pool
	rows pgx.Rows
	err  error
}

func (d fakeDB) Stat() *pgxpool.Stat                                     { return d.pool.Stat() }
func (d fakeDB) Query(context.Context, string, ...any) (pgx.Rows, error) { return d.rows, d.err }

type fakeRows struct {
	pgx.Rows
	next                int
	scanErr, errorAfter error
	closed              bool
}

func (r *fakeRows) Next() bool { r.next++; return r.next == 1 }
func (r *fakeRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	*dest[0].(*string) = "node"
	*dest[1].(*float64) = 12
	*dest[2].(*float64) = -1
	return nil
}
func (r *fakeRows) Err() error { return r.errorAfter }
func (r *fakeRows) Close()     { r.closed = true }
func TestDatabaseCollectorAndFailures(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.MinConns = 0
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, mode := range []string{"ok", "query", "scan", "rows"} {
		t.Run(mode, func(t *testing.T) {
			rows := &fakeRows{}
			source := fakeDB{pool: pool, rows: rows}
			failure := errors.New("private database error")
			switch mode {
			case "query":
				source.err = failure
			case "scan":
				rows.scanErr = failure
			case "rows":
				rows.errorAfter = failure
			}
			meter := New("test")
			meter.RegisterDatabase(source)
			if mode != "ok" {
				var b bytes.Buffer
				if err := meter.Write(&b); err == nil || strings.Contains(err.Error(), "private") {
					t.Fatal(err)
				}
				if b.Len() != 0 {
					t.Fatal("partial scrape")
				}
				return
			}
			f := scrape(t, meter)
			for _, name := range []string{"acquired_connections", "idle_connections", "total_connections", "max_connections", "wait_count_total", "wait_duration_seconds_total"} {
				if f["speedtest_db_pool_"+name] == nil {
					t.Fatal(name)
				}
			}
			if f["speedtest_node_heartbeat_age_seconds"].Metric[0].GetGauge().GetValue() != 12 || f["speedtest_node_last_result_age_seconds"].Metric[0].GetGauge().GetValue() != -1 || !rows.closed {
				t.Fatal("node ages/rows lifecycle")
			}
		})
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func TestWriteFailure(t *testing.T) {
	if err := New("test").Write(brokenWriter{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
}

func TestInvalidExpositionFailsBeforeWriting(t *testing.T) {
	var b bytes.Buffer
	if err := writeFamilies(&b, []*dto.MetricFamily{{}}); err == nil || b.Len() != 0 {
		t.Fatal("invalid family was exposed", err, b.String())
	}
}
