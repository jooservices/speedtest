package server

import (
	"net/http"
	"strings"

	"github.com/jooservices/speedtest/internal/center/apicontract"
	"github.com/jooservices/speedtest/internal/center/apitoken"
)

// route is one area of the HTTP API: which requests it takes and the handler that serves them. Areas are tried in
// order, so a more specific route must be registered before a general one. Adding an endpoint means adding a route
// here, not changing a dispatcher. The name identifies the route in tests (see dispatchRecord); it changes no response.
type route struct {
	name       string
	matches    func(*http.Request) bool
	handler    http.Handler
	tokenScope func(*http.Request) (apitoken.Scope, bool)
}

// addToken registers an owner route whose bearer permission comes from its OpenAPI operation.
func (t *routeTable) addToken(name string, handler http.Handler, matches func(*http.Request) bool, scope func(*http.Request) (apitoken.Scope, bool)) {
	if handler != nil {
		*t = append(*t, route{name: name, matches: matches, handler: handler, tokenScope: scope})
	}
}

type routeTable []route

// add registers a route; an area whose store is not configured passes a nil handler and is left out.
func (t *routeTable) add(name string, handler http.Handler, matches func(*http.Request) bool) {
	if handler != nil {
		*t = append(*t, route{name: name, matches: matches, handler: handler})
	}
}

// onPath adapts a path predicate to a request predicate.
func onPath(match func(string) bool) func(*http.Request) bool {
	return func(r *http.Request) bool { return match(r.URL.Path) }
}

// routeFor returns the first route that takes the request.
func (t routeTable) routeFor(r *http.Request) (route, bool) {
	for _, rt := range t {
		if rt.matches(r) {
			return rt, true
		}
	}
	return route{}, false
}

func (t routeTable) tokenPermission(r *http.Request) (apitoken.Scope, bool) {
	for _, rt := range t {
		if rt.matches(r) && rt.tokenScope != nil {
			scope, ok := rt.tokenScope(r)
			if !ok {
				return apitoken.Full, true
			}
			return scope, true
		}
	}
	operation, ok, err := apicontract.Find(r.Method, r.URL.Path)
	return apitoken.Scope(operation.Scope), ok && err == nil && operation.Scope != ""
}

func operationScope(r *http.Request) (apitoken.Scope, bool) {
	operation, ok, err := apicontract.Find(r.Method, r.URL.Path)
	return apitoken.Scope(operation.Scope), ok && err == nil && operation.Scope != ""
}

func accessTokenRoute(path string) bool {
	return path == "/api/access-tokens" || strings.HasPrefix(path, "/api/access-tokens/")
}

// ownerAreas builds the session-protected owner APIs in the order they are matched. Each area exists only when its
// store is configured.
func ownerAreas(o Options, auth *serverAuth) routeTable {
	var t routeTable
	t.add("auth", authRoutes(auth), onPath(func(p string) bool { return p == "/api/auth" || strings.HasPrefix(p, "/api/auth/") }))
	if o.Audit != nil {
		t.addToken("audit", newOwnerAuditHandler(o.Audit, auth), onPath(auditRoute), operationScope)
	}
	if o.Installer != nil {
		t.addToken("installer", newInstallerHandler(o.Installer, auth, o.AgentRateLimit), onPath(installerRoute), operationScope)
	}
	if o.OwnerServers != nil {
		t.add("servers", newOwnerServersHandler(o.OwnerServers, auth), onPath(ownerServersRoute))
	}
	if o.OwnerNodes != nil {
		// Creating a node belongs to the enrolment handler.
		t.addToken("nodes", newOwnerNodesHandler(o.OwnerNodes, auth), func(r *http.Request) bool {
			return ownerNodesRoute(r.URL.Path) && (r.Method != http.MethodPost || r.URL.Path != "/api/nodes")
		}, operationScope)
	}
	if o.OwnerSchedules != nil {
		t.addToken("schedules", newOwnerSchedulesHandler(o.OwnerSchedules, auth), onPath(ownerSchedulesRoute), operationScope)
	}
	if o.OwnerUsage != nil {
		t.add("usage", newOwnerUsageHandler(o.OwnerUsage, auth), onPath(ownerUsageRoute))
	}
	if o.OwnerJobs != nil {
		t.addToken("jobs", newOwnerJobsHandler(o.OwnerJobs, auth), onPath(ownerJobsRoute), operationScope)
	}
	if o.OwnerDashboard != nil {
		t.addToken("dashboard", newOwnerDashboardHandler(o.OwnerDashboard, auth), onPath(ownerDashboardRoute), operationScope)
	}
	if o.OwnerNotifications != nil {
		t.addToken("notifications", newOwnerNotificationsHandler(o.OwnerNotifications, auth, o.AgentRateLimit), onPath(ownerNotificationsRoute), operationScope)
	}
	if o.OwnerIP != nil {
		t.addToken("ip", newOwnerIPHandler(o.OwnerIP, auth), onPath(ownerIPRoute), operationScope)
	}
	if o.Settings != nil {
		t.addToken("settings", newOwnerSettingsHandler(o.Settings, auth), onPath(ownerSettingsRoute), operationScope)
	}
	if o.OwnerResults != nil {
		t.addToken("results", newOwnerResultsHandler(o.OwnerResults, auth, o.NotificationEvents), onPath(ownerResultsRoute), operationScope)
	}
	if o.OwnerStats != nil {
		t.addToken("statistics", newOwnerStatsHandler(o.OwnerStats, auth), onPath(ownerStatsRoute), operationScope)
	}
	if o.OwnerReport != nil {
		t.add("reports", newOwnerReportHandler(o.OwnerReport, auth), onPath(ownerReportRoute))
	}
	if o.OwnerISP != nil {
		t.addToken("isps", newOwnerISPHandler(o.OwnerISP, auth), onPath(ownerISPRoute), operationScope)
	}
	if o.OwnerAlerts != nil {
		t.addToken("alerts", newOwnerAlertsHandler(o.OwnerAlerts, auth), onPath(ownerAlertsRoute), operationScope)
	}
	if o.AccessTokens != nil {
		t.add("access-tokens", newAccessTokenHandler(o.AccessTokens, auth), onPath(accessTokenRoute))
	}
	return t
}

