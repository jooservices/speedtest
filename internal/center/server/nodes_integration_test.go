//go:build integration

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/auth"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/nodes"
	"github.com/jooservices/speedtest/internal/shared/config"
)

func openServerIntegrationPool(t *testing.T) (*pgxpool.Pool, context.Context, []byte) {
	t.Helper()
	values, err := godotenv.Read(filepath.Join("..", "..", "..", ".env"))
	if err != nil {
		t.Fatal("a configured worktree .env is required")
	}
	for key, value := range values {
		t.Setenv(key, value)
	}
	cfg, err := config.LoadCenter()
	if err != nil {
		t.Fatal("invalid worktree test configuration")
	}
	if err := db.CheckTestDatabase(cfg.Database.Name); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var name, marker string
	if err := pool.QueryRow(ctx, `SELECT current_database(),shobj_description(oid,'pg_database') FROM pg_database WHERE datname=current_database()`).Scan(&name, &marker); err != nil || name != cfg.Database.Name || marker != "workspaces-project:speedtest" {
		t.Fatal("integration database identity and ownership marker verification failed")
	}
	t.Log("PASS: API integration uses the verified non-live Speedtest worktree database")
	return pool, ctx, bytes.Repeat([]byte{0x31}, 32)
}

func TestNodeCredentialHTTPFlowIntegration(t *testing.T) {
	pool, ctx, appKey := openServerIntegrationPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	service := nodes.NewService(pool, appKey)
	passwordHash, err := auth.HashPassword("integration test owner password")
	if err != nil {
		t.Fatal("could not prepare integration owner")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,email,password_hash) VALUES ('00000000-0000-4000-8000-000000000901','owner@example.test',$1)`, passwordHash); err != nil {
		t.Fatal(err)
	}
	authStore, err := auth.NewPostgresStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	authService, err := auth.NewService(authStore, auth.Config{SessionTTL: time.Hour, LoginMaxAttempts: 5, LoginWindow: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(Options{NodeCredentials: service, Auth: authService})
	session, csrf := loginIntegrationOwner(t, handler)

	withoutSession := httptest.NewRequest(http.MethodPost, "http://center.test/api/settings/enrolment-key", nil)
	withoutSession.Header.Set("Origin", "http://center.test")
	withoutSession.Header.Set("Idempotency-Key", "missing-owner-session")
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, withoutSession)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("Settings key without owner session status = %d, want 401", unauthorized.Code)
	}

	rotateKey := httptest.NewRequest(http.MethodPost, "http://center.test/api/settings/enrolment-key", nil)
	ownerHTTPWrite(rotateKey, session, csrf)
	rotateKey.Header.Set("Idempotency-Key", "api-key-rotation")
	keyResponse := httptest.NewRecorder()
	handler.ServeHTTP(keyResponse, rotateKey)
	if keyResponse.Code != http.StatusCreated {
		t.Fatalf("Settings key rotation status = %d", keyResponse.Code)
	}
	key := responseString(t, keyResponse.Body.Bytes(), "data", "enrollToken")

	create := httptest.NewRequest(http.MethodPost, "http://center.test/api/agent/tokens", strings.NewReader(`{"node":{"name":"http-integration-edge","expectedDown":300,"expectedUp":100}}`))
	create.Header.Set("Authorization", "Bearer "+key)
	create.Header.Set("Idempotency-Key", "api-node-create")
	createResponse := httptest.NewRecorder()
	handler.ServeHTTP(createResponse, create)
	if createResponse.Code != http.StatusCreated {
		t.Fatalf("bootstrap create status = %d", createResponse.Code)
	}
	nodeID := responseString(t, createResponse.Body.Bytes(), "data", "node", "id")
	token := responseString(t, createResponse.Body.Bytes(), "data", "token")

	enroll := httptest.NewRequest(http.MethodPost, "http://center.test/api/agent/enroll", strings.NewReader(`{"token":"`+token+`","name":"http-integration-edge","agent":"speedtest-agent/1.0"}`))
	enrollResponse := httptest.NewRecorder()
	handler.ServeHTTP(enrollResponse, enroll)
	if enrollResponse.Code != http.StatusCreated {
		t.Fatalf("agent enrollment status = %d", enrollResponse.Code)
	}
	credential := responseString(t, enrollResponse.Body.Bytes(), "data", "credential")

	validAgent := httptest.NewRequest(http.MethodGet, "http://center.test/api/agent/heartbeat", nil)
	validAgent.Header.Set("Authorization", "Bearer "+credential)
	validResponse := httptest.NewRecorder()
	handler.ServeHTTP(validResponse, validAgent)
	if validResponse.Code != http.StatusNotFound {
		t.Fatalf("valid agent middleware status = %d, want route placeholder 404", validResponse.Code)
	}

	rotate := httptest.NewRequest(http.MethodPost, "http://center.test/api/nodes/"+nodeID+"/credential", nil)
	ownerHTTPWrite(rotate, session, csrf)
	rotate.Header.Set("Idempotency-Key", "api-credential-rotation")
	rotateResponse := httptest.NewRecorder()
	handler.ServeHTTP(rotateResponse, rotate)
	if rotateResponse.Code != http.StatusCreated {
		t.Fatalf("credential rotation status = %d", rotateResponse.Code)
	}
	newCredential := responseString(t, rotateResponse.Body.Bytes(), "data", "credential")
	oldAgent := httptest.NewRequest(http.MethodGet, "http://center.test/api/agent/heartbeat", nil)
	oldAgent.Header.Set("Authorization", "Bearer "+credential)
	oldResponse := httptest.NewRecorder()
	handler.ServeHTTP(oldResponse, oldAgent)
	if oldResponse.Code != http.StatusUnauthorized {
		t.Fatalf("old bearer after rotation status = %d", oldResponse.Code)
	}

	revoke := httptest.NewRequest(http.MethodDelete, "http://center.test/api/nodes/"+nodeID+"/credential", nil)
	ownerHTTPWrite(revoke, session, csrf)
	revokeResponse := httptest.NewRecorder()
	handler.ServeHTTP(revokeResponse, revoke)
	if revokeResponse.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d", revokeResponse.Code)
	}
	revokedAgent := httptest.NewRequest(http.MethodGet, "http://center.test/api/agent/heartbeat", nil)
	revokedAgent.Header.Set("Authorization", "Bearer "+newCredential)
	revokedResponse := httptest.NewRecorder()
	handler.ServeHTTP(revokedResponse, revokedAgent)
	if revokedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("revoked bearer status = %d", revokedResponse.Code)
	}
	t.Log("PASS: anonymous auth bootstrap, login, session+CSRF protected node calls, one-time enrollment, per-node bearer auth, rotation and immediate revocation")
}

func loginIntegrationOwner(t *testing.T, handler http.Handler) (string, string) {
	t.Helper()
	bootstrap := httptest.NewRecorder()
	handler.ServeHTTP(bootstrap, httptest.NewRequest(http.MethodGet, "http://center.test/api/auth/me", nil))
	if bootstrap.Code != http.StatusOK {
		t.Fatalf("anonymous GET /api/auth/me status = %d", bootstrap.Code)
	}
	var anonymousCookie *http.Cookie
	for _, cookie := range bootstrap.Result().Cookies() {
		if cookie.Name == sessionCookieName {
			anonymousCookie = cookie
		}
	}
	if anonymousCookie == nil {
		t.Fatal("anonymous auth bootstrap did not set a session cookie")
	}
	anonymousCSRF := responseString(t, bootstrap.Body.Bytes(), "data", "csrfToken")
	login := httptest.NewRequest(http.MethodPost, "http://center.test/api/auth/login", strings.NewReader(`{"email":"owner@example.test","password":"integration test owner password"}`))
	login.AddCookie(anonymousCookie)
	login.Header.Set("Origin", "http://center.test")
	login.Header.Set("X-CSRF-Token", anonymousCSRF)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, login)
	if response.Code != http.StatusOK {
		t.Fatalf("owner login status = %d", response.Code)
	}
	var ownerCookie *http.Cookie
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == sessionCookieName {
			ownerCookie = cookie
		}
	}
	if ownerCookie == nil {
		t.Fatal("owner login did not set an authenticated session cookie")
	}
	csrf := responseString(t, response.Body.Bytes(), "data", "csrfToken")
	return ownerCookie.Value, csrf
}

func ownerHTTPWrite(request *http.Request, session, csrf string) {
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
	request.Header.Set("X-CSRF-Token", csrf)
	request.Header.Set("Origin", "http://center.test")
	request.Host = "center.test"
}

func responseString(t *testing.T, body []byte, path ...string) string {
	t.Helper()
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatal("invalid API JSON response")
	}
	for _, key := range path {
		object, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("response path %v is missing", path)
		}
		value, ok = object[key]
		if !ok {
			t.Fatalf("response path %v is missing", path)
		}
	}
	result, ok := value.(string)
	if !ok || result == "" {
		t.Fatalf("response path %v is not a non-empty string", path)
	}
	return result
}
