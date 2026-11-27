package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jooservices/speedtest/docs"
	"github.com/jooservices/speedtest/internal/center/alerts"
	"github.com/jooservices/speedtest/internal/center/apicontract"
	"github.com/jooservices/speedtest/internal/center/apitoken"
	"github.com/jooservices/speedtest/internal/center/installer"
	"github.com/jooservices/speedtest/internal/center/jobs"
	"gopkg.in/yaml.v3"
)

// specMethods are the HTTP method keys of a path item in docs/openapi.yaml.
var specMethods = map[string]bool{"get": true, "post": true, "put": true, "patch": true, "delete": true, "head": true, "options": true}

// specCanonicalPaths reads docs/openapi.yaml and returns, keyed by "METHOD /api/...", the x-canonical-path of every
// /api operation. It fails the test for an /api operation that is not deprecated or whose x-canonical-path is not the
// /api/v1 form of its path.
func specCanonicalPaths(t *testing.T) map[string]string {
	t.Helper()
	var spec struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(docs.OpenAPI, &spec); err != nil {
		t.Fatalf("parse docs/openapi.yaml: %v", err)
	}
	canonical := make(map[string]string)
	for path, item := range spec.Paths {
		if !strings.HasPrefix(path, apiLegacyPrefix+"/") {
			continue
		}
		for method, raw := range item {
			if !specMethods[method] {
				continue
			}
			key := strings.ToUpper(method) + " " + path
			op, _ := raw.(map[string]any)
			if deprecated, _ := op["deprecated"].(bool); !deprecated {
				t.Errorf("%s: deprecated is not true", key)
			}
			want := apiVersionPrefix + strings.TrimPrefix(path, apiLegacyPrefix)
			got, _ := op["x-canonical-path"].(string)
			if got != want {
				t.Errorf("%s: x-canonical-path = %q, want %q", key, got, want)
			}
			canonical[key] = got
		}
	}
	return canonical
}

const parityRequestID = "parity-req-0001"

// concretePath fills every template parameter of an OpenAPI path with the fixture node id.
func concretePath(template string) string {
	segments := strings.Split(template, "/")
	for i, segment := range segments {
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
			segments[i] = nodeID
		}
	}
	return strings.Join(segments, "/")
}

// specCredentials returns, keyed by "METHOD /api/...", the security scheme each /api operation of docs/openapi.yaml
// requires. An operation without its own security list uses the document's; an empty alternative ("{}") or an empty
// list is anonymous, which is returned as "".
func specCredentials(t *testing.T) map[string]string {
	t.Helper()
	var spec struct {
		Security any                       `yaml:"security"`
		Paths    map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(docs.OpenAPI, &spec); err != nil {
		t.Fatalf("parse docs/openapi.yaml security: %v", err)
	}
	credentials := make(map[string]string)
	for path, item := range spec.Paths {
		if !strings.HasPrefix(path, apiLegacyPrefix+"/") {
			continue
		}
		for method, raw := range item {
			if !specMethods[method] {
				continue
			}
			operation, _ := raw.(map[string]any)
			security, own := operation["security"]
			if !own {
				security = spec.Security
			}
			credentials[strings.ToUpper(method)+" "+path] = firstScheme(security)
		}
	}
	return credentials
}

// firstScheme returns the first named security scheme of a security list, or "" for an anonymous list.
func firstScheme(security any) string {
	alternatives, _ := security.([]any)
	for _, alternative := range alternatives {
		named, _ := alternative.(map[string]any)
		for name := range named {
			return name
		}
	}
	return ""
}

// requiredCredential prepares a request with the credential its scheme names: a session cookie with the CSRF token and
// origin for writes, a bearer token for the owner, node and enrolment credentials, and nothing for an anonymous one.
func requiredCredential(scheme, method string) func(*http.Request) {
	bearer := func(token string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
	}
	switch scheme {
	case "SessionCookie":
		if method == http.MethodGet || method == http.MethodHead {
			return ownerRead
		}
		return ownerWrite
	case "OwnerBearer":
		return bearer("valid-token")
	case "NodeBearer":
		return bearer("good")
	case "EnrollmentKey":
		return bearer("settings-key")
	}
	return nil
}

// configuredOptions returns the options of a command center with every /api area configured by a minimal store or
// fake, so a request reaches its area's handler rather than the router's unmatched-path or "not configured" answer.
// The owner token store accepts "valid-token" with full scope; the node credential fake accepts "good" as an agent
// credential and "settings-key" as the enrolment key.
func configuredOptions() Options {
	return Options{
		Auth:               nodeTestAuth{},
		NodeCredentials:    &fakeNodeCredentialService{},
		AgentAPI:           stubAgentStore{},
		AccessTokens:       &tokenAuthStore{principal: apitoken.Principal{TokenID: "token-1", UserID: "owner-id", Email: "owner@example.test", Scope: apitoken.Full}},
		Audit:              &fakeAudit{},
		OwnerNodes:         &fakeOwnerNodes{},
		OwnerServers:       &fakeOwnerServers{},
		OwnerSchedules:     &fakeOwnerSchedules{},
		OwnerJobs:          fakeOwnerJobs{err: jobs.ErrNotFound},
		OwnerDashboard:     &fakeOwnerDashboard{},
		OwnerIP:            &fakeOwnerIP{},
		OwnerNotifications: &notificationStub{},
		Settings:           settingsWithLanguage("en"),
		OwnerResults:       &fakeOwnerResults{},
		OwnerStats:         &fakeOwnerStats{},
		OwnerUsage:         &fakeOwnerUsage{},
		OwnerISP:           &fakeOwnerISP{},
		OwnerReport:        &fakeReports{},
		OwnerAlerts:        &fakeOwnerAlerts{},
		Installer:          &fakeInstallerService{serviceErr: installer.ErrNotFound},
	}
}

// serveSameRequest sends one request with a fixed request id and the prepared credential, so both prefixes can be
// compared byte for byte.
func serveSameRequest(handler http.Handler, method, target string, prepare func(*http.Request)) *httptest.ResponseRecorder {
	return serveSameRequestWithBody(handler, method, target, "", prepare)
}

// serveSameRequestWithBody is serveSameRequest with a request body.
func serveSameRequestWithBody(handler http.Handler, method, target, body string, prepare func(*http.Request)) *httptest.ResponseRecorder {
	return do(handler, method, target, body, func(r *http.Request) {
		r.Header.Set(requestIDHeader, parityRequestID)
		if prepare != nil {
			prepare(r)
		}
	})
}

var rfc3339Timestamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)

