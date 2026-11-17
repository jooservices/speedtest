package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/apicontract"
)

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

// serveSameRequest sends one request with a fixed request id, so both prefixes can be compared byte for byte.
func serveSameRequest(handler http.Handler, method, target string, owner bool) *httptest.ResponseRecorder {
	return do(handler, method, target, "", func(r *http.Request) {
		r.Header.Set(requestIDHeader, parityRequestID)
		if owner {
			ownerRead(r)
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
	stripped := func(h http.Header) string {
		c := h.Clone()
		c.Del("Deprecation")
		c.Del("Sunset")
		return fmt.Sprint(c)
	}
	return stripped(a) == stripped(b)
}

// TestEveryOperationServedUnderAPIv1 calls every docs/openapi.yaml operation on /api/v1 and on its /api alias, once
// with an anonymous handler and once with an owner session, and requires the same status, body and headers. Only the
// alias may carry Deprecation and Sunset. The handlers are bare (no stores), so this proves the routing and the header
// rules; each store handler is covered by its own tests.
func TestEveryOperationServedUnderAPIv1(t *testing.T) {
	operations, err := apicontract.Operations()
	if err != nil {
		t.Fatalf("load operations: %v", err)
	}
	if len(operations) == 0 {
		t.Fatal("no operations in docs/openapi.yaml")
	}
	variants := []struct {
		name    string
		handler http.Handler
		owner   bool
	}{
		{name: "anonymous", handler: NewHandler(Options{})},
		{name: "owner", handler: newNodeHandler(Options{OwnerServers: &fakeOwnerServers{}}), owner: true},
	}
	for _, variant := range variants {
		for _, operation := range operations {
			if !strings.HasPrefix(operation.Path, apiLegacyPrefix+"/") {
				// Only /api operations have a /api/v1 form; /install.sh and the download paths do not.
				continue
			}
			t.Run(variant.name+" "+operation.Method+" "+operation.Path, func(t *testing.T) {
				checkOperationParity(t, variant.handler, operation, variant.owner)
			})
		}
	}
}

// checkOperationParity compares one operation on /api/v1 with its /api alias: status, body and headers, with the
// alias headers checked on the original responses before the other headers are compared.
func checkOperationParity(t *testing.T, handler http.Handler, operation apicontract.Operation, owner bool) {
	t.Helper()
	legacy := concretePath(operation.Path)
	versioned := apiVersionPrefix + strings.TrimPrefix(legacy, apiLegacyPrefix)
	onV1 := serveSameRequest(handler, operation.Method, versioned, owner)
	onAlias := serveSameRequest(handler, operation.Method, legacy, owner)
	if onV1.Code != onAlias.Code {
		t.Fatalf("status on %s = %d, on alias %s = %d", versioned, onV1.Code, legacy, onAlias.Code)
	}
	if maskTimestamps(onV1.Body.String()) != maskTimestamps(onAlias.Body.String()) {
		t.Errorf("body on %s = %q, on alias %s = %q", versioned, onV1.Body.String(), legacy, onAlias.Body.String())
	}
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
			onV1 := serveSameRequest(NewHandler(tc.options), http.MethodGet, versioned, false)
			before := serveSameRequest(newRouter(tc.options), http.MethodGet, versioned, false)
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
			onAlias := serveSameRequest(NewHandler(tc.options), http.MethodGet, legacy, false)
			assertProblemResponse(t, onAlias, http.StatusNotFound, "/problems/notfound", "Resource not found or not owned", "Resource not found or not owned.", parityRequestID)
			if onAlias.Header().Get("Deprecation") != "" || onAlias.Header().Get("Sunset") != "" {
				t.Errorf("unmatched alias %s carries alias headers: %v", legacy, onAlias.Header())
			}
		})
	}
}