// authRoutes is the sign-in, session and password API.
func authRoutes(auth *serverAuth) http.Handler { return http.HandlerFunc(auth.serve) }

// platformAreas builds the agent API, the installer script and the health, metrics and readiness endpoints.
func platformAreas(o Options, auth *serverAuth, readiness http.Handler) routeTable {
	var t routeTable
	t.add("agent", newAPIHandler(o.NodeCredentials, o.AgentRateLimit, auth, o.AgentAPI), onPath(func(p string) bool { return p == "/api" || strings.HasPrefix(p, "/api/") }))
	if o.Install != nil {
		t.add("install", o.Install, onPath(func(p string) bool { return p == "/install.sh" || strings.HasPrefix(p, "/download/") }))
	}
	t.add("healthz", getOnly(http.HandlerFunc(healthz)), onPath(func(p string) bool { return p == "/healthz" }))
	if o.Metrics != nil {
		t.add("metrics", getOnly(metricsHandler(o.Metrics, o.MetricsToken, o.Telemetry)), onPath(func(p string) bool { return p == "/metrics" }))
	}
	t.add("readyz", getOnly(readiness), onPath(func(p string) bool { return p == "/readyz" }))
	return t
}

// getOnly serves GET (and HEAD) and answers anything else with 405.
func getOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveGetRoute(w, r, next) })
}

// dispatchRecord tells a test which route served a request, and whether that route then missed it: its handler answered
// with writeRouteNotFound, the routing fallback, not a resource answer. A test attaches one to the request context; a
// request without one is dispatched exactly as before.
type dispatchRecord struct {
	route  string
	missed bool
}

// dispatchRecordKey is the context key of a dispatchRecord.
type dispatchRecordKey struct{}

// writeRouteNotFound answers a request that a route takes but none of its operations matches. It writes the response
// writeNotFoundProblem writes and notes the miss in the dispatch record, so the parity test can tell a broken operation
// from a missing resource, which writeNotFoundProblem also answers.
func writeRouteNotFound(w http.ResponseWriter, r *http.Request) {
	if record, ok := r.Context().Value(dispatchRecordKey{}).(*dispatchRecord); ok {
		record.missed = true
	}
	writeNotFoundProblem(w, r)
}

// dispatcher sends a request to the first matching area, or to the UI, and notes the route it used in any dispatchRecord
// the request carries.
func dispatcher(areas routeTable, ui http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rt, ok := areas.routeFor(r); ok {
			if record, ok := r.Context().Value(dispatchRecordKey{}).(*dispatchRecord); ok {
				record.route = rt.name
			}
			rt.handler.ServeHTTP(w, r)
			return
		}
		ui.ServeHTTP(w, r)
	})
}