// maskTimestamps hides the clock values a handler writes into a body (session expiry, for example), which differ
// between two requests made a moment apart.
func maskTimestamps(body string) string {
	return rfc3339Timestamp.ReplaceAllString(body, "<timestamp>")
}

// sameExceptAliasHeaders reports whether two responses carry the same headers once Deprecation and Sunset are ignored.
// It works on copies, so the caller can still check the alias headers on the originals.
func sameExceptAliasHeaders(a, b http.Header) bool {
	stripped := func(h http.Header) http.Header {
		c := h.Clone()
		c.Del("Deprecation")
		c.Del("Sunset")
		return c
	}
	return reflect.DeepEqual(stripped(a), stripped(b))
}

func TestSameExceptAliasHeadersKeepsHeaderValueBoundaries_AC2(t *testing.T) {
	combined := http.Header{"X-Example": {"a b"}, "Deprecation": {"first"}}
	split := http.Header{"X-Example": {"a", "b"}, "Sunset": {"second"}}
	if sameExceptAliasHeaders(combined, split) {
		t.Fatal("different header value lists compared equal")
	}
	matching := http.Header{"X-Example": {"a b"}, "Sunset": {"different"}}
	if !sameExceptAliasHeaders(combined, matching) {
		t.Fatal("Deprecation and Sunset should be ignored")
	}
}

func TestVersionedResponseRejectsEmptyAliasHeaders_AC3(t *testing.T) {
	for _, name := range []string{"Deprecation", "Sunset"} {
		t.Run(name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/problem+json")
				if strings.HasPrefix(r.URL.Path, apiVersionPrefix) {
					w.Header().Add(name, "")
				} else {
					w.Header().Set("Deprecation", legacyAPIDeprecation)
					w.Header().Set("Sunset", legacyAPISunset)
				}
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(`{"type":"/problems/validation"}`))
			})
			case_ := errorParityCase{area: "test", method: http.MethodGet, path: "/api/test", wantStatus: http.StatusUnprocessableEntity}
			if err := checkErrorOperationParity(handler, case_); err == nil || !strings.Contains(err.Error(), "carries alias headers") {
				t.Fatalf("parity check did not reject the empty versioned %s header: %v", name, err)
			}
		})
	}
}

func versionedResponseHasAliasHeaders(headers http.Header) bool {
	for name := range headers {
		if strings.EqualFold(name, "Deprecation") || strings.EqualFold(name, "Sunset") {
			return true
		}
	}
	return false
}

// TestEveryOperationServedUnderAPIv1 calls every docs/openapi.yaml /api operation on /api/v1 and on its /api alias with
// the credential the operation requires, against one command center with every area configured, and requires the same
// status, body and headers. It fails when a response is the router's unmatched-path answer, a "not configured" answer,
// or a 401 or 403 despite the credential. It also compares one validation or missing-resource error per OpenAPI
// resource area. Only the alias may carry Deprecation and Sunset.
func TestEveryOperationServedUnderAPIv1(t *testing.T) {
	operations, err := apicontract.Operations()
	if err != nil {
		t.Fatalf("load operations: %v", err)
	}
	if len(operations) == 0 {
		t.Fatal("no operations in docs/openapi.yaml")
	}
	handler := newNodeHandler(configuredOptions())
	canonical := specCanonicalPaths(t)
	credentials := specCredentials(t)
	tags, _ := specOperationAreas(t)
	for _, operation := range operations {
		if !strings.HasPrefix(operation.Path, apiLegacyPrefix+"/") {
			// Only /api operations have a /api/v1 form; /install.sh and the download paths do not.
			continue
		}
		key := operation.Method + " " + operation.Path
		versioned := canonical[key]
		if versioned == "" {
			t.Errorf("%s has no x-canonical-path in docs/openapi.yaml", key)
			continue
		}
		scheme := credentials[key]
		body, hasBody := operationBodies[key]
		if hasBody {
			scheme = bodyTokenScheme
		}
		expected := expectedRoute(key, tags[key])
		t.Run(key, func(t *testing.T) {
			checkOperationParity(t, handler, operation, versioned, scheme, body, expected)
		})
	}
	checkErrorParityPerArea(t)
}

