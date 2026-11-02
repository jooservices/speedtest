//go:build integration

package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/jooservices/speedtest/internal/center/server"
	"github.com/jooservices/speedtest/internal/shared/config"
)

func integrationPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	path, err := filepath.Abs("../../../.env")
	if err != nil {
		t.Fatal(err)
	}
	values, err := godotenv.Read(path)
	if err != nil {
		t.Fatal("a configured worktree .env is required")
	}
	for key, value := range values {
		t.Setenv(key, value)
	}
	cfg, err := config.LoadCenter()
	if err != nil {
		t.Fatal("invalid worktree database configuration")
	}
	if err := db.CheckTestDatabase(cfg.Database.Name); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var name, marker string
	err = pool.QueryRow(ctx, `SELECT current_database(),shobj_description(oid,'pg_database')
		FROM pg_database WHERE datname=current_database()`).Scan(&name, &marker)
	if err != nil || name != cfg.Database.Name || marker != "workspaces-project:speedtest" {
		t.Fatal("worktree database identity/ownership marker verification failed")
	}
	t.Log("PASS: worktree database name and ownership marker verified")
	return pool, ctx
}

func TestConfiguredWorktreeDatabaseIdentity(t *testing.T) {
	_, _ = integrationPool(t)
}

func TestAuthLoginMeLogoutMeIntegration(t *testing.T) {
	pool, ctx := integrationPool(t)
	if err := db.Migrate(ctx, pool, "up"); err != nil {
		t.Fatal(err)
	}
	store, err := auth.NewPostgresStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	service, err := auth.NewService(store, auth.Config{SessionTTL: time.Hour, LoginMaxAttempts: 10, LoginWindow: time.Minute})
	if err != nil {
		t.Fatal(err)
	}

	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	email := fmt.Sprintf("joost12-%s@example.test", hex.EncodeToString(suffix[:]))
	password := "integration owner password"
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SeedAdmin(ctx, email, hash); err != nil {
		t.Fatal(err)
	}
	user, err := store.FindUserByEmail(ctx, email)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, user.ID)
	})

	handler := server.NewHandler(server.Options{Auth: service})
	expiredTokenHash := sha256.Sum256([]byte("expired-session-fixture"))
	expiredHash := hex.EncodeToString(expiredTokenHash[:])
	if _, err := pool.Exec(ctx, `INSERT INTO sessions(token_hash,csrf_token_hash,created_at,expires_at)
		VALUES ($1,'expired-csrf-hash',now()-interval '2 hours',now()-interval '1 hour')`, expiredHash); err != nil {
		t.Fatal(err)
	}
	bootstrap := httptest.NewRecorder()
	handler.ServeHTTP(bootstrap, httptest.NewRequest(http.MethodGet, "http://speedtest.local/api/auth/me", nil))
	if bootstrap.Code != http.StatusOK {
		t.Fatalf("GET /api/auth/me bootstrap = %d: %s", bootstrap.Code, bootstrap.Body.String())
	}
	var bootstrapData struct {
		Data struct {
			User      *json.RawMessage `json:"user"`
			CSRFToken string           `json:"csrfToken"`
		} `json:"data"`
	}
	if err := json.Unmarshal(bootstrap.Body.Bytes(), &bootstrapData); err != nil || bootstrapData.Data.User != nil || bootstrapData.Data.CSRFToken == "" {
		t.Fatalf("anonymous bootstrap body invalid: %v", err)
	}
	var expiredRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE token_hash=$1`, expiredHash).Scan(&expiredRows); err != nil || expiredRows != 0 {
		t.Fatal("anonymous bootstrap did not prune expired sessions")
	}
	anonymousCookie := bootstrap.Result().Cookies()[0]
	csrf := bootstrapData.Data.CSRFToken

	login := httptest.NewRecorder()
	loginRequest := httptest.NewRequest(http.MethodPost, "http://speedtest.local/api/auth/login", strings.NewReader(fmt.Sprintf(`{"email":%q,"password":%q}`, email, password)))
	loginRequest.AddCookie(anonymousCookie)
	loginRequest.Header.Set("Origin", "http://speedtest.local")
	loginRequest.Header.Set("X-CSRF-Token", csrf)
	handler.ServeHTTP(login, loginRequest)
	if login.Code != http.StatusOK {
		t.Fatalf("POST /api/auth/login = %d: %s", login.Code, login.Body.String())
	}
	var loginData struct {
		Data struct {
			User *struct {
				ID    string `json:"id"`
				Email string `json:"email"`
			} `json:"user"`
			CSRFToken string `json:"csrfToken"`
		} `json:"data"`
	}
	if err := json.Unmarshal(login.Body.Bytes(), &loginData); err != nil || loginData.Data.User == nil || loginData.Data.User.ID != user.ID {
		t.Fatalf("login response identity mismatch: %v", err)
	}
	authenticatedCookie := login.Result().Cookies()[0]
	if authenticatedCookie.Value == anonymousCookie.Value || !authenticatedCookie.HttpOnly || authenticatedCookie.SameSite != http.SameSiteLaxMode {
		t.Fatal("login did not rotate the anonymous cookie with safe attributes")
	}
	var storedTokenHash string
	if err := pool.QueryRow(ctx, `SELECT token_hash FROM sessions WHERE user_id=$1`, user.ID).Scan(&storedTokenHash); err != nil {
		t.Fatal(err)
	}
	if storedTokenHash == authenticatedCookie.Value {
		t.Fatal("database stored a plaintext session token")
	}

	me := httptest.NewRecorder()
	meRequest := httptest.NewRequest(http.MethodGet, "http://speedtest.local/api/auth/me", nil)
	meRequest.AddCookie(authenticatedCookie)
	handler.ServeHTTP(me, meRequest)
	if me.Code != http.StatusOK {
		t.Fatalf("GET /api/auth/me authenticated = %d: %s", me.Code, me.Body.String())
	}

	logout := httptest.NewRecorder()
	logoutRequest := httptest.NewRequest(http.MethodPost, "http://speedtest.local/api/auth/logout", nil)
	logoutRequest.AddCookie(authenticatedCookie)
	logoutRequest.Header.Set("Origin", "http://speedtest.local")
	logoutRequest.Header.Set("X-CSRF-Token", loginData.Data.CSRFToken)
	handler.ServeHTTP(logout, logoutRequest)
	if logout.Code != http.StatusNoContent || len(logout.Result().Cookies()) == 0 || logout.Result().Cookies()[0].MaxAge >= 0 {
		t.Fatalf("POST /api/auth/logout = %d; cookie was not expired", logout.Code)
	}

	oldSession := httptest.NewRecorder()
	oldSessionRequest := httptest.NewRequest(http.MethodGet, "http://speedtest.local/api/auth/me", nil)
	oldSessionRequest.AddCookie(authenticatedCookie)
	handler.ServeHTTP(oldSession, oldSessionRequest)
	if oldSession.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/auth/me after logout = %d, want 401", oldSession.Code)
	}

	bootstrapAgain := func() (string, string) {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://speedtest.local/api/auth/me", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET /api/auth/me before password change = %d", response.Code)
		}
		var data struct {
			Data struct {
				CSRFToken string `json:"csrfToken"`
			} `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
		return response.Result().Cookies()[0].Value, data.Data.CSRFToken
	}
	loginAgain := func(rawCookie, token string) (*http.Cookie, string) {
		t.Helper()
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "http://speedtest.local/api/auth/login", strings.NewReader(fmt.Sprintf(`{"email":%q,"password":%q}`, email, password)))
		request.AddCookie(&http.Cookie{Name: "speedtest_session", Value: rawCookie})
		request.Header.Set("Origin", "http://speedtest.local")
		request.Header.Set("X-CSRF-Token", token)
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("POST /api/auth/login before password change = %d: %s", response.Code, response.Body.String())
		}
		var data struct {
			Data struct {
				CSRFToken string `json:"csrfToken"`
			} `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
		return response.Result().Cookies()[0], data.Data.CSRFToken
	}
	passwordBootstrap, passwordCSRF := bootstrapAgain()
	passwordSession, passwordSessionCSRF := loginAgain(passwordBootstrap, passwordCSRF)
	otherBootstrap, otherCSRF := bootstrapAgain()
	otherSession, _ := loginAgain(otherBootstrap, otherCSRF)

	wrongPassword := httptest.NewRecorder()
	wrongPasswordRequest := httptest.NewRequest(http.MethodPost, "http://speedtest.local/api/auth/password", strings.NewReader(`{"currentPassword":"incorrect current password","newPassword":"a newly changed owner password"}`))
	wrongPasswordRequest.AddCookie(passwordSession)
	wrongPasswordRequest.Header.Set("Origin", "http://speedtest.local")
	wrongPasswordRequest.Header.Set("X-CSRF-Token", passwordSessionCSRF)
	handler.ServeHTTP(wrongPassword, wrongPasswordRequest)
	if wrongPassword.Code != http.StatusUnauthorized {
		t.Fatalf("POST /api/auth/password with incorrect current password = %d", wrongPassword.Code)
	}

	const newPassword = "a newly changed owner password"
	change := httptest.NewRecorder()
	changeRequest := httptest.NewRequest(http.MethodPost, "http://speedtest.local/api/auth/password", strings.NewReader(fmt.Sprintf(`{"currentPassword":%q,"newPassword":%q}`, password, newPassword)))
	changeRequest.AddCookie(passwordSession)
	changeRequest.Header.Set("Origin", "http://speedtest.local")
	changeRequest.Header.Set("X-CSRF-Token", passwordSessionCSRF)
	handler.ServeHTTP(change, changeRequest)
	if change.Code != http.StatusNoContent || len(change.Result().Cookies()) == 0 {
		t.Fatalf("POST /api/auth/password = %d: %s", change.Code, change.Body.String())
	}
	rotatedSession := change.Result().Cookies()[0]
	if rotatedSession.Value == passwordSession.Value {
		t.Fatal("password change did not rotate the current session cookie")
	}

	otherMe := httptest.NewRecorder()
	otherMeRequest := httptest.NewRequest(http.MethodGet, "http://speedtest.local/api/auth/me", nil)
	otherMeRequest.AddCookie(otherSession)
	handler.ServeHTTP(otherMe, otherMeRequest)
	if otherMe.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/auth/me in another session after password change = %d, want 401", otherMe.Code)
	}
	rotatedMe := httptest.NewRecorder()
	rotatedMeRequest := httptest.NewRequest(http.MethodGet, "http://speedtest.local/api/auth/me", nil)
	rotatedMeRequest.AddCookie(rotatedSession)
	handler.ServeHTTP(rotatedMe, rotatedMeRequest)
	if rotatedMe.Code != http.StatusOK {
		t.Fatalf("GET /api/auth/me with rotated password session = %d", rotatedMe.Code)
	}

	oldPasswordBootstrap, oldPasswordCSRF := bootstrapAgain()
	oldPasswordLogin := httptest.NewRecorder()
	oldPasswordRequest := httptest.NewRequest(http.MethodPost, "http://speedtest.local/api/auth/login", strings.NewReader(fmt.Sprintf(`{"email":%q,"password":%q}`, email, password)))
	oldPasswordRequest.AddCookie(&http.Cookie{Name: "speedtest_session", Value: oldPasswordBootstrap})
	oldPasswordRequest.Header.Set("Origin", "http://speedtest.local")
	oldPasswordRequest.Header.Set("X-CSRF-Token", oldPasswordCSRF)
	handler.ServeHTTP(oldPasswordLogin, oldPasswordRequest)
	if oldPasswordLogin.Code != http.StatusUnauthorized {
		t.Fatalf("POST /api/auth/login with previous password = %d, want 401", oldPasswordLogin.Code)
	}
	newPasswordBootstrap, newPasswordCSRF := bootstrapAgain()
	newPasswordLogin := httptest.NewRecorder()
	newPasswordRequest := httptest.NewRequest(http.MethodPost, "http://speedtest.local/api/auth/login", strings.NewReader(fmt.Sprintf(`{"email":%q,"password":%q}`, email, newPassword)))
	newPasswordRequest.AddCookie(&http.Cookie{Name: "speedtest_session", Value: newPasswordBootstrap})
	newPasswordRequest.Header.Set("Origin", "http://speedtest.local")
	newPasswordRequest.Header.Set("X-CSRF-Token", newPasswordCSRF)
	handler.ServeHTTP(newPasswordLogin, newPasswordRequest)
	if newPasswordLogin.Code != http.StatusOK {
		t.Fatalf("POST /api/auth/login with new password = %d: %s", newPasswordLogin.Code, newPasswordLogin.Body.String())
	}
}

func TestPostgresStoreRejectsDuplicateAndStaleStateIntegration(t *testing.T) {
	pool, ctx := integrationPool(t)
	if err := db.Migrate(ctx, pool, "up"); err != nil {
		t.Fatal(err)
	}
	store, err := auth.NewPostgresStore(pool)
	if err != nil {
		t.Fatal(err)
	}

	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	email := fmt.Sprintf("joost12-store-%s@example.test", hex.EncodeToString(suffix[:]))
	passwordHash, err := auth.HashPassword("a valid integration owner password")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SeedAdmin(ctx, email, passwordHash); err != nil {
		t.Fatal(err)
	}
	user, err := store.FindUserByEmail(ctx, email)
	if err != nil {
		t.Fatal(err)
	}

	probeTokens := []string{
		integrationTokenHash("duplicate-session"),
		integrationTokenHash("rotate-old"),
		integrationTokenHash("rotate-collision"),
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sessions WHERE user_id=$1`, user.ID)
		for _, tokenHash := range probeTokens {
			_, _ = pool.Exec(context.Background(), `DELETE FROM sessions WHERE token_hash=$1`, tokenHash)
		}
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, user.ID)
	})

	if _, err := store.FindUserByEmail(ctx, "missing-joost12@example.test"); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("missing email lookup error = %v, want ErrNotFound", err)
	}
	if _, err := store.FindUserByID(ctx, "00000000-0000-4000-8000-000000000000"); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("missing ID lookup error = %v, want ErrNotFound", err)
	}
	if _, err := store.GetSession(ctx, integrationTokenHash("missing-session"), time.Now()); !errors.Is(err, auth.ErrInvalidSession) {
		t.Fatalf("missing session lookup error = %v, want ErrInvalidSession", err)
	}
	if err := store.DeleteSession(ctx, integrationTokenHash("missing-delete")); !errors.Is(err, auth.ErrInvalidSession) {
		t.Fatalf("missing session delete error = %v, want ErrInvalidSession", err)
	}
	if err := store.RotateAnonymousSession(ctx, integrationTokenHash("missing-old"), user.ID, integrationTokenHash("new-session"), integrationTokenHash("new-csrf"), time.Now().Add(time.Hour)); !errors.Is(err, auth.ErrInvalidSession) {
		t.Fatalf("stale anonymous session rotation error = %v, want ErrInvalidSession", err)
	}

	expires := time.Now().UTC().Add(time.Hour)
	duplicateHash := probeTokens[0]
	if err := store.CreateSession(ctx, duplicateHash, nil, integrationTokenHash("duplicate-csrf"), expires); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(ctx, duplicateHash, nil, integrationTokenHash("duplicate-csrf-2"), expires); err == nil {
		t.Fatal("duplicate session token hash was accepted")
	}

	oldHash, collisionHash := probeTokens[1], probeTokens[2]
	if err := store.CreateSession(ctx, oldHash, nil, integrationTokenHash("old-csrf"), expires); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(ctx, collisionHash, nil, integrationTokenHash("collision-csrf"), expires); err != nil {
		t.Fatal(err)
	}
	if err := store.RotateAnonymousSession(ctx, oldHash, user.ID, collisionHash, integrationTokenHash("new-csrf"), expires); err == nil || errors.Is(err, auth.ErrInvalidSession) {
		t.Fatalf("colliding session rotation error = %v, want a database uniqueness error", err)
	}
	var oldRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE token_hash=$1`, oldHash).Scan(&oldRows); err != nil || oldRows != 1 {
		t.Fatal("failed rotation did not roll back deletion of the anonymous session")
	}

	if err := store.ChangePassword(ctx, user.ID, "wrong old hash", "new hash", oldHash, integrationTokenHash("rotated"), integrationTokenHash("csrf"), expires); !errors.Is(err, auth.ErrInvalidCurrentPass) {
		t.Fatalf("stale password hash update error = %v, want ErrInvalidCurrentPass", err)
	}
	newPasswordHash, err := auth.HashPassword("a newly changed integration password")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ChangePassword(ctx, user.ID, user.PasswordHash, newPasswordHash, integrationTokenHash("missing-owner-session"), integrationTokenHash("rotated-owner-session"), integrationTokenHash("csrf"), expires); !errors.Is(err, auth.ErrInvalidSession) {
		t.Fatalf("missing owner session password rotation error = %v, want ErrInvalidSession", err)
	}
	unchanged, err := store.FindUserByID(ctx, user.ID)
	if err != nil || unchanged.PasswordHash != user.PasswordHash {
		t.Fatal("failed password/session rotation did not roll back the password update")
	}
}

func integrationTokenHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
