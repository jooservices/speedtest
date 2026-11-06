package server

import "testing"

func TestRouteLabelNeverInventsGroups(t *testing.T) {
	const id = "10000000-0000-4000-8000-000000000001"
	cases := map[string]string{
		"/api/nodes/" + id + "/run":      "/api/nodes/{id}/run",
		"/api/alerts/events/" + id:       "/api/alerts/events/{id}",
		"/api/schedules/rules":           "/api/schedules/rules",
		"/api/stats/summary":             "/api/stats/summary",
		"/api/export/results.csv":        "/api/export/results.csv",
		"/api/wp-admin/setup.php":        "/api/*/*",
		"/api/nodes/abc/def/ghi/jkl/mno": "/api/nodes/*/*/*",
		"/healthz":                       "/healthz",
		"/wp-login.php":                  "ui",
		"/assets/js/app.js":              "ui",
		"/":                              "ui",
	}
	for path, want := range cases {
		if got := routeLabel(path); got != want {
			t.Errorf("routeLabel(%q) = %q, want %q", path, got, want)
		}
	}
}
