package telemetry

import (
	"strings"

	"github.com/google/uuid"
)

// Route maps only documented API templates, exact UI assets and health endpoints.
// Unknown paths, including scanner input under /api, share one finite label.
func Route(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if _, err := uuid.Parse(part); err == nil {
			parts[i] = "{id}"
			continue
		}
		numeric := part != ""
		for _, c := range part {
			if c < '0' || c > '9' {
				numeric = false
				break
			}
		}
		if numeric {
			parts[i] = "{id}"
		}
	}
	normalized := strings.Join(parts, "/")
	switch normalized {
	case "/api/auth/me":
		return normalized
	case "/api/auth/login":
		return normalized
	case "/api/auth/logout":
		return normalized
	case "/api/auth/password":
		return normalized
	case "/api/nodes":
		return normalized
	case "/api/agent/tokens":
		return normalized
	case "/api/agent/enroll":
		return normalized
	case "/api/nodes/{id}":
		return normalized
	case "/api/nodes/{id}/run":
		return normalized
	case "/api/nodes/{id}/commands":
		return normalized
	case "/api/nodes/{id}/commands/{id}":
		return normalized
	case "/api/nodes/{id}/credential":
		return normalized
	case "/api/agent/results":
		return normalized
	case "/api/agent/config":
		return normalized
	case "/api/agent/heartbeat":
		return normalized
	case "/api/agent/commands":
		return normalized
	case "/api/agent/commands/{id}/ack":
		return normalized
	case "/api/agent/ip":
		return normalized
	case "/api/results":
		return normalized
	case "/api/results/{id}":
		return normalized
	case "/api/results/export.csv":
		return normalized
	case "/api/export/results.csv":
		return normalized
	case "/api/export/results.json":
		return normalized
	case "/api/stats/summary":
		return normalized
	case "/api/stats/trend":
		return normalized
	case "/api/stats/hour-of-day":
		return normalized
	case "/api/stats/heatmap":
		return normalized
	case "/api/stats/distribution":
		return normalized
	case "/api/dashboard":
		return normalized
	case "/api/series":
		return normalized
	case "/api/schedules":
		return normalized
	case "/api/schedules/{id}":
		return normalized
	case "/api/schedules/rules":
		return normalized
	case "/api/schedules/stagger":
		return normalized
	case "/api/alerts/rules":
		return normalized
	case "/api/alerts/rules/{id}":
		return normalized
	case "/api/audit":
		return normalized
	case "/api/alerts/events":
		return normalized
	case "/api/alerts/events/{id}":
		return normalized
	case "/api/alerts/events/{id}/ack":
		return normalized
	case "/api/alerts/acknowledgements":
		return normalized
	case "/api/notifications/test/{id}":
		return normalized
	case "/api/notifications/deliveries":
		return normalized
	case "/api/notifications/deliveries/{id}":
		return normalized
	case "/api/settings":
		return normalized
	case "/api/settings/enrolment-key":
		return normalized
	case "/api/ip/history":
		return normalized
	case "/api/ip/addresses":
		return normalized
	case "/api/ip/lookup":
		return normalized
	case "/api/ip/map":
		return normalized
	case "/api/jobs/{id}":
		return normalized
	case "/api/installer/probes":
		return normalized
	case "/api/nodes/{id}/installations":
		return normalized
	case "/api/installations/{id}":
		return normalized
	case "/api/installations/{id}/events":
		return normalized
	case "/api/nodes/{id}/uninstallations":
		return normalized
	case "/healthz", "/readyz", "/metrics":
		return normalized
	}
	switch path {
	case "/":
		return "ui"
	case "/index.html":
		return "ui"
	case "/assets/js/store.js":
		return "ui"
	case "/assets/js/ui.js":
		return "ui"
	case "/assets/js/charts.js":
		return "ui"
	case "/assets/js/app.js":
		return "ui"
	case "/assets/js/theme.js":
		return "ui"
	case "/assets/js/api.js":
		return "ui"
	case "/assets/js/seed.js":
		return "ui"
	case "/assets/css/app.css":
		return "ui"
	case "/assets/js/pages/login.js":
		return "ui"
	case "/assets/js/pages/alerts.js":
		return "ui"
	case "/assets/js/pages/nodes.js":
		return "ui"
	case "/assets/js/pages/network.js":
		return "ui"
	case "/assets/js/pages/history.js":
		return "ui"
	case "/assets/js/pages/schedules.js":
		return "ui"
	case "/assets/js/pages/dashboard.js":
		return "ui"
	case "/assets/js/pages/stats.js":
		return "ui"
	case "/assets/js/pages/settings.js":
		return "ui"
	case "/assets/vendor/leaflet/LICENSE":
		return "ui"
	case "/assets/vendor/leaflet/leaflet.js":
		return "ui"
	case "/assets/vendor/leaflet/leaflet.css":
		return "ui"
	case "/assets/vendor/leaflet/images/layers.png":
		return "ui"
	case "/assets/vendor/leaflet/images/marker-icon-2x.png":
		return "ui"
	case "/assets/vendor/leaflet/images/marker-shadow.png":
		return "ui"
	case "/assets/vendor/leaflet/images/marker-icon.png":
		return "ui"
	case "/assets/vendor/leaflet/images/layers-2x.png":
		return "ui"
	}
	return "other"
}
