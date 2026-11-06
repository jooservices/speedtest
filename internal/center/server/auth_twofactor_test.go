package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // RFC 6238 test helper.
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/auth"
)

// Two-factor state for the in-memory auth store used by the HTTP tests.
func (s *authMemoryStore) tfState() map[string]*auth.TwoFactor {
	if s.tf == nil {
		s.tf = map[string]*auth.TwoFactor{}
	}
	return s.tf
}

func (s *authMemoryStore) GetTwoFactor(_ context.Context, id string) (auth.TwoFactor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.tfState()[id]; ok {
		return *r, nil
	}
	return auth.TwoFactor{}, auth.ErrNotFound
}

func (s *authMemoryStore) SaveTwoFactorSetup(_ context.Context, id string, enc []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tfState()[id] = &auth.TwoFactor{SecretEnc: enc}
	return nil
}

func (s *authMemoryStore) EnableTwoFactor(_ context.Context, id string, step int64, hashes []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.tfState()[id]
	if !ok || r.Enabled {
		return auth.ErrTwoFactorNotPending
	}
	r.Enabled, r.LastStep, r.RecoveryHashes = true, step, hashes
	return nil
}

func (s *authMemoryStore) DeleteTwoFactor(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tfState(), id)
	return nil
}

func (s *authMemoryStore) ConsumeTwoFactorStep(_ context.Context, id string, step int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.tfState()[id]
	if !ok || !r.Enabled || r.LastStep >= step {
		return false, nil
	}
	r.LastStep = step
	return true, nil
}

