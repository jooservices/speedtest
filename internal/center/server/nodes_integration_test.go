//go:build integration

package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
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
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,email,password_hash) VALUES ('00000000-0000-4000-8000-000000000901','owner@example.test','test-hash')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sessions(token_hash,user_id,csrf_token_hash,expires_at) VALUES ($1,'00000000-0000-4000-8000-000000000901',$2,now()+interval '1 hour')`, testSecretHash("session"), testSecretHash("csrf")); err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(Options{NodeCredentials: service})

	rotateKey := httptest.NewRequest(http.MethodPost, "http://center.test/api/settings/enrolment-key", nil)
	ownerHTTPWrite(rotateKey)
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
	ownerHTTPWrite(rotate)
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
	ownerHTTPWrite(revoke)
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
	t.Log("PASS: HTTP bootstrap, one-time enrollment, per-node bearer auth, rotation and immediate revocation")
}

func ownerHTTPWrite(request *http.Request) {
	request.AddCookie(&http.Cookie{Name: "speedtest_session", Value: "session"})
	request.Header.Set("X-CSRF-Token", "csrf")
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

func testSecretHash(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}
