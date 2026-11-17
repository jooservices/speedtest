package server

import (
	"net/http"
	"strings"

	"github.com/jooservices/speedtest/internal/center/apicontract"
)

const (
	// apiVersionPrefix is the stable API surface: every operation of docs/openapi.yaml is served under it.
	apiVersionPrefix = "/api/v1"
	// apiLegacyPrefix is the released surface. Its operations keep working as deprecated aliases.
	apiLegacyPrefix = "/api"

	// legacyAPIDeprecation is the RFC 9745 Deprecation value for the legacy /api paths: 2026-11-17 00:00:00 UTC.
	legacyAPIDeprecation = "@1794873600"
	// legacyAPISunset is the RFC 9110 HTTP-date after which the legacy /api paths stop being served.
	// PLACEHOLDER: the real Sunset date is chosen separately (JOOST-108 out of scope).
	legacyAPISunset = "Wed, 30 Jun 2027 00:00:00 GMT"
)

// slashRoute is an owner route whose dispatcher trims trailing slashes, so its slash form is served. The route is
// installed only when its store is configured (routes.go), and the slash form counts only then.
type slashRoute struct {
	installed func(Options) bool
	matches   func(string) bool
}

// slashRoutes lists the owner routes that accept a trailing slash. Owner routes that match exact paths get no alias
// headers on their slash forms. Other owner routes whose dispatchers also trim slashes are not listed yet.
var slashRoutes = []slashRoute{
	{installed: func(o Options) bool { return o.OwnerNodes != nil }, matches: ownerNodesRoute},
	{installed: func(o Options) bool { return o.OwnerAlerts != nil }, matches: ownerAlertsRoute},
	{installed: func(o Options) bool { return o.OwnerServers != nil }, matches: ownerServersRoute},
}

// withAPIVersion serves /api/v1 as the canonical path and marks the legacy /api aliases as deprecated. It must be the
// outermost wrapper: a documented /api/v1 request is rewritten to its /api path before auth, audit, telemetry and
// routing see it, so both prefixes reach the same handler with the same status and body. Any other /api/v1 request
// is passed on unchanged, so it gets exactly the response it got before the rewrite existed.
func withAPIVersion(next http.Handler, options Options) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isVersionPath(r.URL.Path) {
			if legacy := legacyPath(r.URL.Path); documentedOperation(options, r.Method, legacy) {
				rewritten := r.Clone(r.Context())
				rewritten.URL.Path = legacy
				rewritten.URL.RawPath = legacyPath(r.URL.RawPath)
				next.ServeHTTP(w, rewritten)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, apiLegacyPrefix+"/") && documentedOperation(options, r.Method, r.URL.Path) {
			w.Header().Set("Deprecation", legacyAPIDeprecation)
			w.Header().Set("Sunset", legacyAPISunset)
		}
		next.ServeHTTP(w, r)
	})
}

func isVersionPath(path string) bool {
	return path == apiVersionPrefix || strings.HasPrefix(path, apiVersionPrefix+"/")
}

// legacyPath maps a /api/v1 path to its /api path. Paths outside /api/v1 (including an empty raw path) are unchanged.
func legacyPath(path string) string {
	if !isVersionPath(path) {
		return path
	}
	return apiLegacyPrefix + strings.TrimPrefix(path, apiVersionPrefix)
}

// documentedOperation reports whether method and path name an operation of docs/openapi.yaml as the dispatchers
// match it. apicontract.Find ignores trailing slashes, but most dispatchers match exact paths, so a trailing slash is
// accepted only on a slash-tolerant owner route that is installed for these options, and an empty segment never is.
// HEAD is answered by the GET operation.
func documentedOperation(options Options, method, path string) bool {
	if strings.Contains(path, "//") || (strings.HasSuffix(path, "/") && !acceptsTrailingSlash(options, path)) {
		return false
	}
	if method == http.MethodHead {
		method = http.MethodGet
	}
	_, ok, err := apicontract.Find(method, path)
	return ok && err == nil
}

// acceptsTrailingSlash reports whether an installed slash-tolerant owner route serves path with its trailing slash.
func acceptsTrailingSlash(options Options, path string) bool {
	for _, route := range slashRoutes {
		if route.installed(options) && route.matches(path) {
			return true
		}
	}
	return false
}
