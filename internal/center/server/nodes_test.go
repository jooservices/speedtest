package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/auth"
	"github.com/jooservices/speedtest/internal/center/nodes"
)

type fakeNodeCredentialService struct {
	createdPrincipal string
	createdKey       string
	createdInput     nodes.Input
	createdCalls     int
	rotatedNode      string
	revokedNode      string
	authCalls        int
	createErr        error
	enrollErr        error
	rotateErr        error
	revokeErr        error
	authErr          error
	validateKeyErr   error
	rotateKeyErr     error
}

func (f *fakeNodeCredentialService) CreateNode(_ context.Context, input nodes.Input, principal, key string) (json.RawMessage, bool, error) {
	f.createdInput, f.createdPrincipal, f.createdKey = input, principal, key
	f.createdCalls++
	if f.createErr != nil {
		return nil, false, f.createErr
	}
	return json.RawMessage(`{"node":{"id":"00000000-0000-4000-8000-000000000001"},"token":"one-time","expiresAt":"2026-11-04T00:00:00Z"}`), false, nil
}
func (f *fakeNodeCredentialService) Enroll(_ context.Context, token, name, host, agent string) (map[string]any, error) {
	if f.enrollErr != nil {
		return nil, f.enrollErr
	}
	if token != "one-time" {
		return nil, nodes.ErrInvalidToken
	}
	return map[string]any{"nodeId": "node-id", "credential": "node-secret", "config": map[string]any{}}, nil
}
func (f *fakeNodeCredentialService) RotateCredential(_ context.Context, id, _ string, _ string) (json.RawMessage, bool, error) {
	f.rotatedNode = id
	if f.rotateErr != nil {
		return nil, false, f.rotateErr
	}
	return json.RawMessage(`{"nodeId":"node-id","credential":"new-secret","config":{}}`), false, nil
}
func (f *fakeNodeCredentialService) RevokeCredential(_ context.Context, id string) error {
	f.revokedNode = id
	return f.revokeErr
}
func (f *fakeNodeCredentialService) AuthenticateAgent(_ context.Context, credential string) (string, error) {
	f.authCalls++
	if f.authErr != nil {
		return "", f.authErr
	}
	if credential == "revoked" || credential == "wrong" {
		return "", nodes.ErrInvalidAuth
	}
	return "node-id", nil
}
func (f *fakeNodeCredentialService) ValidateEnrollmentKey(_ context.Context, key string) (string, error) {
	if f.validateKeyErr != nil {
		return "", f.validateKeyErr
	}
	if key != "settings-key" {
		return "", nodes.ErrInvalidAuth
	}
	return "enrollment:key-hash", nil
}
func (f *fakeNodeCredentialService) RotateEnrollmentKey(_ context.Context, principal, _ string) (json.RawMessage, bool, error) {
	if f.rotateKeyErr != nil {
		return nil, false, f.rotateKeyErr
	}
	if principal != "owner-id" {
		return nil, false, nodes.ErrUnauthorized
	}
	return json.RawMessage(`{"enrollToken":"issued-key","createdAt":"2026-11-03T00:00:00Z"}`), false, nil
}

type nodeTestAuth struct{ scriptedAuthService }

func (nodeTestAuth) Session(_ context.Context, token string) (auth.SessionView, error) {
	if token != "owner-session" {
		return auth.SessionView{}, auth.ErrInvalidSession
	}
	return auth.SessionView{
		Token: token, Identity: &auth.Identity{ID: "owner-id", Email: "owner@example.test"},
		CSRFToken: "csrf-value", ExpiresAt: time.Now().Add(time.Hour),
	}, nil
}

type anonymousNodeTestAuth struct{ nodeTestAuth }

