package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jooservices/speedtest/internal/center/metrics"
)

type fakeMetrics struct{}

func (fakeMetrics) Collect(context.Context) (metrics.Snapshot, error) {
	return metrics.Snapshot{NodesEnabled: 3, NodesOffline: 1, LastResultAgeSeconds: 12}, nil
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
