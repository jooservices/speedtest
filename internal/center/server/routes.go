package server

import (
	"net/http"
	"strings"

	"github.com/jooservices/speedtest/internal/center/apicontract"
	"github.com/jooservices/speedtest/internal/center/apitoken"
)

// route is one area of the HTTP API: which requests it takes and the handler that serves them. Areas are tried in
// order, so a more specific route must be registered before a general one. Adding an endpoint means adding a route
// here, not changing a dispatcher.
type route struct {
	matches    func(*http.Request) bool
	handler    http.Handler
	tokenScope func(*http.Request) (apitoken.Scope, bool)
}

// addToken registers an owner route whose bearer permission comes from its OpenAPI operation.
func (t *routeTable) addToken(handler http.Handler, matches func(*http.Request) bool, scope func(*http.Request) (apitoken.Scope, bool)) {
	if handler != nil {
		*t = append(*t, route{matches: matches, handler: handler, tokenScope: scope})
	}
}

type routeTable []route

// add registers a route; an area whose store is not configured passes a nil handler and is left out.
func (t *routeTable) add(handler http.Handler, matches func(*http.Request) bool) {
	if handler != nil {
		*t = append(*t, route{matches: matches, handler: handler})
	}
}

// onPath adapts a path predicate to a request predicate.
func onPath(match func(string) bool) func(*http.Request) bool {
	return func(r *http.Request) bool { return match(r.URL.Path) }
}

// handlerFor returns the handler of the first route that takes the request, or nil.
func (t routeTable) handlerFor(r *http.Request) http.Handler {
	for _, rt := range t {
		if rt.matches(r) {
			return rt.handler
		}
	}
	return nil
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
	t.add(authRoutes(auth), onPath(func(p string) bool { return p == "/api/auth" || strings.HasPrefix(p, "/api/auth/") }))
	if o.Audit != nil {
		t.addToken(newOwnerAuditHandler(o.Audit, auth), onPath(auditRoute), operationScope)
	}
	if o.Installer != nil {
		t.addToken(newInstallerHandler(o.Installer, auth, o.AgentRateLimit), onPath(installerRoute), operationScope)
	}
	if o.OwnerNodes != nil {
		// Creating a node belongs to the enrolment handler.
		t.addToken(newOwnerNodesHandler(o.OwnerNodes, auth), func(r *http.Request) bool {
			return ownerNodesRoute(r.URL.Path) && (r.Method != http.MethodPost || r.URL.Path != "/api/nodes")
		}, operationScope)
	}
	if o.OwnerSchedules != nil {
		t.addToken(newOwnerSchedulesHandler(o.OwnerSchedules, auth), onPath(ownerSchedulesRoute), operationScope)
	}
	if o.OwnerUsage != nil {
		t.add(newOwnerUsageHandler(o.OwnerUsage, auth), onPath(ownerUsageRoute))
	}
	if o.OwnerJobs != nil {
		t.addToken(newOwnerJobsHandler(o.OwnerJobs, auth), onPath(ownerJobsRoute), operationScope)
	}
	if o.OwnerDashboard != nil {
		t.addToken(newOwnerDashboardHandler(o.OwnerDashboard, auth), onPath(ownerDashboardRoute), operationScope)
	}
	if o.OwnerNotifications != nil {
		t.addToken(newOwnerNotificationsHandler(o.OwnerNotifications, auth, o.AgentRateLimit), onPath(ownerNotificationsRoute), operationScope)
	}
	if o.OwnerIP != nil {
		t.addToken(newOwnerIPHandler(o.OwnerIP, auth), onPath(ownerIPRoute), operationScope)
	}
	if o.Settings != nil {
		t.addToken(newOwnerSettingsHandler(o.Settings, auth), onPath(ownerSettingsRoute), operationScope)
	}
	if o.OwnerResults != nil {
		t.addToken(newOwnerResultsHandler(o.OwnerResults, auth, o.NotificationEvents), onPath(ownerResultsRoute), operationScope)
	}
	if o.OwnerStats != nil {
		t.addToken(newOwnerStatsHandler(o.OwnerStats, auth), onPath(ownerStatsRoute), operationScope)
	}
	if o.OwnerReport != nil {
		t.add(newOwnerReportHandler(o.OwnerReport, auth), onPath(ownerReportRoute))
	}
	if o.OwnerISP != nil {
		t.addToken(newOwnerISPHandler(o.OwnerISP, auth), onPath(ownerISPRoute), operationScope)
	}
	if o.OwnerAlerts != nil {
		t.addToken(newOwnerAlertsHandler(o.OwnerAlerts, auth), onPath(ownerAlertsRoute), operationScope)
	}
	if o.AccessTokens != nil {
		t.add(newAccessTokenHandler(o.AccessTokens, auth), onPath(accessTokenRoute))
	}
	return t
}

// authRoutes is the sign-in, session and password API.
func authRoutes(auth *serverAuth) http.Handler { return http.HandlerFunc(auth.serve) }

// platformAreas builds the agent API, the installer script and the health, metrics and readiness endpoints.
func platformAreas(o Options, auth *serverAuth, readiness http.Handler) routeTable {
	var t routeTable
	t.add(newAPIHandler(o.NodeCredentials, o.AgentRateLimit, auth, o.AgentAPI), onPath(func(p string) bool { return p == "/api" || strings.HasPrefix(p, "/api/") }))
	if o.Install != nil {
		t.add(o.Install, onPath(func(p string) bool { return p == "/install.sh" || strings.HasPrefix(p, "/download/") }))
	}
	t.add(getOnly(http.HandlerFunc(healthz)), onPath(func(p string) bool { return p == "/healthz" }))
	if o.Metrics != nil {
		t.add(getOnly(metricsHandler(o.Metrics, o.MetricsToken, o.Telemetry)), onPath(func(p string) bool { return p == "/metrics" }))
	}
	t.add(getOnly(readiness), onPath(func(p string) bool { return p == "/readyz" }))
	return t
}

// getOnly serves GET (and HEAD) and answers anything else with 405.
func getOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveGetRoute(w, r, next) })
}

// dispatcher sends a request to the first matching area, or to the UI.
func dispatcher(areas routeTable, ui http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := areas.handlerFor(r); h != nil {
			h.ServeHTTP(w, r)
			return
		}
		ui.ServeHTTP(w, r)
	})
}
