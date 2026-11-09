package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jooservices/speedtest/internal/center/apitoken"
)

type tokenAuthStore struct {
	principal apitoken.Principal
	err       error
	calls     int
}

func (s *tokenAuthStore) Authenticate(_ context.Context, raw string) (apitoken.Principal, error) {
	s.calls++
	if raw != "valid-token" {
		return apitoken.Principal{}, apitoken.ErrInvalidToken
	}
	return s.principal, s.err
}
func (*tokenAuthStore) Create(context.Context, string, apitoken.CreateInput) (apitoken.Token, string, error) {
	return apitoken.Token{}, "", nil
}
func (*tokenAuthStore) List(context.Context, string, int, int) ([]apitoken.Token, int, error) {
	return nil, 0, nil
}
func (*tokenAuthStore) Revoke(context.Context, string, string) error { return nil }

func TestAPITokenScopeAuthenticationAndCSRFBypass(t *testing.T) {
	store := &tokenAuthStore{principal: apitoken.Principal{TokenID: "token-1", UserID: "owner-1", Email: "owner@example.test", Scope: apitoken.ReadOnly}}
	auth := &serverAuth{tokens: store, tokenLimiter: newNodeRateLimiter(120, 20)}
	called := 0
	read := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		secured := auth.requireOwner(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			owner, err := ownerPrincipal(r)
			if err != nil || owner != "owner-1" {
				t.Errorf("owner principal = %q, %v", owner, err)
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		secured.ServeHTTP(w, r)
	})
	routes := routeTable{{matches: onPath(func(path string) bool { return path == "/api/nodes" }), handler: read, tokenScope: operationScope}}
	handler := withAPITokenAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		read.ServeHTTP(w, r)
	}), routes, auth)
	request := httptest.NewRequest(http.MethodGet, "https://center.test/api/nodes", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || called != 1 {
		t.Fatalf("read token request = %d %s, handler calls %d", response.Code, response.Body.String(), called)
	}
}

func TestAPITokenRejectsInvalidScopeAndMalformedBearer(t *testing.T) {
	store := &tokenAuthStore{principal: apitoken.Principal{TokenID: "token-1", UserID: "owner-1", Scope: apitoken.ReadOnly}}
	auth := &serverAuth{tokens: store, tokenLimiter: newNodeRateLimiter(120, 20)}
	routes := routeTable{{matches: onPath(func(path string) bool { return strings.HasSuffix(path, "/run") }), tokenScope: operationScope}}
	handler := withAPITokenAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }), routes, auth)
	for _, authorization := range []string{"Bearer valid-token", "Bearer", "Basic valid-token"} {
		request := httptest.NewRequest(http.MethodPost, "https://center.test/api/nodes/123e4567-e89b-12d3-a456-426614174000/run", nil)
		request.Header.Set("Authorization", authorization)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized && authorization != "Bearer valid-token" {
			t.Errorf("Authorization %q status = %d, want 401", authorization, response.Code)
		}
		if authorization == "Bearer valid-token" && response.Code != http.StatusForbidden {
			t.Errorf("read token write status = %d, want 403", response.Code)
		}
		if strings.Contains(response.Body.String(), authorization) {
			t.Errorf("response disclosed Authorization value %q", authorization)
		}
	}
	store.err = errors.New("database detail")
	request := httptest.NewRequest(http.MethodPost, "https://center.test/api/nodes/123e4567-e89b-12d3-a456-426614174000/run", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "database detail") {
		t.Fatalf("store failure response = %d %s", response.Code, response.Body.String())
	}
}

func TestAPITokenPerTokenRateLimit(t *testing.T) {
	store := &tokenAuthStore{principal: apitoken.Principal{TokenID: "token-1", UserID: "owner-1", Scope: apitoken.ReadOnly}}
	auth := &serverAuth{tokens: store, tokenLimiter: newNodeRateLimiter(1, 1)}
	routes := routeTable{{matches: onPath(func(path string) bool { return path == "/api/nodes" }), tokenScope: operationScope}}
	handler := withAPITokenAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }), routes, auth)
	for i, want := range []int{http.StatusNoContent, http.StatusTooManyRequests} {
		request := httptest.NewRequest(http.MethodGet, "https://center.test/api/nodes", nil)
		request.Header.Set("Authorization", "Bearer valid-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Errorf("request %d status = %d, want %d", i+1, response.Code, want)
		}
		if want == http.StatusTooManyRequests && response.Header().Get("Retry-After") == "" {
			t.Error("rate limited response has no Retry-After")
		}
	}
}

func TestTokenPermissionUsesOpenAPIForCreateNodePlatformRoute(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/nodes", nil)
	scope, ok := (routeTable{}).tokenPermission(request)
	if !ok || scope != apitoken.ReadWrite {
		t.Fatalf("POST /api/nodes token permission = (%q, %t), want read-write", scope, ok)
	}
}
