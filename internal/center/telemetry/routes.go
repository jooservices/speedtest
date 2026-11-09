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
	if apiRoutes[normalized] {
		return normalized
	}
	if uiRoutes[path] {
		return "ui"
	}
	return "other"
}

var apiRoutes = map[string]bool{
	"/api/auth/me":                       true,
	"/api/auth/login":                    true,
	"/api/auth/logout":                   true,
	"/api/auth/password":                 true,
	"/api/auth/2fa":                      true,
	"/api/auth/2fa/setup":                true,
	"/api/auth/2fa/enable":               true,
	"/api/auth/2fa/disable":              true,
	"/api/nodes":                         true,
	"/api/agent/tokens":                  true,
	"/api/agent/enroll":                  true,
	"/api/nodes/{id}":                    true,
	"/api/nodes/{id}/run":                true,
	"/api/nodes/{id}/commands":           true,
	"/api/nodes/{id}/commands/{id}":      true,
	"/api/nodes/{id}/credential":         true,
	"/api/agent/results":                 true,
	"/api/agent/config":                  true,
	"/api/agent/heartbeat":               true,
	"/api/agent/commands":                true,
	"/api/agent/commands/{id}/ack":       true,
	"/api/agent/ip":                      true,
	"/api/results":                       true,
	"/api/results/{id}":                  true,
	"/api/results/export.csv":            true,
	"/api/export/results.csv":            true,
	"/api/export/results.json":           true,
	"/api/stats/summary":                 true,
	"/api/stats/trend":                   true,
	"/api/stats/hour-of-day":             true,
	"/api/stats/heatmap":                 true,
	"/api/stats/distribution":            true,
	"/api/dashboard":                     true,
	"/api/series":                        true,
	"/api/schedules":                     true,
	"/api/schedules/{id}":                true,
	"/api/schedules/rules":               true,
	"/api/schedules/stagger":             true,
	"/api/alerts/rules":                  true,
	"/api/alerts/rules/{id}":             true,
	"/api/audit":                         true,
	"/api/alerts/events":                 true,
	"/api/alerts/events/{id}":            true,
	"/api/alerts/events/{id}/ack":        true,
	"/api/alerts/acknowledgements":       true,
	"/api/notifications/test/{id}":       true,
	"/api/notifications/deliveries":      true,
	"/api/notifications/deliveries/{id}": true,
	"/api/settings":                      true,
	"/api/settings/enrolment-key":        true,
	"/api/ip/history":                    true,
	"/api/ip/addresses":                  true,
	"/api/ip/lookup":                     true,
	"/api/ip/map":                        true,
	"/api/isps":                          true,
	"/api/isps/compare":                  true,
	"/api/isps/{id}":                     true,
	"/api/isps/{id}/trend":               true,
	"/api/isps/{id}/plans":               true,
	"/api/isps/{id}/plans/{id}":         true,
	"/api/jobs/{id}":                     true,
	"/api/installer/probes":              true,
	"/api/nodes/{id}/installations":      true,
	"/api/installations/{id}":            true,
	"/api/installations/{id}/events":     true,
	"/api/nodes/{id}/uninstallations":    true,
	"/healthz":                           true,
	"/readyz":                            true,
	"/metrics":                           true,
}

var uiRoutes = map[string]bool{
	"/":                                                true,
	"/index.html":                                      true,
	"/assets/js/store.js":                              true,
	"/assets/js/ui.js":                                 true,
	"/assets/js/charts.js":                             true,
	"/assets/js/app.js":                                true,
	"/assets/js/theme.js":                              true,
	"/assets/js/api.js":                                true,
	"/assets/js/seed.js":                               true,
	"/assets/css/app.css":                              true,
	"/assets/js/pages/login.js":                        true,
	"/assets/js/pages/alerts.js":                       true,
	"/assets/js/pages/nodes.js":                        true,
	"/assets/js/pages/network.js":                      true,
	"/assets/js/pages/history.js":                      true,
	"/assets/js/pages/schedules.js":                    true,
	"/assets/js/pages/dashboard.js":                    true,
	"/assets/js/pages/stats.js":                        true,
	"/assets/js/pages/settings.js":                     true,
	"/assets/vendor/leaflet/LICENSE":                   true,
	"/assets/vendor/leaflet/leaflet.js":                true,
	"/assets/vendor/leaflet/leaflet.css":               true,
	"/assets/vendor/leaflet/images/layers.png":         true,
	"/assets/vendor/leaflet/images/marker-icon-2x.png": true,
	"/assets/vendor/leaflet/images/marker-shadow.png":  true,
	"/assets/vendor/leaflet/images/marker-icon.png":    true,
	"/assets/vendor/leaflet/images/layers-2x.png":      true,
}