// tagRoutes names the route that serves each OpenAPI resource area (its tag), as routes.go registers the areas.
var tagRoutes = map[string]string{
	"Access tokens": "access-tokens", "Agent": "agent", "Alerts": "alerts", "Auth": "auth", "Bandwidth": "usage",
	"Dashboard": "dashboard", "IP": "ip", "ISPs": "isps", "Installer": "installer", "Jobs": "jobs", "Nodes": "nodes",
	"Notifications": "notifications", "Reports": "reports", "Results": "results", "Schedules": "schedules",
	"Settings": "settings", "Statistics": "statistics",
}

// operationRoutes are the operations served by a route other than their tag's: node creation and credential rotation by
// the agent API's handler (see routes.go), the node server catalogue by the servers route, and the audit log by the audit route.
var operationRoutes = map[string]string{
	"POST /api/nodes":                      "agent",
	"POST /api/nodes/{id}/credential":      "agent",
	"DELETE /api/nodes/{id}/credential":    "agent",
	"GET /api/nodes/{id}/servers":          "servers",
	"POST /api/nodes/{id}/servers":         "servers",
	"PUT /api/nodes/{id}/server-selection": "servers",
	"GET /api/audit":                       "audit", // tagged Settings in docs/openapi.yaml, served by the audit route
}

// expectedRoute returns the route an operation must be dispatched to: its operationRoutes entry, or else the route of its
// OpenAPI tag. An empty result means the tag has no route, which the check reports.
func expectedRoute(key, tag string) string {
	if route, ok := operationRoutes[key]; ok {
		return route
	}
	return tagRoutes[tag]
}

// bodyTokenScheme labels a credential that travels in the request body rather than in a header.
const bodyTokenScheme = "token in request body"

// operationBodies are the request bodies the parity check sends. The enrolment operation carries its single-use token in
// the body (docs/openapi.yaml, EnrollRequest), so it is sent with the fake's valid token.
var operationBodies = map[string]string{
	"POST /api/agent/enroll": `{"token":"one-time","name":"edge","host":"edge.test","agent":"v1"}`,
}

// withDispatchRecord prepares a request with prepare's credential and attaches record to its context, so the dispatcher
// notes the route that serves it.
func withDispatchRecord(prepare func(*http.Request), record *dispatchRecord) func(*http.Request) {
	return func(r *http.Request) {
		if prepare != nil {
			prepare(r)
		}
		*r = *r.WithContext(context.WithValue(r.Context(), dispatchRecordKey{}, record))
	}
}

// checkOperationParity compares one operation on its x-canonical-path (versioned) with its /api alias, both sent with
// the credential its scheme names and the operation's body: each prefix must be dispatched to the expected route, and the
// status, body and headers must match, with the alias headers checked on the original responses before the other headers
// are compared.
func checkOperationParity(t testing.TB, handler http.Handler, operation apicontract.Operation, versioned, scheme, body, expected string) {
	t.Helper()
	credential := requiredCredential(scheme, operation.Method)
	legacy := concretePath(operation.Path)
	versioned = concretePath(versioned)
	var onV1Dispatch, onAliasDispatch dispatchRecord
	onV1 := serveSameRequestWithBody(handler, operation.Method, versioned, body, withDispatchRecord(credential, &onV1Dispatch))
	onAlias := serveSameRequestWithBody(handler, operation.Method, legacy, body, withDispatchRecord(credential, &onAliasDispatch))
	t.Logf("%s %s -> route %s, status %d", operation.Method, legacy, onAliasDispatch.route, onAlias.Code)
	if expected == "" {
		t.Fatalf("%s %s has no route for its OpenAPI area", operation.Method, legacy)
	}
	namesResource := strings.Contains(operation.Path, "{")
	checkDispatch(t, operation, versioned, expected, scheme, namesResource, onV1, onV1Dispatch)
	checkDispatch(t, operation, legacy, expected, scheme, namesResource, onAlias, onAliasDispatch)
	checkSameResponse(t, onV1, onAlias, versioned, legacy)
}

// checkDispatch checks one prefix's request: it reached the expected route, the route's own operation matched it, and the
// response is one an area handler produced.
func checkDispatch(t testing.TB, operation apicontract.Operation, target, expected, scheme string, namesResource bool, rec *httptest.ResponseRecorder, dispatch dispatchRecord) {
	t.Helper()
	if dispatch.route != expected {
		t.Errorf("%s %s dispatched to route %q, want %q", operation.Method, target, dispatch.route, expected)
	}
	if dispatch.missed {
		t.Errorf("%s %s reached route %q, but no operation of it matched: the routing fallback answered", operation.Method, target, dispatch.route)
	}
	if problem := servedProblem(rec, scheme, namesResource); problem != "" {
		t.Errorf("%s %s reached no handler: %s (status %d, body %q)", operation.Method, target, problem, rec.Code, rec.Body.String())
	}
}

