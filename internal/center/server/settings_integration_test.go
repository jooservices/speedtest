//go:build integration

package server

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/auth"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/settings"
)

func TestSettingsHTTPFlowIntegration(t *testing.T) {
	pool, ctx, _ := openServerIntegrationPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	hash, err := auth.HashPassword("integration test owner password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,email,password_hash) VALUES ('00000000-0000-4000-8000-000000000901','owner@example.test',$1)`, hash); err != nil {
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
	cipher, err := settings.NewCipher(hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(Options{Auth: authService, Settings: settings.NewPostgresStore(pool, cipher, settings.Defaults("https://speedtest.example.com"))})
	session, csrf := loginIntegrationOwner(t, handler)
	call := func(method, path, body string, withSession bool, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://center.test"+path, strings.NewReader(body))
		switch {
		case withSession && method == http.MethodGet:
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
		case withSession:
			ownerHTTPWrite(req, session, csrf)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := call(http.MethodGet, "/api/settings", "", false, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("settings without a session = %d", rec.Code)
	}
	first := call(http.MethodGet, "/api/settings", "", true, nil)
	tag := first.Header().Get("ETag")
	if first.Code != http.StatusOK || tag != `"0"` || responseString(t, first.Body.Bytes(), "data", "theme") != "dark" || responseString(t, first.Body.Bytes(), "data", "centerUrl") != "https://speedtest.example.com" {
		t.Fatalf("defaults = %d etag %q %s", first.Code, tag, first.Body.String())
	}
	body := `{"theme":"light","language":"en","range":"30d","units":"Gbps","timezone":"Asia/Ho_Chi_Minh","hour12":true,"decimals":2,"retentionDays":90,"geoProvider":"geolite2","geoFallback":"ip-api","geoAutoUpdate":false,"engine":"librespeed","defaultThreads":8,"serverMode":"auto","maxConcurrent":2,"stagger":false,"quietStart":"23:00","quietEnd":"06:00","centerUrl":"https://speedtest.example.com","geoKey":"super-secret-geo-key","notify":{"email":{"on":true,"to":"ops@example.com","secret":"smtp-password"},"telegram":{"on":false,"to":""},"slack":{"on":false,"to":""},"webhook":{"on":true,"to":"","secret":"https://hooks.example.com/secret-path"}}}`
	if rec := call(http.MethodPut, "/api/settings", body, true, nil); rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("PUT without If-Match = %d", rec.Code)
	}
	if rec := call(http.MethodPut, "/api/settings", body, false, map[string]string{"If-Match": tag}); rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("PUT without credentials = %d", rec.Code)
	}
	saved := call(http.MethodPut, "/api/settings", body, true, map[string]string{"If-Match": tag})
	if saved.Code != http.StatusOK || saved.Header().Get("ETag") == tag {
		t.Fatalf("PUT = %d %s", saved.Code, saved.Body.String())
	}
	for _, leak := range []string{"super-secret-geo-key", "smtp-password", "secret-path"} {
		if strings.Contains(saved.Body.String(), leak) || strings.Contains(call(http.MethodGet, "/api/settings", "", true, nil).Body.String(), leak) {
			t.Fatalf("secret %q must never be returned", leak)
		}
	}
	var raw string
	if err := pool.QueryRow(ctx, `SELECT coalesce(string_agg(encode(secret_ciphertext,'escape'),''),'') FROM settings WHERE secret_ciphertext IS NOT NULL`).Scan(&raw); err != nil || raw == "" ||
		strings.Contains(raw, "super-secret-geo-key") || strings.Contains(raw, "smtp-password") || strings.Contains(raw, "secret-path") {
		t.Fatalf("secrets must be stored encrypted: %v", err)
	}
	after := call(http.MethodGet, "/api/settings", "", true, nil)
	if responseString(t, after.Body.Bytes(), "data", "theme") != "light" || !strings.Contains(after.Body.String(), `"geoKeyConfigured":true`) || !strings.Contains(after.Body.String(), `"retentionDays":90`) {
		t.Fatalf("saved settings = %s", after.Body.String())
	}
	if rec := call(http.MethodPut, "/api/settings", body, true, map[string]string{"If-Match": tag}); rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale PUT = %d", rec.Code)
	}
	invalid := strings.Replace(body, `"decimals":2`, `"decimals":9`, 1)
	rec := call(http.MethodPut, "/api/settings", invalid, true, map[string]string{"If-Match": saved.Header().Get("ETag")})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "decimals") {
		t.Fatalf("invalid PUT = %d %s", rec.Code, rec.Body.String())
	}
	// The shared values reach the rows the retention job and the agent configuration read.
	var retention, engine, quiet string
	if err := pool.QueryRow(ctx, `SELECT (SELECT value #>> '{}' FROM settings WHERE key='retentionDays'), (SELECT value #>> '{}' FROM settings WHERE key='engine'), (SELECT value #>> '{}' FROM settings WHERE key='quietStart')`).Scan(&retention, &engine, &quiet); err != nil ||
		retention != "90" || engine != "librespeed" || quiet != "23:00" {
		t.Fatalf("shared rows = %q %q %q (%v)", retention, engine, quiet, err)
	}

	key := call(http.MethodPost, "/api/settings/enrolment-key", "", true, nil)
	var issued struct {
		Data struct {
			EnrollToken string `json:"enrollToken"`
		} `json:"data"`
	}
	if key.Code != http.StatusCreated || json.Unmarshal(key.Body.Bytes(), &issued) != nil || len(issued.Data.EnrollToken) < 20 {
		t.Fatalf("rotate = %d %s", key.Code, key.Body.String())
	}
	if again := call(http.MethodGet, "/api/settings", "", true, nil); strings.Contains(again.Body.String(), issued.Data.EnrollToken) || !strings.Contains(again.Body.String(), `"enrollKeyConfigured":true`) {
		t.Fatalf("the enrolment key is shown once and then only flagged: %s", again.Body.String())
	}
	if rec := call(http.MethodPost, "/api/settings/enrolment-key", "", false, nil); rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("rotate without credentials = %d", rec.Code)
	}
}
