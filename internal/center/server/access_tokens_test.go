package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/apitoken"
)

type accessTokenTestStore struct {
	items       []apitoken.Token
	createToken apitoken.Token
	raw         string
	total       int
	createErr   error
	listErr     error
	revokeErr   error
	listedPage  int
	listedSize  int
	ownerID     string
	name        string
	scope       apitoken.Scope
}

func (*accessTokenTestStore) Authenticate(context.Context, string) (apitoken.Principal, error) {
	return apitoken.Principal{}, apitoken.ErrInvalidToken
}
func (s *accessTokenTestStore) Create(_ context.Context, owner string, input apitoken.CreateInput) (apitoken.Token, string, error) {
	s.ownerID, s.name, s.scope = owner, input.Name, input.Scope
	return s.createToken, s.raw, s.createErr
}
func (s *accessTokenTestStore) List(_ context.Context, owner string, page, size int) ([]apitoken.Token, int, error) {
	s.ownerID, s.listedPage, s.listedSize = owner, page, size
	return s.items, s.total, s.listErr
}
func (s *accessTokenTestStore) Revoke(_ context.Context, owner, id string) error {
	s.ownerID, s.name = owner, id
	return s.revokeErr
}

func TestAccessTokenHTTPCreateListsAndRevokes(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	store := &accessTokenTestStore{raw: "st_at_one-time", createToken: apitoken.Token{ID: "123e4567-e89b-12d3-a456-426614174000", Name: "automation", Scope: apitoken.ReadWrite, CreatedAt: now}}
	handler := newAccessTokenHandler(store, &serverAuth{})
	request := withTestOwner(http.MethodPost, "/api/access-tokens", `{"name":"automation","scope":"read-write"}`)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || response.Header().Get("Location") != "/api/access-tokens/123e4567-e89b-12d3-a456-426614174000" || !strings.Contains(response.Body.String(), store.raw) {
		t.Fatalf("create response status=%d location=%q", response.Code, response.Header().Get("Location"))
	}
	var created struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil || created.Data["id"] == nil || created.Data["token"] == nil || created.Data["accessToken"] != nil {
		t.Fatalf("create response data does not match the documented flat token shape: %v", err)
	}
	if store.ownerID != "owner-id" || store.name != "automation" || store.scope != apitoken.ReadWrite {
		t.Fatalf("create arguments owner=%q name=%q scope=%q", store.ownerID, store.name, store.scope)
	}
	store.items, store.total = []apitoken.Token{store.createToken}, 1
	list := withTestOwner(http.MethodGet, "/api/access-tokens?page=2&size=3", "")
	listResponse := httptest.NewRecorder()
	handler.ServeHTTP(listResponse, list)
	if listResponse.Code != http.StatusOK || strings.Contains(listResponse.Body.String(), store.raw) || store.listedPage != 2 || store.listedSize != 3 {
		t.Fatalf("list response status=%d page=%d size=%d", listResponse.Code, store.listedPage, store.listedSize)
	}
	revoke := withTestOwner(http.MethodDelete, "/api/access-tokens/123e4567-e89b-12d3-a456-426614174000", "")
	revokeResponse := httptest.NewRecorder()
	handler.ServeHTTP(revokeResponse, revoke)
	if revokeResponse.Code != http.StatusNoContent || store.name != "123e4567-e89b-12d3-a456-426614174000" {
		t.Fatalf("revoke response status=%d id=%q", revokeResponse.Code, store.name)
	}
}