// checkSameResponse checks that the two prefixes answer with the same status, body and headers.
func checkSameResponse(t testing.TB, onV1, onAlias *httptest.ResponseRecorder, versioned, legacy string) {
	t.Helper()
	if onV1.Code != onAlias.Code {
		t.Fatalf("status on %s = %d, on alias %s = %d", versioned, onV1.Code, legacy, onAlias.Code)
	}
	if maskTimestamps(onV1.Body.String()) != maskTimestamps(onAlias.Body.String()) {
		t.Errorf("body on %s = %q, on alias %s = %q", versioned, onV1.Body.String(), legacy, onAlias.Body.String())
	}
	checkAliasHeaders(t, onV1, onAlias, versioned, legacy)
}

// checkAliasHeaders checks that only the alias carries Deprecation and Sunset, and that the other headers match.
func checkAliasHeaders(t testing.TB, onV1, onAlias *httptest.ResponseRecorder, versioned, legacy string) {
	t.Helper()
	if got := onAlias.Header().Get("Deprecation"); got != legacyAPIDeprecation {
		t.Errorf("alias %s Deprecation = %q, want %q", legacy, got, legacyAPIDeprecation)
	}
	if got := onAlias.Header().Get("Sunset"); got != legacyAPISunset {
		t.Errorf("alias %s Sunset = %q, want %q", legacy, got, legacyAPISunset)
	}
	if onV1.Header().Get("Deprecation") != "" || onV1.Header().Get("Sunset") != "" {
		t.Errorf("%s carries alias headers: %v", versioned, onV1.Header())
	}
	if !sameExceptAliasHeaders(onV1.Header(), onAlias.Header()) {
		t.Errorf("headers on %s = %v, on alias %s = %v", versioned, onV1.Header(), legacy, onAlias.Header())
	}
}

// servedProblem names why a response shows that no route served the request: a 404 for an operation that names no resource
// (no path parameter), the router's unmatched-path answer, a "not configured" answer, or a 401 or 403 although a credential
// was sent. It returns "" for a response an area handler produced. A credential-less operation may legitimately answer 401
// or 403. A parameterised operation may legitimately answer 404 for a missing resource, so its 404 passes here.
func servedProblem(rec *httptest.ResponseRecorder, scheme string, namesResource bool) string {
	body := rec.Body.String()
	switch {
	case rec.Code == http.StatusNotFound && !namesResource:
		return "a 404 although the operation names no resource, so it is the router's not-found answer"
	case rec.Code == http.StatusNotFound && body == "404 page not found\n":
		return "the router's unmatched-path response"
	case strings.Contains(body, "not configured"):
		return `a "not configured" answer`
	case scheme != "" && (rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden):
		return fmt.Sprintf("status %d although its credential (%s) was sent", rec.Code, scheme)
	}
	return ""
}

// recordingTB records the failures of a check instead of failing the test, so a test can assert that the check rejects a
// response. Fatalf records too and returns, which is enough for the checks that use it.
type recordingTB struct {
	testing.TB
	failures []string
}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

func (r *recordingTB) Fatalf(format string, args ...any) { r.Errorf(format, args...) }

// TestParityRejectsRoutingFallbackButAcceptsMissingResource checks the parameterised case. A route that takes
// POST /api/nodes/{id}/run but whose operation does not match must fail on both prefixes, while the same route answering
// a missing node with the resource not-found answer must pass.
func TestParityRejectsRoutingFallbackButAcceptsMissingResource(t *testing.T) {
	operation := apicontract.Operation{Method: http.MethodPost, Path: "/api/nodes/{id}/run"}
	build := func(answer http.HandlerFunc) http.Handler {
		table := routeTable{{name: "nodes", handler: answer, matches: onPath(func(p string) bool { return strings.HasSuffix(p, "/run") })}}
		return withAPIVersion(dispatcher(table, http.NotFoundHandler()), Options{})
	}
	t.Run("broken operation", func(t *testing.T) {
		rec := &recordingTB{TB: t}
		checkOperationParity(rec, build(writeRouteNotFound), operation, "/api/v1/nodes/{id}/run", "SessionCookie", "", "nodes")
		if len(rec.failures) != 2 {
			t.Fatalf("failures = %d, want one per prefix: %q", len(rec.failures), rec.failures)
		}
		for _, failure := range rec.failures {
			if !strings.Contains(failure, "the routing fallback answered") {
				t.Errorf("failure does not name the routing fallback: %q", failure)
			}
		}
	})
	t.Run("missing resource", func(t *testing.T) {
		rec := &recordingTB{TB: t}
		checkOperationParity(rec, build(func(w http.ResponseWriter, r *http.Request) { writeNotFoundProblem(w, r) }), operation, "/api/v1/nodes/{id}/run", "SessionCookie", "", "nodes")
		if len(rec.failures) != 0 {
			t.Errorf("a missing resource was rejected: %q", rec.failures)
		}
	})
}

