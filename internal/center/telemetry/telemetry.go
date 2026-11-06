// Package telemetry owns the center's isolated operational metrics registry.
package telemetry

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"github.com/jooservices/speedtest/internal/center/agentapi"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
)

// Telemetry is shared by the server and background workers. It never uses the global registry.
type Telemetry struct {
	Registry  *prometheus.Registry
	requests  *prometheus.CounterVec
	duration  *prometheus.HistogramVec
	flight    prometheus.Gauge
	ingestion *prometheus.CounterVec
	runs      *prometheus.CounterVec
	success   *prometheus.GaugeVec
}

// New creates an independent registry, including runtime, process and build collectors.
func New(version string) *Telemetry {
	t := &Telemetry{Registry: prometheus.NewRegistry(),
		requests:  prometheus.NewCounterVec(prometheus.CounterOpts{Name: "http_requests_total", Help: "Completed HTTP requests."}, []string{"method", "route", "status_class"}),
		duration:  prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "http_request_duration_seconds", Help: "HTTP request latency in seconds.", Buckets: prometheus.DefBuckets}, []string{"method", "route"}),
		flight:    prometheus.NewGauge(prometheus.GaugeOpts{Name: "http_requests_in_flight", Help: "Active HTTP requests."}),
		ingestion: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "speedtest_results_ingested_total", Help: "Result items by ingestion outcome."}, []string{"status"}),
		runs:      prometheus.NewCounterVec(prometheus.CounterOpts{Name: "speedtest_job_runs_total", Help: "Completed worker passes by outcome."}, []string{"job", "status"}),
		success:   prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "speedtest_job_last_success_timestamp_seconds", Help: "Unix time of the last successful worker pass; zero until success."}, []string{"job"}),
	}
	build := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "speedtest_build_info", Help: "Build and Go version."}, []string{"version", "go_version"})
	build.WithLabelValues(version, runtime.Version()).Set(1)
	t.Registry.MustRegister(t.requests, t.duration, t.flight, t.ingestion, t.runs, t.success, build, collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	for _, status := range []string{"accepted", "duplicate", "rejected"} {
		t.ingestion.WithLabelValues(status)
	}
	for _, job := range []string{"retention", "alerts", "notification"} {
		t.success.WithLabelValues(job).Set(0)
		for _, status := range []string{"success", "error"} {
			t.runs.WithLabelValues(job, status)
		}
	}
	return t
}

// Ingest records one validated batch item outcome, ignoring unsupported labels.
func (t *Telemetry) Ingest(status string) {
	switch status {
	case "accepted", "duplicate", "rejected":
		t.ingestion.WithLabelValues(status).Inc()
	}
}

// Job records a completed worker pass without affecting scheduling or retries.
func (t *Telemetry) Job(job string, ok bool) {
	switch job {
	case "retention", "alerts", "notification":
	default:
		return
	}
	status := "error"
	if ok {
		status = "success"
		t.success.WithLabelValues(job).Set(float64(time.Now().Unix()))
	}
	t.runs.WithLabelValues(job, status).Inc()
}

// Write emits the dedicated registry in the legacy Prometheus text format.
// Gathering completes before writing so collector errors cannot produce partial success.
func (t *Telemetry) Write(w io.Writer) error {
	families, err := t.Registry.Gather()
	if err != nil {
		return err
	}
	return writeFamilies(w, families)
}

func writeFamilies(w io.Writer, families []*dto.MetricFamily) error {
	var b bytes.Buffer
	for _, family := range families {
		if _, err := expfmt.MetricFamilyToText(&b, family); err != nil {
			return err
		}
	}
	_, err := w.Write(b.Bytes())
	return err
}

type response struct {
	http.ResponseWriter
	status int
}

func (w *response) WriteHeader(code int) {
	if code >= 100 && code < 200 && code != 101 {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	if w.status == 0 {
		w.status = code
		w.ResponseWriter.WriteHeader(code)
	}
}
func (w *response) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(b)
}
func (w *response) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Middleware wraps the complete handler so recovered failures and auth rejections are counted.
func (t *Telemetry) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.Method
		switch method {
		case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE":
		default:
			method = "other"
		}
		route := Route(r.URL.Path)
		start := time.Now()
		t.flight.Inc()
		rw := &response{ResponseWriter: w}
		defer func() {
			t.flight.Dec()
			status := rw.status
			if status == 0 {
				status = 200
			}
			t.requests.WithLabelValues(method, route, strconv.Itoa(status/100)+"xx").Inc()
			t.duration.WithLabelValues(method, route).Observe(time.Since(start).Seconds())
		}()
		next.ServeHTTP(rw, r.WithContext(agentapi.WithIngestObserver(r.Context(), t.Ingest)))
	})
}

// collectorError reports collection failures without exposing database errors.
func collectorError(ch chan<- prometheus.Metric, desc *prometheus.Desc) {
	ch <- prometheus.NewInvalidMetric(desc, fmt.Errorf("operational metrics unavailable"))
}