func (anonymousNodeTestAuth) Session(_ context.Context, token string) (auth.SessionView, error) {
	if token != "owner-session" {
		return auth.SessionView{}, auth.ErrInvalidSession
	}
	return auth.SessionView{Token: token, CSRFToken: "csrf-value", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func newNodeHandler(options Options) http.Handler {
	if options.Auth == nil {
		options.Auth = nodeTestAuth{}
	}
	return NewHandler(options)
}

func TestCreateNodeAuthenticatedAndAppliesDefaults(t *testing.T) {
	service := &fakeNodeCredentialService{}
	handler := newNodeHandler(Options{NodeCredentials: service})
	req := httptest.NewRequest(http.MethodPost, "/api/nodes", strings.NewReader(`{"name":"edge","expectedDown":300,"expectedUp":100}`))
	ownerWrite(req)
	req.Header.Set("Idempotency-Key", "node-create-1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Location"); got != "/api/nodes/00000000-0000-4000-8000-000000000001" {
		t.Errorf("Location = %q", got)
	}
	if service.createdPrincipal != "owner-id" || service.createdKey != "node-create-1" || service.createdCalls != 1 {
		t.Fatalf("create call = principal %q key %q count %d", service.createdPrincipal, service.createdKey, service.createdCalls)
	}
	if service.createdInput.Threads != 4 || !service.createdInput.Enabled || service.createdInput.ServerID != nil {
		t.Errorf("input defaults = %+v", service.createdInput)
	}
	var envelope apiEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || !envelope.Success || envelope.Code != 201 {
		t.Fatalf("response envelope = %s, %v", response.Body.String(), err)
	}
}

func TestOwnerEndpointsFailClosedWithoutValidSessionCSRFAndOrigin(t *testing.T) {
	for _, test := range []struct {
		name, cookie, csrf, origin string
		want                       int
	}{
		{name: "missing session", csrf: "csrf-value", origin: "http://center.test", want: http.StatusUnauthorized},
		{name: "wrong origin", cookie: "owner-session", csrf: "csrf-value", origin: "http://attacker.test", want: http.StatusForbidden},
		{name: "wrong csrf", cookie: "owner-session", csrf: "bad", origin: "http://center.test", want: http.StatusForbidden},
		{name: "scheme mismatch", cookie: "owner-session", csrf: "csrf-value", origin: "https://center.test", want: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := newNodeHandler(Options{NodeCredentials: &fakeNodeCredentialService{}})
			req := httptest.NewRequest(http.MethodPost, "/api/nodes", strings.NewReader(`{"name":"edge","expectedDown":300,"expectedUp":100}`))
			if test.cookie != "" {
				req.AddCookie(&http.Cookie{Name: "speedtest_session", Value: test.cookie})
			}
			req.Header.Set("X-CSRF-Token", test.csrf)
			req.Header.Set("Origin", test.origin)
			req.Header.Set("Idempotency-Key", "create")
			req.Host = "center.test"
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != test.want {
				t.Fatalf("status = %d body=%s, want %d", response.Code, response.Body.String(), test.want)
			}
		})
	}
	duplicate := httptest.NewRequest(http.MethodPost, "/api/nodes", strings.NewReader(`{}`))
	duplicate.AddCookie(&http.Cookie{Name: "speedtest_session", Value: "owner-session"})
	duplicate.Header.Add("Cookie", "speedtest_session=another")
	ownerWrite(duplicate)
	duplicate.Header.Set("Idempotency-Key", "create")
	response := httptest.NewRecorder()
	newNodeHandler(Options{NodeCredentials: &fakeNodeCredentialService{}}).ServeHTTP(response, duplicate)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("duplicate session cookie status = %d", response.Code)
	}
}

func TestOwnerEndpointsRejectAnonymousAuthSession(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://center.test/api/settings/enrolment-key", nil)
	ownerWrite(request)
	response := httptest.NewRecorder()
	NewHandler(Options{Auth: anonymousNodeTestAuth{}, NodeCredentials: &fakeNodeCredentialService{}}).ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("owner endpoint with anonymous session status = %d, want 401", response.Code)
	}
}