// TestParityRejectsMissingEnrollmentCredential checks the body-token case: an enrolment answered 401 or 403 although its
// one-time token was sent in the body fails on both prefixes.
func TestParityRejectsMissingEnrollmentCredential(t *testing.T) {
	operation := apicontract.Operation{Method: http.MethodPost, Path: "/api/agent/enroll"}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			enroll := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })
			table := routeTable{{name: "agent", handler: enroll, matches: onPath(func(p string) bool { return p == "/api/agent/enroll" })}}
			rec := &recordingTB{TB: t}
			checkOperationParity(rec, withAPIVersion(dispatcher(table, http.NotFoundHandler()), Options{}), operation,
				"/api/v1/agent/enroll", bodyTokenScheme, operationBodies["POST /api/agent/enroll"], "agent")
			if len(rec.failures) != 2 {
				t.Fatalf("failures = %d, want one per prefix: %q", len(rec.failures), rec.failures)
			}
			for _, failure := range rec.failures {
				if !strings.Contains(failure, "although its credential (token in request body) was sent") {
					t.Errorf("failure does not name the rejected credential: %q", failure)
				}
			}
		})
	}
}

// breakEventRoute stands in for a removed GET /api/alerts/events/{id} route: a request for one event reaches the alerts
// area with a path no alerts operation matches, so the real dispatcher takes its routing fallback.
func breakEventRoute(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/alerts/events/") && len(strings.Split(r.URL.Path, "/")) == 5 {
			r = r.Clone(r.Context())
			r.URL.Path += "/unrouted"
		}
		next.ServeHTTP(w, r)
	})
}

// alertsParityHandler serves the alerts area through the real dispatcher, with its event route broken when broken is set.
func alertsParityHandler(store OwnerAlertsStore, broken bool) http.Handler {
	auth := &serverAuth{service: nodeTestAuth{}, tokenLimiter: newNodeRateLimiter(0, 0)}
	area := newOwnerAlertsHandler(store, auth)
	if broken {
		area = breakEventRoute(area)
	}
	table := routeTable{{name: "alerts", handler: area, matches: onPath(ownerAlertsRoute)}}
	return withAPIVersion(dispatcher(table, http.NotFoundHandler()), Options{})
}

// TestParityNamesBrokenAlertsEventRoute checks the parameterised alerts operation GET /api/alerts/events/{id}, which the
// parity check of TestEveryOperationServedUnderAPIv1 uses. A broken route must fail on both prefixes and name the
// operation, while a missing event must pass.
func TestParityNamesBrokenAlertsEventRoute(t *testing.T) {
	operation := apicontract.Operation{Method: http.MethodGet, Path: "/api/alerts/events/{id}"}
	t.Run("broken route", func(t *testing.T) {
		rec := &recordingTB{TB: t}
		checkOperationParity(rec, alertsParityHandler(&fakeOwnerAlerts{}, true), operation, "/api/v1/alerts/events/{id}", "SessionCookie", "", "alerts")
		if len(rec.failures) != 2 {
			t.Fatalf("failures = %d, want one per prefix: %q", len(rec.failures), rec.failures)
		}
		for _, failure := range rec.failures {
			if !strings.Contains(failure, "GET /api/") || !strings.Contains(failure, "/alerts/events/") || !strings.Contains(failure, "the routing fallback answered") {
				t.Errorf("failure does not name the operation and the fallback: %q", failure)
			}
		}
	})
	t.Run("missing event", func(t *testing.T) {
		rec := &recordingTB{TB: t}
		checkOperationParity(rec, alertsParityHandler(&fakeOwnerAlerts{err: alerts.ErrNotFound}, false), operation, "/api/v1/alerts/events/{id}", "SessionCookie", "", "alerts")
		if len(rec.failures) != 0 {
			t.Errorf("a missing event was rejected: %q", rec.failures)
		}
	})
}

type errorParityCase struct {
	area, method, path, body string
	prepare                  func(*http.Request)
	wantStatus               int
}

