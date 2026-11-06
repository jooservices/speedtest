package server

import (
	"context"
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
	"time"

	"bytes"

	"github.com/jooservices/speedtest/internal/center/metrics"
	"github.com/jooservices/speedtest/internal/center/telemetry"
)

// MetricsSource produces one scrape of operator gauges.
type MetricsSource interface {
	Collect(context.Context) (metrics.Snapshot, error)
}

// metricsHandler serves GET /metrics. Access requires the bearer token when one
// is configured, otherwise the direct peer must be a loopback address. The
// gauges contain no secrets, but they are not public.
func metricsHandler(src MetricsSource, token string, operational ...*telemetry.Telemetry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !metricsAllowed(r, token) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		snap, err := src.Collect(ctx)
		if err != nil {
			http.Error(w, "metrics unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		// Buffer both sources before committing the response.
		var buffer bytes.Buffer
		if len(operational) > 0 && operational[0] != nil {
			if err := operational[0].Write(&buffer); err != nil {
				http.Error(w, "metrics unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		snap.Write(&buffer)
		_, _ = w.Write(buffer.Bytes())
	})
}

func metricsAllowed(r *http.Request, token string) bool {
	if token != "" {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		return ok && subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback() && r.Header.Get("X-Forwarded-For") == ""
}