func TestAccessTokenHTTPValidationAndSafeFailures(t *testing.T) {
	store := &accessTokenTestStore{raw: "st_at_never-returned"}
	handler := newAccessTokenHandler(store, &serverAuth{})
	for _, test := range []struct {
		name, body string
		status     int
	}{
		{"malformed", `{`, http.StatusBadRequest},
		{"unknown field", `{"name":"x","scope":"read","secret":"secret-value"}`, http.StatusBadRequest},
		{"trailing JSON", `{"name":"x","scope":"read"}{}`, http.StatusBadRequest},
		{"invalid input", `{"name":"x","scope":"admin"}`, http.StatusUnprocessableEntity},
		{"oversized", `{"name":"` + strings.Repeat("x", maxAccessTokenBody) + `","scope":"read"}`, http.StatusRequestEntityTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, withTestOwner(http.MethodPost, "/api/access-tokens", test.body))
			if response.Code != test.status || strings.Contains(response.Body.String(), "secret-value") {
				t.Fatalf("response status=%d body length=%d", response.Code, response.Body.Len())
			}
		})
	}
	store.createErr = errors.New("database detail")
	failedCreate := httptest.NewRecorder()
	handler.ServeHTTP(failedCreate, withTestOwner(http.MethodPost, "/api/access-tokens", `{"name":"x","scope":"read"}`))
	if failedCreate.Code != http.StatusInternalServerError || strings.Contains(failedCreate.Body.String(), "database detail") || strings.Contains(failedCreate.Body.String(), store.raw) {
		t.Fatalf("create error response status=%d", failedCreate.Code)
	}
	store.listErr = errors.New("database detail")
	failedList := httptest.NewRecorder()
	handler.ServeHTTP(failedList, withTestOwner(http.MethodGet, "/api/access-tokens", ""))
	if failedList.Code != http.StatusInternalServerError || strings.Contains(failedList.Body.String(), "database detail") {
		t.Fatalf("list error response status=%d", failedList.Code)
	}
	store.revokeErr = errors.New("database detail")
	failedRevoke := httptest.NewRecorder()
	handler.ServeHTTP(failedRevoke, withTestOwner(http.MethodDelete, "/api/access-tokens/123e4567-e89b-12d3-a456-426614174000", ""))
	if failedRevoke.Code != http.StatusInternalServerError || strings.Contains(failedRevoke.Body.String(), "database detail") {
		t.Fatalf("revoke error response status=%d", failedRevoke.Code)
	}
}

func TestAccessTokenHTTPRejectsBearerManagementAndInvalidIDs(t *testing.T) {
	store := &accessTokenTestStore{}
	handler := newAccessTokenHandler(store, &serverAuth{})
	request := httptest.NewRequest(http.MethodGet, "/api/access-tokens", nil)
	request.Header.Set("Authorization", "Bearer any-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("bearer token management status=%d", response.Code)
	}
	for _, path := range []string{"/api/access-tokens/not-a-uuid", "/api/access-tokens/123e4567-e89b-12d3-a456-426614174000/extra"} {
		revoke := withTestOwner(http.MethodDelete, path, "")
		revokeResponse := httptest.NewRecorder()
		handler.ServeHTTP(revokeResponse, revoke)
		if revokeResponse.Code != http.StatusNotFound {
			t.Errorf("DELETE %s status=%d, want 404", path, revokeResponse.Code)
		}
	}
	wrongMethod := withTestOwner(http.MethodPut, "/api/access-tokens", "")
	methodResponse := httptest.NewRecorder()
	handler.ServeHTTP(methodResponse, wrongMethod)
	if methodResponse.Code != http.StatusMethodNotAllowed || methodResponse.Header().Get("Allow") != "GET, POST, DELETE" {
		t.Fatalf("wrong method status=%d Allow=%q", methodResponse.Code, methodResponse.Header().Get("Allow"))
	}
}

func TestAccessTokenHTTPRejectsMissingOwnerAndUnknownToken(t *testing.T) {
	store := &accessTokenTestStore{revokeErr: apitoken.ErrNotFound}
	handler := newAccessTokenHandler(store, &serverAuth{})
	for name, call := range map[string]func(http.ResponseWriter, *http.Request){
		"list":   (&accessTokens{store: store}).list,
		"create": (&accessTokens{store: store}).create,
		"revoke": (&accessTokens{store: store}).revoke,
	} {
		response := httptest.NewRecorder()
		call(response, httptest.NewRequest(http.MethodGet, "/api/access-tokens", nil))
		if response.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated direct %s status = %d", name, response.Code)
		}
	}
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/access-tokens", nil),
		httptest.NewRequest(http.MethodPost, "/api/access-tokens", strings.NewReader(`{"name":"x","scope":"read"}`)),
		httptest.NewRequest(http.MethodDelete, "/api/access-tokens/123e4567-e89b-12d3-a456-426614174000", nil),
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusServiceUnavailable {
			t.Errorf("unauthenticated %s status = %d", request.Method, response.Code)
		}
	}
	invalidPage := httptest.NewRecorder()
	handler.ServeHTTP(invalidPage, withTestOwner(http.MethodGet, "/api/access-tokens?page=0", ""))
	if invalidPage.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid token page status = %d", invalidPage.Code)
	}
	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, withTestOwner(http.MethodDelete, "/api/access-tokens/123e4567-e89b-12d3-a456-426614174000", ""))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing token revoke status = %d", missing.Code)
	}
}

func withTestOwner(method, path, body string) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	principal := apitoken.Principal{TokenID: "test-token", UserID: "owner-id", Email: "owner@example.test", Scope: apitoken.Full}
	return request.WithContext(apitoken.WithPrincipal(request.Context(), principal))
}