func checkErrorParityPerArea(t *testing.T) {
	t.Helper()
	authHandler := NewHandler(Options{Auth: anonymousNodeTestAuth{}})
	handler := newNodeHandler(configuredOptions())
	areasByOperation, areas := specOperationAreas(t)
	cases := []errorParityCase{
		{area: "Auth", method: http.MethodPost, path: "/api/auth/login", body: `{"email":"bad address","password":"valid password"}`, prepare: ownerWriteWith(nil), wantStatus: http.StatusUnprocessableEntity},
		{area: "Access tokens", method: http.MethodGet, path: "/api/access-tokens?page=0", prepare: ownerRead, wantStatus: http.StatusUnprocessableEntity},
		{area: "Agent", method: http.MethodPost, path: "/api/agent/heartbeat", body: `{}`, prepare: func(r *http.Request) { r.Header.Set("Authorization", "Bearer good") }, wantStatus: http.StatusUnprocessableEntity},
		{area: "Nodes", method: http.MethodGet, path: "/api/nodes?size=0", prepare: ownerRead, wantStatus: http.StatusUnprocessableEntity},
		{area: "Results", method: http.MethodGet, path: "/api/results?range=1y", prepare: ownerRead, wantStatus: http.StatusUnprocessableEntity},
		{area: "Statistics", method: http.MethodGet, path: "/api/stats/trend?range=90d", prepare: ownerRead, wantStatus: http.StatusUnprocessableEntity},
		{area: "Bandwidth", method: http.MethodGet, path: "/api/usage?group=year", prepare: ownerRead, wantStatus: http.StatusUnprocessableEntity},
		{area: "Dashboard", method: http.MethodGet, path: "/api/dashboard?range=1y", prepare: ownerRead, wantStatus: http.StatusUnprocessableEntity},
		{area: "Schedules", method: http.MethodPut, path: "/api/schedules/rules", body: `{"stagger":true,"maxConcurrent":0,"quiet":{"quietStart":"","quietEnd":"","timezone":"UTC"}}`, prepare: ownerWriteWith(map[string]string{"If-Match": `"parity-schedules"`}), wantStatus: http.StatusUnprocessableEntity},
		{area: "Alerts", method: http.MethodGet, path: "/api/alerts/events?state=bad", prepare: ownerRead, wantStatus: http.StatusUnprocessableEntity},
		{area: "Notifications", method: http.MethodGet, path: "/api/notifications/deliveries?size=101", prepare: ownerRead, wantStatus: http.StatusUnprocessableEntity},
		{area: "Settings", method: http.MethodPatch, path: "/api/settings", body: `{"language":"fr"}`, prepare: ownerWriteWith(map[string]string{"If-Match": `"3"`}), wantStatus: http.StatusUnprocessableEntity},
		{area: "IP", method: http.MethodGet, path: "/api/ip/lookup?ip=x1.example", prepare: ownerRead, wantStatus: http.StatusUnprocessableEntity},
		{area: "Jobs", method: http.MethodGet, path: "/api/jobs/{id}", prepare: ownerRead, wantStatus: http.StatusNotFound},
		{area: "Installer", method: http.MethodGet, path: "/api/installations/{id}", prepare: ownerRead, wantStatus: http.StatusNotFound},
		{area: "ISPs", method: http.MethodGet, path: "/api/isps/compare?range=7d&isps=,,,", prepare: ownerRead, wantStatus: http.StatusUnprocessableEntity},
		{area: "Reports", method: http.MethodGet, path: "/api/reports/monthly?format=xml", prepare: ownerRead, wantStatus: http.StatusUnprocessableEntity},
	}
	seen := make(map[string]bool, len(cases))
	for _, tc := range cases {
		if seen[tc.area] {
			t.Errorf("multiple error comparisons for resource area %q", tc.area)
			continue
		}
		seen[tc.area] = true
		operation := tc.method + " " + strings.SplitN(tc.path, "?", 2)[0]
		if got := areasByOperation[operation]; got != tc.area {
			t.Errorf("error comparison %s belongs to area %q, want %q", operation, got, tc.area)
			continue
		}
		t.Run("error AC1 AC2 AC3 "+tc.area, func(t *testing.T) {
			areaHandler := handler
			if tc.area == "Auth" {
				areaHandler = authHandler
			}
			if err := checkErrorOperationParity(areaHandler, tc); err != nil {
				t.Fatal(err)
			}
			t.Logf("error response parity: area=%s operation=%s %s status=%d", tc.area, tc.method, strings.SplitN(tc.path, "?", 2)[0], tc.wantStatus)
		})
	}
	for area := range areas {
		if !seen[area] {
			t.Errorf("no error comparison for OpenAPI resource area %q", area)
		}
	}
}

func specOperationAreas(t *testing.T) (map[string]string, map[string]bool) {
	t.Helper()
	var spec struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(docs.OpenAPI, &spec); err != nil {
		t.Fatalf("parse docs/openapi.yaml resource areas: %v", err)
	}
	operations := make(map[string]string)
	areas := make(map[string]bool)
	for path, item := range spec.Paths {
		if !strings.HasPrefix(path, apiLegacyPrefix+"/") {
			continue
		}
		for method, raw := range item {
			if !specMethods[method] {
				continue
			}
			operation, _ := raw.(map[string]any)
			tags, _ := operation["tags"].([]any)
			if len(tags) == 0 {
				t.Fatalf("%s %s has no OpenAPI resource area", strings.ToUpper(method), path)
			}
			area, ok := tags[0].(string)
			if !ok || area == "" {
				t.Fatalf("%s %s has an invalid OpenAPI resource area: %v", strings.ToUpper(method), path, tags[0])
			}
			operations[strings.ToUpper(method)+" "+path] = area
			areas[area] = true
		}
	}
	return operations, areas
}

func checkErrorOperationParity(handler http.Handler, tc errorParityCase) error {
	legacy := concretePath(tc.path)
	versioned := concretePath(apiVersionPrefix + strings.TrimPrefix(tc.path, apiLegacyPrefix))
	onV1 := serveParityRequest(handler, tc.method, versioned, tc.body, tc.prepare)
	onAlias := serveParityRequest(handler, tc.method, legacy, tc.body, tc.prepare)
	if onV1.Code != onAlias.Code {
		return fmt.Errorf("status on %s = %d, on alias %s = %d", versioned, onV1.Code, legacy, onAlias.Code)
	}
	if onV1.Code != tc.wantStatus {
		return fmt.Errorf("%s error status = %d, want %d", tc.area, onV1.Code, tc.wantStatus)
	}
	if maskTimestamps(onV1.Body.String()) != maskTimestamps(onAlias.Body.String()) {
		return fmt.Errorf("body on %s = %q, on alias %s = %q", versioned, onV1.Body.String(), legacy, onAlias.Body.String())
	}
	if got := onAlias.Header().Get("Deprecation"); got != legacyAPIDeprecation {
		return fmt.Errorf("alias %s Deprecation = %q, want %q", legacy, got, legacyAPIDeprecation)
	}
	if got := onAlias.Header().Get("Sunset"); got != legacyAPISunset {
		return fmt.Errorf("alias %s Sunset = %q, want %q", legacy, got, legacyAPISunset)
	}
	if versionedResponseHasAliasHeaders(onV1.Header()) {
		return fmt.Errorf("%s carries alias headers: %v", versioned, onV1.Header())
	}
	if !sameExceptAliasHeaders(onV1.Header(), onAlias.Header()) {
		return fmt.Errorf("headers on %s = %v, on alias %s = %v", versioned, onV1.Header(), legacy, onAlias.Header())
	}
	return nil
}