func TestBootstrapEnrollmentAndCredentialEndpoints(t *testing.T) {
	service := &fakeNodeCredentialService{}
	handler := newNodeHandler(Options{NodeCredentials: service})
	bootstrap := httptest.NewRequest(http.MethodPost, "/api/agent/tokens", strings.NewReader(`{"node":{"name":"edge","expectedDown":300,"expectedUp":100}}`))
	bootstrap.Header.Set("Authorization", "Bearer settings-key")
	bootstrap.Header.Set("Idempotency-Key", "bootstrap-create")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, bootstrap)
	if response.Code != http.StatusCreated || service.createdPrincipal != "enrollment:key-hash" {
		t.Fatalf("bootstrap status=%d principal=%q body=%s", response.Code, service.createdPrincipal, response.Body.String())
	}

	enroll := httptest.NewRequest(http.MethodPost, "/api/agent/enroll", strings.NewReader(`{"token":"one-time","name":"edge","agent":"speedtest-agent/1.0"}`))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, enroll)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), "node-secret") {
		t.Fatalf("enroll status=%d body=%s", response.Code, response.Body.String())
	}

	rotateKey := httptest.NewRequest(http.MethodPost, "/api/settings/enrolment-key", nil)
	ownerWrite(rotateKey)
	rotateKey.Header.Set("Idempotency-Key", "key-rotate")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, rotateKey)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), "issued-key") {
		t.Fatalf("key rotation status=%d body=%s", response.Code, response.Body.String())
	}

	rotate := httptest.NewRequest(http.MethodPost, "/api/nodes/00000000-0000-4000-8000-000000000001/credential", nil)
	ownerWrite(rotate)
	rotate.Header.Set("Idempotency-Key", "credential-rotate")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, rotate)
	if response.Code != http.StatusCreated || service.rotatedNode == "" {
		t.Fatalf("credential rotation status=%d body=%s", response.Code, response.Body.String())
	}

	revoke := httptest.NewRequest(http.MethodDelete, "/api/nodes/00000000-0000-4000-8000-000000000001/credential", nil)
	ownerWrite(revoke)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, revoke)
	if response.Code != http.StatusNoContent || service.revokedNode == "" {
		t.Fatalf("revoke status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestAgentMiddlewareRejectsWrongAndRevokedAndRateLimitsByNode(t *testing.T) {
	service := &fakeNodeCredentialService{}
	handler := newNodeHandler(Options{NodeCredentials: service, AgentRateLimit: 1})
	for _, token := range []string{"wrong", "revoked"} {
		req := httptest.NewRequest(http.MethodGet, "/api/agent/unknown", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusUnauthorized {
			t.Errorf("token %q status = %d", token, response.Code)
		}
	}
	for i := 0; i < defaultAgentBurst; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/agent/unknown", nil)
		req.Header.Set("Authorization", "Bearer good")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusNotFound {
			t.Fatalf("request %d status = %d", i, response.Code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/agent/unknown", nil)
	req.Header.Set("Authorization", "Bearer good")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") == "" {
		t.Fatalf("rate limited status=%d headers=%v", response.Code, response.Header())
	}
}

func TestCredentialAPIValidationAndFailClosedDispatch(t *testing.T) {
	service := &fakeNodeCredentialService{}
	handler := newNodeHandler(Options{NodeCredentials: service})
	for _, body := range []string{`{`, `{"name":"edge","expectedDown":1,"expectedUp":1,"extra":true}`, `{} {}`} {
		req := httptest.NewRequest(http.MethodPost, "/api/agent/enroll", strings.NewReader(body))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusBadRequest {
			t.Errorf("body %q status = %d", body, response.Code)
		}
	}
	large := httptest.NewRequest(http.MethodPost, "/api/agent/enroll", strings.NewReader(`{"token":"`+strings.Repeat("x", maxNodeRequestBody)+`"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, large)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("large body status = %d", response.Code)
	}
	badMethod := httptest.NewRequest(http.MethodGet, "/api/agent/enroll", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, badMethod)
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodPost {
		t.Errorf("method response = %d %v", response.Code, response.Header())
	}
	unknown := httptest.NewRequest(http.MethodGet, "/api/unknown", nil)
	response = httptest.NewRecorder()
	newNodeHandler(Options{}).ServeHTTP(response, unknown)
	if response.Code != http.StatusNotFound {
		t.Errorf("unknown path without service status = %d", response.Code)
	}
	known := httptest.NewRequest(http.MethodPost, "/api/agent/enroll", nil)
	response = httptest.NewRecorder()
	newNodeHandler(Options{}).ServeHTTP(response, known)
	if response.Code != http.StatusServiceUnavailable {
		t.Errorf("known path without service status = %d", response.Code)
	}
}

func TestRateLimiterRefillsAndBoundsState(t *testing.T) {
	limiter := newNodeRateLimiter(60, 1)
	current := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	limiter.now = func() time.Time { return current }
	if allowed, _ := limiter.allow("node"); !allowed {
		t.Fatal("first request denied")
	}
	if allowed, wait := limiter.allow("node"); allowed || wait <= 0 {
		t.Fatalf("second request = %t wait=%s", allowed, wait)
	}
	current = current.Add(2 * time.Second)
	if allowed, _ := limiter.allow("node"); !allowed {
		t.Fatal("request was not allowed after refill")
	}
	if allowed, _ := newNodeRateLimiter(0, 0).allow("default"); !allowed {
		t.Fatal("default limiter denied initial request")
	}
}

func ownerWrite(req *http.Request) {
	req.AddCookie(&http.Cookie{Name: "speedtest_session", Value: "owner-session"})
	req.Header.Set("X-CSRF-Token", "csrf-value")
	req.Header.Set("Origin", "http://center.test")
	req.Host = "center.test"
}

func TestSameOriginAndBearerParsing(t *testing.T) {
	good := httptest.NewRequest(http.MethodPost, "http://center.test/api/nodes", nil)
	good.Header.Set("Origin", "http://center.test")
	if !sameOrigin(good) {
		t.Fatal("same origin rejected")
	}
	bad := httptest.NewRequest(http.MethodPost, "http://center.test/api/nodes", nil)
	bad.Header.Set("Origin", "null")
	if sameOrigin(bad) {
		t.Fatal("opaque origin accepted")
	}
	forwarded := httptest.NewRequest(http.MethodPost, "http://center.test/api/nodes", nil)
	forwarded.Header.Set("Origin", "https://center.test")
	forwarded.Header.Set("X-Forwarded-Proto", "https")
	trustedAuth := &serverAuth{trustedProxyCIDRs: "192.0.2.0/24"}
	if !trustedAuth.sameOrigin(forwarded) {
		t.Fatal("trusted HTTPS forwarded origin rejected")
	}
	forwarded.Header.Add("X-Forwarded-Proto", "http")
	if trustedAuth.sameOrigin(forwarded) {
		t.Fatal("ambiguous forwarded scheme accepted")
	}
	for _, value := range []string{"Bearer token", "bearer token"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", value)
		if got, ok := bearerValue(req); !ok || got != "token" {
			t.Errorf("bearerValue(%q) = %q, %t", value, got, ok)
		}
	}
	for _, value := range []string{"token", "Basic token", "Bearer one two", "Bearer " + strings.Repeat("x", 8193)} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", value)
		if _, ok := bearerValue(req); ok {
			t.Errorf("bearerValue(%q) accepted", value)
		}
	}
	duplicate := httptest.NewRequest(http.MethodGet, "/", nil)
	duplicate.Header.Add("Authorization", "Bearer one")
	duplicate.Header.Add("Authorization", "Bearer two")
	if _, ok := bearerValue(duplicate); ok {
		t.Fatal("duplicate Authorization header accepted")
	}
}

func TestServiceErrorDoesNotExposeInternalDetails(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/agent/unknown", nil)
	for _, test := range []struct {
		name string
		err  error
		want int
	}{
		{"unauthorized", nodes.ErrUnauthorized, http.StatusUnauthorized},
		{"invalid credential", nodes.ErrInvalidAuth, http.StatusUnauthorized},
		{"invalid enrollment token", nodes.ErrInvalidToken, http.StatusUnauthorized},
		{"forbidden", nodes.ErrForbidden, http.StatusForbidden},
		{"not found", nodes.ErrNotFound, http.StatusNotFound},
		{"conflict", nodes.ErrConflict, http.StatusConflict},
		{"invalid input", nodes.ErrInvalidInput, http.StatusUnprocessableEntity},
		{"crypto key missing", nodes.ErrCryptoKeyUnset, http.StatusServiceUnavailable},
		{"internal", fmt.Errorf("private database detail"), http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			writeServiceError(response, request, test.err)
			if response.Code != test.want || strings.Contains(response.Body.String(), "private database detail") {
				t.Fatalf("error response = %d %s, want status %d without internal detail", response.Code, response.Body.String(), test.want)
			}
		})
	}
}

func TestNodeInputWireOptionalFieldsAndValidation(t *testing.T) {
	name, label, provider, plan, note := "edge", "West rack", "isp", "fibre", "nightly"
	down, up, threads, serverID := 500.0, 120.0, 8, 12
	enabled, autoUpdate := false, true
	iface, sourceIP := "eth0", "192.0.2.18"
	selection := nodes.ServerSelection{ID: &serverID}
	wire := nodeInputWire{
		Name: &name, Label: &label, Provider: &provider, Plan: &plan,
		ExpectedDown: &down, ExpectedUp: &up, Threads: &threads, Server: &selection,
		Note: &note, Enabled: &enabled, Binding: &nodes.Binding{Iface: iface, SourceIP: &sourceIP}, AutoUpdate: &autoUpdate,
	}
	got, err := wire.input()
	if err != nil {
		t.Fatalf("input() error = %v", err)
	}
	if got.Label != label || got.Provider != provider || got.Plan != plan || got.Threads != threads || got.ServerID == nil || *got.ServerID != serverID || got.Note != note || got.Enabled || !got.AutoUpdate || got.Binding.Iface != iface || got.Binding.SourceIP == nil || *got.Binding.SourceIP != sourceIP {
		t.Fatalf("input() values = %+v", got)
	}
	if _, err := (nodeInputWire{}).input(); !errors.Is(err, nodes.ErrInvalidInput) {
		t.Fatalf("missing required fields error = %v", err)
	}
	zeroThreads := 0
	if _, err := (nodeInputWire{Name: &name, ExpectedDown: &down, ExpectedUp: &up, Threads: &zeroThreads}).input(); !errors.Is(err, nodes.ErrInvalidInput) {
		t.Fatalf("zero threads error = %v", err)
	}
}

func TestCredentialHandlerValidationErrorsAndLimits(t *testing.T) {
	t.Run("owner create method and request validation", func(t *testing.T) {
		handler := newNodeHandler(Options{NodeCredentials: &fakeNodeCredentialService{}})
		for _, test := range []struct {
			name, method, body string
			withKey            bool
			want               int
		}{
			{"wrong method", http.MethodGet, "", true, http.StatusMethodNotAllowed},
			{"missing idempotency key", http.MethodPost, `{"name":"edge","expectedDown":1,"expectedUp":1}`, false, http.StatusBadRequest},
			{"invalid JSON", http.MethodPost, `{`, true, http.StatusBadRequest},
			{"invalid node", http.MethodPost, `{"name":"edge","expectedDown":0,"expectedUp":1}`, true, http.StatusUnprocessableEntity},
		} {
			t.Run(test.name, func(t *testing.T) {
				req := httptest.NewRequest(test.method, "/api/nodes", strings.NewReader(test.body))
				ownerWrite(req)
				if test.withKey {
					req.Header.Set("Idempotency-Key", "create-key")
				}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, req)
				if response.Code != test.want {
					t.Fatalf("status = %d body=%s, want %d", response.Code, response.Body.String(), test.want)
				}
			})
		}
	})

	t.Run("bootstrap errors", func(t *testing.T) {
		for _, test := range []struct {
			name, auth, key, body string
			serviceErr            error
			want                  int
		}{
			{"missing key", "", "", `{"node":{"name":"edge","expectedDown":1,"expectedUp":1}}`, nil, http.StatusUnauthorized},
			{"invalid key", "Bearer wrong", "", `{"node":{"name":"edge","expectedDown":1,"expectedUp":1}}`, nil, http.StatusUnauthorized},
			{"missing idempotency", "Bearer settings-key", "", `{"node":{"name":"edge","expectedDown":1,"expectedUp":1}}`, nil, http.StatusBadRequest},
			{"invalid input", "Bearer settings-key", "key", `{}`, nil, http.StatusUnprocessableEntity},
			{"create conflict", "Bearer settings-key", "key", `{"node":{"name":"edge","expectedDown":1,"expectedUp":1}}`, nodes.ErrConflict, http.StatusConflict},
		} {
			t.Run(test.name, func(t *testing.T) {
				service := &fakeNodeCredentialService{createErr: test.serviceErr}
				req := httptest.NewRequest(http.MethodPost, "/api/agent/tokens", strings.NewReader(test.body))
				if test.auth != "" {
					req.Header.Set("Authorization", test.auth)
				}
				if test.key != "" {
					req.Header.Set("Idempotency-Key", test.key)
				}
				response := httptest.NewRecorder()
				newNodeHandler(Options{NodeCredentials: service}).ServeHTTP(response, req)
				if response.Code != test.want {
					t.Fatalf("status = %d body=%s, want %d", response.Code, response.Body.String(), test.want)
				}
			})
		}
	})

	t.Run("enrollment and owner credential failures", func(t *testing.T) {
		cases := []struct {
			name, method, path, body string
			owner                    bool
			key                      string
			service                  *fakeNodeCredentialService
			want                     int
		}{
			{"enrollment rejection", http.MethodPost, "/api/agent/enroll", `{"token":"bad","name":"edge","agent":"agent"}`, false, "", &fakeNodeCredentialService{enrollErr: nodes.ErrInvalidToken}, http.StatusUnauthorized},
			{"enrollment method", http.MethodGet, "/api/agent/enroll", "", false, "", &fakeNodeCredentialService{}, http.StatusMethodNotAllowed},
			{"malformed node credential path", http.MethodGet, "/api/nodes/bad/path/credential", "", false, "", &fakeNodeCredentialService{}, http.StatusNotFound},
			{"credential method", http.MethodGet, "/api/nodes/node-id/credential", "", true, "", &fakeNodeCredentialService{}, http.StatusMethodNotAllowed},
			{"rotation idempotency", http.MethodPost, "/api/nodes/node-id/credential", "", true, "", &fakeNodeCredentialService{}, http.StatusBadRequest},
			{"rotation not found", http.MethodPost, "/api/nodes/node-id/credential", "", true, "rotate", &fakeNodeCredentialService{rotateErr: nodes.ErrNotFound}, http.StatusNotFound},
			{"revocation internal error", http.MethodDelete, "/api/nodes/node-id/credential", "", true, "", &fakeNodeCredentialService{revokeErr: errors.New("database unavailable")}, http.StatusInternalServerError},
			{"rotate key method", http.MethodGet, "/api/settings/enrolment-key", "", true, "", &fakeNodeCredentialService{}, http.StatusMethodNotAllowed},
			{"rotate key idempotency", http.MethodPost, "/api/settings/enrolment-key", "", true, "", &fakeNodeCredentialService{}, http.StatusBadRequest},
			{"rotate key conflict", http.MethodPost, "/api/settings/enrolment-key", "", true, "key", &fakeNodeCredentialService{rotateKeyErr: nodes.ErrConflict}, http.StatusConflict},
		}
		for _, test := range cases {
			t.Run(test.name, func(t *testing.T) {
				req := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
				if test.owner {
					ownerWrite(req)
				}
				if test.key != "" {
					req.Header.Set("Idempotency-Key", test.key)
				}
				response := httptest.NewRecorder()
				newNodeHandler(Options{NodeCredentials: test.service}).ServeHTTP(response, req)
				if response.Code != test.want {
					t.Fatalf("status = %d body=%s, want %d", response.Code, response.Body.String(), test.want)
				}
			})
		}
	})

	t.Run("agent middleware method and authentication errors", func(t *testing.T) {
		for _, test := range []struct {
			name, method, auth string
			service            *fakeNodeCredentialService
			want               int
		}{
			{"method", http.MethodPatch, "Bearer good", &fakeNodeCredentialService{}, http.StatusMethodNotAllowed},
			{"missing bearer", http.MethodGet, "", &fakeNodeCredentialService{}, http.StatusUnauthorized},
			{"database error", http.MethodGet, "Bearer good", &fakeNodeCredentialService{authErr: errors.New("database unavailable")}, http.StatusInternalServerError},
		} {
			t.Run(test.name, func(t *testing.T) {
				req := httptest.NewRequest(test.method, "/api/agent/unknown", nil)
				if test.auth != "" {
					req.Header.Set("Authorization", test.auth)
				}
				response := httptest.NewRecorder()
				newNodeHandler(Options{NodeCredentials: test.service}).ServeHTTP(response, req)
				if response.Code != test.want {
					t.Fatalf("status = %d body=%s, want %d", response.Code, response.Body.String(), test.want)
				}
			})
		}
	})
}

func TestOwnerRateLimitIdempotencyHeadersAndOriginBoundaries(t *testing.T) {
	service := &fakeNodeCredentialService{}
	handler := newNodeHandler(Options{NodeCredentials: service, AgentRateLimit: 1})
	for i := 0; i < defaultAgentBurst; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/nodes", strings.NewReader(`{"name":"edge","expectedDown":1,"expectedUp":1}`))
		ownerWrite(req)
		req.Header.Set("Idempotency-Key", fmt.Sprintf("owner-%d", i))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusCreated {
			t.Fatalf("owner request %d status = %d", i, response.Code)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/api/nodes", strings.NewReader(`{"name":"edge","expectedDown":1,"expectedUp":1}`))
	ownerWrite(req)
	req.Header.Set("Idempotency-Key", "owner-over-limit")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("owner rate limit status = %d", response.Code)
	}

	for _, headers := range [][]string{{""}, {"one", "two"}, {strings.Repeat("x", 129)}} {
		req := httptest.NewRequest(http.MethodPost, "/api/settings/enrolment-key", nil)
		ownerWrite(req)
		for _, value := range headers {
			req.Header.Add("Idempotency-Key", value)
		}
		response := httptest.NewRecorder()
		newNodeHandler(Options{NodeCredentials: &fakeNodeCredentialService{}}).ServeHTTP(response, req)
		if response.Code != http.StatusBadRequest {
			t.Errorf("Idempotency-Key values %q status = %d", headers, response.Code)
		}
	}

	for _, origin := range []string{"%", "http://user@center.test", "http://center.test?x=1", "http://center.test#fragment", "http://center.test/path", "ftp://center.test", "http://other.test"} {
		request := httptest.NewRequest(http.MethodPost, "http://center.test/api/nodes", nil)
		request.Header.Set("Origin", origin)
		if sameOrigin(request) {
			t.Errorf("sameOrigin accepted %q", origin)
		}
	}
	duplicateOrigin := httptest.NewRequest(http.MethodPost, "http://center.test/api/nodes", nil)
	duplicateOrigin.Header.Add("Origin", "http://center.test")
	duplicateOrigin.Header.Add("Origin", "http://center.test")
	if sameOrigin(duplicateOrigin) {
		t.Fatal("duplicate origin accepted")
	}
	pathOrigin := httptest.NewRequest(http.MethodPost, "http://center.test/api/nodes", nil)
	pathOrigin.Header.Set("Origin", "http://center.test/")
	if !sameOrigin(pathOrigin) {
		t.Fatal("root origin path rejected")
	}
}

func TestLimiterStateBoundsAndHTTPHelpers(t *testing.T) {
	now := time.Now()
	full := newNodeRateLimiter(60, 1)
	full.now = func() time.Time { return now }
	for i := 0; i < 100000; i++ {
		full.buckets[fmt.Sprintf("node-%d", i)] = rateBucket{tokens: 1, last: now, seen: now}
	}
	if allowed, _ := full.allow("overflow"); allowed {
		t.Fatal("limiter accepted a new node beyond the state cap")
	}
	stale := newNodeRateLimiter(60, 1)
	stale.now = func() time.Time { return now }
	for i := 0; i < 256; i++ {
		stale.buckets[fmt.Sprintf("old-%d", i)] = rateBucket{tokens: 1, last: now.Add(-2 * time.Hour), seen: now.Add(-2 * time.Hour)}
	}
	if allowed, _ := stale.allow("new"); !allowed {
		t.Fatal("limiter did not discard stale state")
	}
	fast := newNodeRateLimiter(60000, 1)
	fast.now = func() time.Time { return now }
	if allowed, _ := fast.allow("fast"); !allowed {
		t.Fatal("fast limiter denied its first token")
	}
	fast.buckets["fraction"] = rateBucket{tokens: 0.5, last: now, seen: now}
	if allowed, wait := fast.allow("fraction"); allowed || wait < time.Second {
		t.Fatalf("sub-second retry delay = %t, %s", allowed, wait)
	}

	if got := clientIP(&http.Request{RemoteAddr: "192.0.2.2"}); got != "192.0.2.2" {
		t.Errorf("clientIP without port = %q", got)
	}
	if got := clientIP(&http.Request{RemoteAddr: "[2001:db8::1]:1234"}); got != "2001:db8::1" {
		t.Errorf("clientIP IPv6 = %q", got)
	}
	response := httptest.NewRecorder()
	setLocation(response, json.RawMessage(`{`), "/api/nodes/")
	if response.Header().Get("Location") != "" {
		t.Errorf("invalid response set Location = %q", response.Header().Get("Location"))
	}
}

func TestCredentialHandlerServiceAndSessionBoundaries(t *testing.T) {
	t.Run("owner creation service failure", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/nodes", strings.NewReader(`{"name":"edge","expectedDown":1,"expectedUp":1}`))
		ownerWrite(req)
		req.Header.Set("Idempotency-Key", "owner-create")
		response := httptest.NewRecorder()
		newNodeHandler(Options{NodeCredentials: &fakeNodeCredentialService{createErr: nodes.ErrConflict}}).ServeHTTP(response, req)
		if response.Code != http.StatusConflict {
			t.Fatalf("owner create status = %d", response.Code)
		}
	})
	t.Run("bootstrap malformed JSON and rate limit", func(t *testing.T) {
		service := &fakeNodeCredentialService{}
		handler := newNodeHandler(Options{NodeCredentials: service, AgentRateLimit: 1})
		bad := httptest.NewRequest(http.MethodPost, "/api/agent/tokens", strings.NewReader("{"))
		bad.Header.Set("Authorization", "Bearer settings-key")
		bad.Header.Set("Idempotency-Key", "bad-json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, bad)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("bootstrap malformed JSON status = %d", response.Code)
		}
		for i := 1; i < defaultAgentBurst; i++ {
			req := httptest.NewRequest(http.MethodPost, "/api/agent/tokens", strings.NewReader(`{"node":{"name":"edge","expectedDown":1,"expectedUp":1}}`))
			req.Header.Set("Authorization", "Bearer settings-key")
			req.Header.Set("Idempotency-Key", fmt.Sprintf("bootstrap-%d", i))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != http.StatusCreated {
				t.Fatalf("bootstrap request %d status = %d", i, response.Code)
			}
		}
		req := httptest.NewRequest(http.MethodPost, "/api/agent/tokens", strings.NewReader(`{"node":{"name":"edge","expectedDown":1,"expectedUp":1}}`))
		req.Header.Set("Authorization", "Bearer settings-key")
		req.Header.Set("Idempotency-Key", "bootstrap-over-limit")
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusTooManyRequests {
			t.Fatalf("bootstrap rate limit status = %d", response.Code)
		}
	})
	t.Run("enrollment rate limit", func(t *testing.T) {
		handler := newNodeHandler(Options{NodeCredentials: &fakeNodeCredentialService{}, AgentRateLimit: 1})
		for i := 0; i < defaultAgentBurst; i++ {
			req := httptest.NewRequest(http.MethodPost, "/api/agent/enroll", strings.NewReader(`{"token":"one-time","name":"edge","agent":"agent"}`))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != http.StatusCreated {
				t.Fatalf("enrollment request %d status = %d", i, response.Code)
			}
		}
		req := httptest.NewRequest(http.MethodPost, "/api/agent/enroll", strings.NewReader(`{"token":"one-time","name":"edge","agent":"agent"}`))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusTooManyRequests {
			t.Fatalf("enrollment over-limit status = %d", response.Code)
		}
	})
	t.Run("credential management rate limit", func(t *testing.T) {
		handler := newNodeHandler(Options{NodeCredentials: &fakeNodeCredentialService{}, AgentRateLimit: 1})
		for i := 0; i < defaultAgentBurst; i++ {
			req := httptest.NewRequest(http.MethodPost, "/api/nodes/node-id/credential", nil)
			ownerWrite(req)
			req.Header.Set("Idempotency-Key", fmt.Sprintf("rotate-%d", i))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != http.StatusCreated {
				t.Fatalf("credential request %d status = %d", i, response.Code)
			}
		}
		req := httptest.NewRequest(http.MethodPost, "/api/nodes/node-id/credential", nil)
		ownerWrite(req)
		req.Header.Set("Idempotency-Key", "rotate-over-limit")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusTooManyRequests {
			t.Fatalf("credential over-limit status = %d", response.Code)
		}
	})
	t.Run("owner session failures on settings and credential routes", func(t *testing.T) {
		for _, path := range []string{"/api/settings/enrolment-key", "/api/nodes/node-id/credential"} {
			req := httptest.NewRequest(http.MethodPost, path, nil)
			req.Header.Set("Origin", "http://center.test")
			req.Host = "center.test"
			if path == "/api/nodes/node-id/credential" {
				req.Header.Set("Idempotency-Key", "rotate")
			}
			response := httptest.NewRecorder()
			newNodeHandler(Options{NodeCredentials: &fakeNodeCredentialService{}}).ServeHTTP(response, req)
			if response.Code != http.StatusUnauthorized {
				t.Errorf("%s without session status = %d", path, response.Code)
			}
		}
	})
	t.Run("node handler path and owner method guards", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/nodes/malformed", nil)
		response := httptest.NewRecorder()
		handleNodeCredential(&fakeNodeCredentialService{}, newNodeRateLimiter(60, 1), response, req)
		if response.Code != http.StatusNotFound {
			t.Fatalf("direct malformed node path status = %d", response.Code)
		}
		if _, err := ownerPrincipal(httptest.NewRequest(http.MethodGet, "/api/nodes", nil)); !errors.Is(err, nodes.ErrUnauthorized) {
			t.Fatalf("ownerPrincipal without authenticated context error = %v", err)
		}
	})
	t.Run("TLS origin", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "https://center.test/api/nodes", nil)
		req.TLS = &tls.ConnectionState{}
		req.Header.Set("Origin", "https://center.test")
		if !sameOrigin(req) {
			t.Fatal("same-origin TLS request rejected")
		}
	})
}