func (s *authMemoryStore) ConsumeRecoveryCode(_ context.Context, id, hash string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.tfState()[id]
	if !ok || !r.Enabled {
		return false, nil
	}
	for i, h := range r.RecoveryHashes {
		if h == hash {
			r.RecoveryHashes = append(r.RecoveryHashes[:i:i], r.RecoveryHashes[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

func totpFor(t *testing.T, secretB32 string, at time.Time) string {
	t.Helper()
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secretB32)
	if err != nil {
		t.Fatal(err)
	}
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(at.Unix()/30)) //nolint:gosec // test clock.
	mac := hmac.New(sha1.New, key)
	_, _ = mac.Write(counter[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	return fmt.Sprintf("%06d", (binary.BigEndian.Uint32(sum[offset:offset+4])&0x7fffffff)%1_000_000)
}

type tfClient struct {
	t       *testing.T
	handler http.Handler
	cookie  *http.Cookie
	csrf    string
}

func newTFHandler(t *testing.T, appKey bool) http.Handler {
	t.Helper()
	store := newAuthMemoryStore()
	hash, err := auth.HashPassword("the owner password")
	if err != nil {
		t.Fatal(err)
	}
	store.users["owner@example.com"] = auth.Credential{Identity: auth.Identity{ID: "user-id", Email: "owner@example.com"}, PasswordHash: hash}
	cfg := auth.Config{SessionTTL: time.Hour, LoginMaxAttempts: 100, LoginWindow: time.Minute}
	if appKey {
		cfg.AppKey = []byte(strings.Repeat("k", 32))
	}
	service, err := auth.NewService(store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return NewHandler(Options{Auth: service, TrustedProxyCIDRs: "127.0.0.0/8", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
}

func (c *tfClient) do(method, path, body string) *httptest.ResponseRecorder {
	c.t.Helper()
	request := httptest.NewRequest(method, "http://speedtest.local"+path, strings.NewReader(body))
	if c.cookie != nil {
		request.AddCookie(c.cookie)
	}
	if method != http.MethodGet {
		request.Header.Set("Origin", "http://speedtest.local")
		request.Header.Set("X-CSRF-Token", c.csrf)
	}
	response := httptest.NewRecorder()
	c.handler.ServeHTTP(response, request)
	if cookies := response.Result().Cookies(); len(cookies) > 0 && cookies[0].Value != "" {
		c.cookie = cookies[0]
	}
	var envelope authEnvelope
	if json.Unmarshal(response.Body.Bytes(), &envelope) == nil && envelope.Data.CSRFToken != "" {
		c.csrf = envelope.Data.CSRFToken
	}
	return response
}

func (c *tfClient) signIn(code string) *httptest.ResponseRecorder {
	c.t.Helper()
	c.cookie, c.csrf = nil, ""
	c.do(http.MethodGet, "/api/auth/me", "")
	body := `{"email":"owner@example.com","password":"the owner password"`
	if code != "" {
		body += `,"code":"` + code + `"`
	}
	return c.do(http.MethodPost, "/api/auth/login", body+"}")
}

func dataOf(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var envelope struct {
		Data map[string]any `json:"data"`
		Type string         `json:"type"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("%s: %v", response.Body.String(), err)
	}
	if envelope.Data == nil {
		return map[string]any{"type": envelope.Type}
	}
	return envelope.Data
}

func TestTwoFactorHTTPLifecycle(t *testing.T) {
	c := &tfClient{t: t, handler: newTFHandler(t, true)}
	if r := c.signIn(""); r.Code != http.StatusOK {
		t.Fatalf("sign-in = %d %s", r.Code, r.Body.String())
	}
	if status := dataOf(t, c.do(http.MethodGet, "/api/auth/2fa", "")); status["available"] != true || status["enabled"] != false {
		t.Fatalf("status = %v", status)
	}
	if r := c.do(http.MethodPost, "/api/auth/2fa/setup", `{"currentPassword":"nope"}`); r.Code != http.StatusUnauthorized {
		t.Fatalf("setup with a wrong password = %d", r.Code)
	}
	setup := c.do(http.MethodPost, "/api/auth/2fa/setup", `{"currentPassword":"the owner password"}`)
	data := dataOf(t, setup)
	secret, _ := data["secret"].(string)
	if setup.Code != http.StatusCreated || secret == "" || !strings.HasPrefix(fmt.Sprint(data["qr"]), "data:image/png;base64,") || !strings.HasPrefix(fmt.Sprint(data["uri"]), "otpauth://totp/") {
		t.Fatalf("setup = %d %s", setup.Code, setup.Body.String())
	}
	if setup.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("the secret response must not be cached")
	}
	if r := c.do(http.MethodPost, "/api/auth/2fa/enable", `{"code":"000000"}`); r.Code != http.StatusUnauthorized {
		t.Fatalf("enable with a wrong code = %d", r.Code)
	}
	enable := c.do(http.MethodPost, "/api/auth/2fa/enable", `{"code":"`+totpFor(t, secret, time.Now())+`"}`)
	codes, _ := dataOf(t, enable)["recoveryCodes"].([]any)
	if enable.Code != http.StatusOK || len(codes) != 10 {
		t.Fatalf("enable = %d %s", enable.Code, enable.Body.String())
	}
	if status := dataOf(t, c.do(http.MethodGet, "/api/auth/2fa", "")); status["enabled"] != true || status["recoveryRemaining"] != float64(10) {
		t.Fatalf("status after enable = %v", status)
	}
	if r := c.do(http.MethodPost, "/api/auth/2fa/setup", `{"currentPassword":"the owner password"}`); r.Code != http.StatusConflict {
		t.Fatalf("setup while enabled = %d", r.Code)
	}

	// Sign-in now needs the code.
	required := c.signIn("")
	if required.Code != http.StatusUnauthorized || dataOf(t, required)["type"] != "/problems/two-factor-required" || required.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("sign-in without a code = %d %s", required.Code, required.Body.String())
	}
	if r := c.signIn("123456"); r.Code != http.StatusUnauthorized || !strings.Contains(r.Body.String(), "Invalid authentication code") {
		t.Fatalf("sign-in with a wrong code = %d %s", r.Code, r.Body.String())
	}
	recovery := fmt.Sprint(codes[0])
	if r := c.signIn(recovery); r.Code != http.StatusOK {
		t.Fatalf("sign-in with a recovery code = %d %s", r.Code, r.Body.String())
	}
	if r := c.signIn(recovery); r.Code != http.StatusUnauthorized {
		t.Fatalf("a recovery code worked twice: %d", r.Code)
	}
	if r := c.signIn(strings.Repeat("9", 40)); r.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an over-long code = %d", r.Code)
	}

	// Disable: password plus a code.
	if r := c.signIn(fmt.Sprint(codes[1])); r.Code != http.StatusOK {
		t.Fatalf("sign-in = %d", r.Code)
	}
	if r := c.do(http.MethodPost, "/api/auth/2fa/disable", `{"currentPassword":"the owner password","code":"bogus"}`); r.Code != http.StatusUnauthorized {
		t.Fatalf("disable with a wrong code = %d", r.Code)
	}
	if r := c.do(http.MethodPost, "/api/auth/2fa/disable", `{"currentPassword":"the owner password","code":"`+fmt.Sprint(codes[2])+`"}`); r.Code != http.StatusNoContent {
		t.Fatalf("disable = %d %s", r.Code, r.Body.String())
	}
	if r := c.do(http.MethodPost, "/api/auth/2fa/disable", `{"currentPassword":"the owner password","code":"`+fmt.Sprint(codes[3])+`"}`); r.Code != http.StatusConflict {
		t.Fatalf("disable twice = %d", r.Code)
	}
	if r := c.signIn(""); r.Code != http.StatusOK {
		t.Fatalf("sign-in after disable = %d", r.Code)
	}
}

func TestTwoFactorHTTPGuards(t *testing.T) {
	c := &tfClient{t: t, handler: newTFHandler(t, true)}
	// Anonymous: no status, no actions.
	c.do(http.MethodGet, "/api/auth/me", "")
	if r := c.do(http.MethodGet, "/api/auth/2fa", ""); r.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d", r.Code)
	}
	if r := c.do(http.MethodPost, "/api/auth/2fa/setup", `{}`); r.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous setup = %d", r.Code)
	}
	noCookie := &tfClient{t: t, handler: c.handler}
	if r := noCookie.do(http.MethodGet, "/api/auth/2fa", ""); r.Code != http.StatusUnauthorized {
		t.Fatalf("no cookie = %d", r.Code)
	}
	c.signIn("")
	// Method checks, the same-origin rule, CSRF, body validation.
	if r := c.do(http.MethodPost, "/api/auth/2fa", `{}`); r.Code != http.StatusMethodNotAllowed || r.Header().Get("Allow") != "GET" {
		t.Fatalf("POST status = %d", r.Code)
	}
	if r := c.do(http.MethodGet, "/api/auth/2fa/setup", ""); r.Code != http.StatusMethodNotAllowed || r.Header().Get("Allow") != "POST" {
		t.Fatalf("GET setup = %d", r.Code)
	}
	badOrigin := httptest.NewRequest(http.MethodPost, "http://speedtest.local/api/auth/2fa/setup", strings.NewReader(`{}`))
	badOrigin.AddCookie(c.cookie)
	badOrigin.Header.Set("Origin", "http://evil.example")
	badOrigin.Header.Set("X-CSRF-Token", c.csrf)
	if r := httptest.NewRecorder(); func() int { c.handler.ServeHTTP(r, badOrigin); return r.Code }() != http.StatusForbidden {
		t.Fatal("a cross-origin write was accepted")
	}
	noCSRF := &tfClient{t: t, handler: c.handler, cookie: c.cookie}
	if r := noCSRF.do(http.MethodPost, "/api/auth/2fa/setup", `{}`); r.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF = %d", r.Code)
	}
	if r := c.do(http.MethodPost, "/api/auth/2fa/setup", `{"nope":1}`); r.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d", r.Code)
	}
	if r := c.do(http.MethodPost, "/api/auth/2fa/setup", `{"currentPassword":"`+strings.Repeat("x", 2000)+`"}`); r.Code != http.StatusUnprocessableEntity {
		t.Fatalf("long password = %d", r.Code)
	}
	if r := c.do(http.MethodPost, "/api/auth/2fa/enable", `{"code":"123456"}`); r.Code != http.StatusConflict {
		t.Fatalf("enable before setup = %d", r.Code)
	}
	if r := c.do(http.MethodPost, "/api/auth/2fa/other", `{}`); r.Code != http.StatusNotFound {
		t.Fatalf("unknown action = %d", r.Code)
	}
}

func TestTwoFactorHTTPWithoutAppKey(t *testing.T) {
	c := &tfClient{t: t, handler: newTFHandler(t, false)}
	c.signIn("")
	if status := dataOf(t, c.do(http.MethodGet, "/api/auth/2fa", "")); status["available"] != false {
		t.Fatalf("status = %v", status)
	}
	if r := c.do(http.MethodPost, "/api/auth/2fa/setup", `{"currentPassword":"the owner password"}`); r.Code != http.StatusServiceUnavailable {
		t.Fatalf("setup without APP_KEY = %d", r.Code)
	}
}

func TestTwoFactorFailureMapping(t *testing.T) {
	boom := fmt.Errorf("store down")
	cases := []struct {
		err  error
		want int
	}{
		{auth.ErrInvalidSession, 401}, {auth.ErrInvalidCurrentPass, 401}, {auth.ErrRateLimited, 429}, {boom, 500},
		{auth.ErrTwoFactorRequired, 401}, {auth.ErrInvalidTwoFactorCode, 401}, {auth.ErrTwoFactorUnavailable, 503},
		{auth.ErrTwoFactorEnabled, 409}, {auth.ErrTwoFactorNotPending, 409}, {auth.ErrTwoFactorNotEnabled, 409},
	}
	for _, c := range cases {
		service := nodeTestAuth{scriptedAuthService{
			status: func(context.Context, string) (auth.TwoFactorStatus, error) { return auth.TwoFactorStatus{}, c.err },
			begin: func(context.Context, string, string) (auth.TwoFactorSetup, error) {
				return auth.TwoFactorSetup{}, c.err
			},
			enable:  func(context.Context, string, string) ([]string, error) { return nil, c.err },
			disable: func(context.Context, string, string, string) error { return c.err },
		}}
		handler := newNodeHandler(Options{Auth: service})
		for _, req := range []struct{ method, path, body string }{
			{http.MethodGet, "/api/auth/2fa", ""},
			{http.MethodPost, "/api/auth/2fa/setup", `{"currentPassword":"x"}`},
			{http.MethodPost, "/api/auth/2fa/enable", `{"code":"123456"}`},
			{http.MethodPost, "/api/auth/2fa/disable", `{"currentPassword":"x","code":"123456"}`},
		} {
			request := httptest.NewRequest(req.method, "http://speedtest.local"+req.path, strings.NewReader(req.body))
			request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "owner-session"})
			request.Header.Set("Origin", "http://speedtest.local")
			request.Header.Set("X-CSRF-Token", "csrf-value")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != c.want {
				t.Errorf("%v on %s: status %d, want %d (%s)", c.err, req.path, response.Code, c.want, response.Body.String())
			}
		}
	}
}