func serveParityRequest(handler http.Handler, method, target, body string, prepare func(*http.Request)) *httptest.ResponseRecorder {
	return do(handler, method, target, body, func(r *http.Request) {
		r.Header.Set(requestIDHeader, parityRequestID)
		if prepare != nil {
			prepare(r)
		}
	})
}

// TestLegacyResponsesCarryDeprecationAndSunset checks that a legacy /api operation marks success and error responses
// alike, and that the Sunset value is the single constant and a valid HTTP-date.
func TestLegacyResponsesCarryDeprecationAndSunset(t *testing.T) {
	if _, err := http.ParseTime(legacyAPISunset); err != nil {
		t.Fatalf("legacyAPISunset %q is not an HTTP-date: %v", legacyAPISunset, err)
	}
	wantDeprecation := fmt.Sprintf("@%d", time.Date(2026, time.November, 17, 0, 0, 0, 0, time.UTC).Unix())
	if legacyAPIDeprecation != wantDeprecation {
		t.Fatalf("legacyAPIDeprecation = %q, want %q", legacyAPIDeprecation, wantDeprecation)
	}
	handler := ownerServersHandlerFor(&fakeOwnerServers{})
	path := "/api/nodes/" + nodeID + "/servers?engine=librespeed"
	responses := map[string]*httptest.ResponseRecorder{
		"success":         do(handler, http.MethodGet, path, "", ownerRead),
		"validation":      do(handler, http.MethodGet, "/api/nodes/"+nodeID+"/servers?engine=other", "", ownerRead),
		"unauthenticated": do(handler, http.MethodGet, path, "", nil),
	}
	wantStatus := map[string]int{"success": http.StatusOK, "validation": http.StatusUnprocessableEntity, "unauthenticated": http.StatusUnauthorized}
	for name, response := range responses {
		if response.Code != wantStatus[name] {
			t.Fatalf("%s status = %d, want %d", name, response.Code, wantStatus[name])
		}
		if got := response.Header().Get("Deprecation"); got != legacyAPIDeprecation {
			t.Errorf("%s Deprecation = %q, want %q", name, got, legacyAPIDeprecation)
		}
		if got := response.Header().Get("Sunset"); got != legacyAPISunset {
			t.Errorf("%s Sunset = %q, want %q", name, got, legacyAPISunset)
		}
	}
}

// TestTrailingSlashOwnerServersIsAnOperation checks that the owner server catalog accepts a trailing slash, as its
// dispatcher does, so the slash form is a documented operation: it is served, carries both alias headers, and matches
// the body of the slash-free path. The /api/v1 slash form is rewritten to the same handler without headers.
func TestTrailingSlashOwnerServersIsAnOperation(t *testing.T) {
	handler := ownerServersHandlerFor(&fakeOwnerServers{})
	query := "?engine=librespeed"
	plain := do(handler, http.MethodGet, "/api/nodes/"+nodeID+"/servers"+query, "", ownerRead)
	slash := do(handler, http.MethodGet, "/api/nodes/"+nodeID+"/servers/"+query, "", ownerRead)
	if slash.Code != http.StatusOK || slash.Code != plain.Code {
		t.Fatalf("slash form status = %d, plain = %d; want both 200", slash.Code, plain.Code)
	}
	if slash.Body.String() != plain.Body.String() {
		t.Errorf("slash form body = %q, plain = %q", slash.Body.String(), plain.Body.String())
	}
	if got := slash.Header().Get("Deprecation"); got != legacyAPIDeprecation {
		t.Errorf("slash form Deprecation = %q, want %q", got, legacyAPIDeprecation)
	}
	if got := slash.Header().Get("Sunset"); got != legacyAPISunset {
		t.Errorf("slash form Sunset = %q, want %q", got, legacyAPISunset)
	}
	versioned := do(handler, http.MethodGet, "/api/v1/nodes/"+nodeID+"/servers/"+query, "", ownerRead)
	if versioned.Code != http.StatusOK || versioned.Body.String() != plain.Body.String() {
		t.Errorf("/api/v1 slash form = %d %q, want 200 with the plain body", versioned.Code, versioned.Body.String())
	}
	if versioned.Header().Get("Deprecation") != "" || versioned.Header().Get("Sunset") != "" {
		t.Errorf("/api/v1 slash form carries alias headers: %v", versioned.Header())
	}
}

// TestTrailingSlashOwnerNodesAndAlertsAreOperations checks that the owner node and alert routes, which trim trailing
// slashes, serve their slash forms as operations: the slash form returns the same body as the slash-free form and
// carries both alias headers, while the /api/v1 slash form carries none.
func TestTrailingSlashOwnerNodesAndAlertsAreOperations(t *testing.T) {
	cases := []struct {
		name    string
		handler http.Handler
		plain   string
	}{
		{name: "nodes", handler: newNodeHandler(Options{OwnerNodes: &fakeOwnerNodes{}}), plain: "/api/nodes"},
		{name: "alert rules", handler: newNodeHandler(Options{OwnerAlerts: &fakeOwnerAlerts{}}), plain: "/api/alerts/rules"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checkSlashForm(t, tc.handler, tc.plain)
		})
	}
}

// checkSlashForm checks the owner route at plainPath and at plainPath+"/": the slash form is served with the plain
// body and both alias headers, and the /api/v1 slash form is served with the plain body and no alias headers.
func checkSlashForm(t *testing.T, handler http.Handler, plainPath string) {
	t.Helper()
	plain := do(handler, http.MethodGet, plainPath, "", ownerRead)
	slash := do(handler, http.MethodGet, plainPath+"/", "", ownerRead)
	if plain.Code != http.StatusOK || slash.Code != http.StatusOK {
		t.Fatalf("plain = %d, slash = %d; want both 200", plain.Code, slash.Code)
	}
	if maskTimestamps(slash.Body.String()) != maskTimestamps(plain.Body.String()) {
		t.Errorf("slash body = %q, plain = %q", slash.Body.String(), plain.Body.String())
	}
	if got := slash.Header().Get("Deprecation"); got != legacyAPIDeprecation {
		t.Errorf("slash Deprecation = %q, want %q", got, legacyAPIDeprecation)
	}
	if got := slash.Header().Get("Sunset"); got != legacyAPISunset {
		t.Errorf("slash Sunset = %q, want %q", got, legacyAPISunset)
	}
	versioned := do(handler, http.MethodGet, apiVersionPrefix+plainPath[len(apiLegacyPrefix):]+"/", "", ownerRead)
	if versioned.Code != http.StatusOK || maskTimestamps(versioned.Body.String()) != maskTimestamps(plain.Body.String()) {
		t.Errorf("/api/v1 slash = %d %q, want 200 with the plain body", versioned.Code, versioned.Body.String())
	}
	if versioned.Header().Get("Deprecation") != "" || versioned.Header().Get("Sunset") != "" {
		t.Errorf("/api/v1 slash carries alias headers: %v", versioned.Header())
	}
}

// TestUnknownPathsGetNoAliasHeaders checks that an unmatched /api/v1 path gets exactly the response the router gave it
// before the rewrite existed (the router without withAPIVersion, built from the same options), and that it carries no
// Deprecation or Sunset. The configured options have node credentials and no bearer token, so an agent path that is
// wrongly rewritten would answer 401 instead of the previous 404. The /api alias of an unmatched path answers the
// not-found problem and carries no alias headers.
func TestUnknownPathsGetNoAliasHeaders(t *testing.T) {
	anonymous := Options{}
	configured := Options{Auth: nodeTestAuth{}, NodeCredentials: &fakeNodeCredentialService{}}
	cases := []struct {
		name         string
		options      Options
		suffix       string
		compareAlias bool // the /api alias is also unmatched, so it answers the not-found problem
	}{
		{name: "anonymous unknown", options: anonymous, suffix: "/unknown", compareAlias: true},
		{name: "anonymous nested", options: anonymous, suffix: "/nodes/" + nodeID + "/unknown", compareAlias: true},
		{name: "anonymous root", options: anonymous, suffix: ""},
		{name: "agent without bearer", options: configured, suffix: "/agent/unknown"},
		{name: "auth trailing slash", options: configured, suffix: "/auth/me/", compareAlias: true},
		{name: "servers trailing slash without store", options: anonymous, suffix: "/nodes/" + nodeID + "/servers/", compareAlias: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			versioned := apiVersionPrefix + tc.suffix
			onV1 := serveSameRequest(NewHandler(tc.options), http.MethodGet, versioned, nil)
			before := serveSameRequest(newRouter(tc.options), http.MethodGet, versioned, nil)
			if onV1.Code != http.StatusNotFound {
				t.Fatalf("%s = %d, want 404", versioned, onV1.Code)
			}
			if onV1.Code != before.Code || onV1.Body.String() != before.Body.String() {
				t.Errorf("%s = %d %q, before the rewrite = %d %q", versioned, onV1.Code, onV1.Body.String(), before.Code, before.Body.String())
			}
			if onV1.Header().Get("Deprecation") != "" || onV1.Header().Get("Sunset") != "" {
				t.Errorf("%s carries alias headers: %v", versioned, onV1.Header())
			}
			if !tc.compareAlias {
				return
			}
			legacy := apiLegacyPrefix + tc.suffix
			onAlias := serveSameRequest(NewHandler(tc.options), http.MethodGet, legacy, nil)
			assertProblemResponse(t, onAlias, http.StatusNotFound, "/problems/notfound", "Resource not found or not owned", "Resource not found or not owned.", parityRequestID)
			if onAlias.Header().Get("Deprecation") != "" || onAlias.Header().Get("Sunset") != "" {
				t.Errorf("unmatched alias %s carries alias headers: %v", legacy, onAlias.Header())
			}
		})
	}
}
