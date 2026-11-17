package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/auth"
)

type authMemoryStore struct {
	mu       sync.Mutex
	users    map[string]auth.Credential
	sessions map[string]auth.SessionRecord
	tf       map[string]*auth.TwoFactor
}

func newAuthMemoryStore() *authMemoryStore {
	return &authMemoryStore{users: make(map[string]auth.Credential), sessions: make(map[string]auth.SessionRecord)}
}

func (s *authMemoryStore) CreateSession(_ context.Context, hash string, userID *string, csrfHash string, expires time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record := auth.SessionRecord{CSRFTokenHash: csrfHash, ExpiresAt: expires}
	if userID != nil {
		for _, user := range s.users {
			if user.ID == *userID {
				identity := user.Identity
				record.Identity = &identity
			}
		}
	}
	s.sessions[hash] = record
	return nil
}

func (s *authMemoryStore) GetSession(_ context.Context, hash string, now time.Time) (auth.SessionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.sessions[hash]
	if !ok || !record.ExpiresAt.After(now) {
		return auth.SessionRecord{}, auth.ErrInvalidSession
	}
	return record, nil
}

func (s *authMemoryStore) FindUserByEmail(_ context.Context, email string) (auth.Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	user, ok := s.users[strings.ToLower(email)]
	if !ok {
		return auth.Credential{}, auth.ErrNotFound
	}
	return user, nil
}

func (s *authMemoryStore) FindUserByID(_ context.Context, id string) (auth.Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, user := range s.users {
		if user.ID == id {
			return user, nil
		}
	}
	return auth.Credential{}, auth.ErrNotFound
}

func (s *authMemoryStore) RotateAnonymousSession(_ context.Context, oldHash, userID, newHash, csrfHash string, expires time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.sessions[oldHash]
	if !ok || old.Identity != nil {
		return auth.ErrInvalidSession
	}
	delete(s.sessions, oldHash)
	for _, user := range s.users {
		if user.ID == userID {
			identity := user.Identity
			s.sessions[newHash] = auth.SessionRecord{Identity: &identity, CSRFTokenHash: csrfHash, ExpiresAt: expires}
			return nil
		}
	}
	return auth.ErrNotFound
}

func (s *authMemoryStore) DeleteSession(_ context.Context, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[hash]; !ok {
		return auth.ErrInvalidSession
	}
	delete(s.sessions, hash)
	return nil
}

func (s *authMemoryStore) ChangePassword(_ context.Context, userID, oldHash, newHash, oldSession, newSession, csrfHash string, expires time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var updated bool
	for key, user := range s.users {
		if user.ID == userID && user.PasswordHash == oldHash {
			user.PasswordHash = newHash
			s.users[key] = user
			updated = true
		}
	}
	if !updated {
		return auth.ErrInvalidCurrentPass
	}
	current, ok := s.sessions[oldSession]
	if !ok || current.Identity == nil || current.Identity.ID != userID {
		return auth.ErrInvalidSession
	}
	for key, record := range s.sessions {
		if record.Identity != nil && record.Identity.ID == userID && key != oldSession {
			delete(s.sessions, key)
		}
	}
	delete(s.sessions, oldSession)
	s.sessions[newSession] = auth.SessionRecord{Identity: current.Identity, CSRFTokenHash: csrfHash, ExpiresAt: expires}
	return nil
}

func (s *authMemoryStore) SeedAdmin(_ context.Context, email, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.ToLower(email)
	if _, exists := s.users[key]; !exists {
		s.users[key] = auth.Credential{Identity: auth.Identity{ID: "owner-id", Email: key}, PasswordHash: hash}
	}
	return nil
}

func newAuthHandler(t *testing.T, maxAttempts int) http.Handler {
	return newAuthHandlerWithHTTPS(t, maxAttempts, false)
}

func newAuthHandlerWithHTTPS(t *testing.T, maxAttempts int, requireHTTPS bool) http.Handler {
	t.Helper()
	store := newAuthMemoryStore()
	hash, err := auth.HashPassword("the owner password")
	if err != nil {
		t.Fatal(err)
	}
	store.users["owner@example.com"] = auth.Credential{
		Identity: auth.Identity{ID: "user-id", Email: "owner@example.com"}, PasswordHash: hash,
	}
	service, err := auth.NewService(store, auth.Config{SessionTTL: time.Hour, LoginMaxAttempts: maxAttempts, LoginWindow: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return NewHandler(Options{Auth: service, RequireHTTPS: requireHTTPS, TrustedProxyCIDRs: "127.0.0.0/8", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
}

type authEnvelope struct {
	Data struct {
		User      *authUserResponse `json:"user"`
		CSRFToken string            `json:"csrfToken"`
		ExpiresAt time.Time         `json:"expiresAt"`
	} `json:"data"`
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

func TestAuthHTTPBootstrapLoginLogoutAndRevocation(t *testing.T) {
	handler := newAuthHandler(t, 5)
	bootstrapResponse, anonymousCookie := bootstrapAuthSession(t, handler)
	loginResponse, authenticatedCookie := loginAuthSession(t, handler, anonymousCookie, bootstrapResponse.Data.CSRFToken)
	assertAuthenticatedAuthSession(t, handler, authenticatedCookie)
	logoutAndAssertAuthRevoked(t, handler, authenticatedCookie, loginResponse.Data.CSRFToken)
}

func bootstrapAuthSession(t *testing.T, handler http.Handler) (authEnvelope, *http.Cookie) {
	t.Helper()
	bootstrap := httptest.NewRecorder()
	handler.ServeHTTP(bootstrap, httptest.NewRequest(http.MethodGet, "http://speedtest.local/api/auth/me", nil))
	response := assertBootstrapResponse(t, bootstrap)
	cookie := assertBootstrapCookie(t, bootstrap)
	return response, cookie
}

func assertBootstrapResponse(t *testing.T, bootstrap *httptest.ResponseRecorder) authEnvelope {
	t.Helper()
	if bootstrap.Code != http.StatusOK {
		t.Fatalf("GET /me status = %d, want 200: %s", bootstrap.Code, bootstrap.Body.String())
	}
	var response authEnvelope
	if err := json.Unmarshal(bootstrap.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.User != nil || response.Data.CSRFToken == "" || response.Data.ExpiresAt.IsZero() {
		t.Fatalf("bootstrap response = %+v", response.Data)
	}
	if bootstrap.Header().Get("X-Request-ID") == "" || bootstrap.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("bootstrap response is missing request ID or no-store cache policy")
	}
	return response
}

func assertBootstrapCookie(t *testing.T, bootstrap *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	cookie := bootstrap.Result().Cookies()[0]
	if cookie.Name != sessionCookieName || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.Secure {
		t.Fatalf("HTTP session cookie attributes = %+v", cookie)
	}
	return cookie
}

func loginAuthSession(t *testing.T, handler http.Handler, anonymousCookie *http.Cookie, csrfToken string) (authEnvelope, *http.Cookie) {
	t.Helper()
	login := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://speedtest.local/api/auth/login", strings.NewReader(`{"email":"OWNER@example.com","password":"the owner password"}`))
	request.AddCookie(anonymousCookie)
	request.Header.Set("Origin", "http://speedtest.local")
	request.Header.Set("X-CSRF-Token", csrfToken)
	handler.ServeHTTP(login, request)
	if login.Code != http.StatusOK {
		t.Fatalf("POST /login status = %d: %s", login.Code, login.Body.String())
	}
	var response authEnvelope
	if err := json.Unmarshal(login.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.User == nil || response.Data.User.ID != "user-id" || response.Data.User.Email != "owner@example.com" {
		t.Fatalf("login user = %+v", response.Data.User)
	}
	cookie := login.Result().Cookies()[0]
	if cookie.Value == anonymousCookie.Value || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatal("login did not rotate the session cookie or preserve secure cookie attributes")
	}
	return response, cookie
}

func assertAuthenticatedAuthSession(t *testing.T, handler http.Handler, cookie *http.Cookie) {
	t.Helper()
	me := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://speedtest.local/api/auth/me", nil)
	request.AddCookie(cookie)
	handler.ServeHTTP(me, request)
	if me.Code != http.StatusOK {
		t.Fatalf("authenticated GET /me status = %d: %s", me.Code, me.Body.String())
	}
	var response authEnvelope
	if err := json.Unmarshal(me.Body.Bytes(), &response); err != nil || response.Data.User == nil || response.Data.User.ID != "user-id" {
		t.Fatalf("authenticated GET /me response = %+v, error %v", response.Data, err)
	}
}

func logoutAndAssertAuthRevoked(t *testing.T, handler http.Handler, cookie *http.Cookie, csrfToken string) {
	t.Helper()
	logout := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://speedtest.local/api/auth/logout", nil)
	request.AddCookie(cookie)
	request.Header.Set("Origin", "http://speedtest.local")
	request.Header.Set("X-CSRF-Token", csrfToken)
	handler.ServeHTTP(logout, request)
	if logout.Code != http.StatusNoContent || len(logout.Body.Bytes()) != 0 {
		t.Fatalf("POST /logout status/body = %d/%q", logout.Code, logout.Body.String())
	}
	if expired := logout.Result().Cookies()[0]; expired.MaxAge >= 0 || !expired.HttpOnly || expired.SameSite != http.SameSiteLaxMode {
		t.Fatalf("logout cookie was not expired safely: %+v", expired)
	}
	revoked := httptest.NewRecorder()
	revokedRequest := httptest.NewRequest(http.MethodGet, "http://speedtest.local/api/auth/me", nil)
	revokedRequest.AddCookie(cookie)
	handler.ServeHTTP(revoked, revokedRequest)
	if revoked.Code != http.StatusUnauthorized || revoked.Header().Get("WWW-Authenticate") != "Session" || len(revoked.Result().Cookies()) == 0 {
		t.Fatalf("GET /me with logged-out cookie = %d, want 401 and clear cookie", revoked.Code)
	}
}

func TestAuthLoginRecordsSecurityEventsWithoutCredentials(t *testing.T) {
	events := &fakeNotificationEvents{}
	identity := auth.Identity{ID: "user-123", Email: "owner@example.com"}
	created := testSessionView()
	created.Identity = &identity
	validService := scriptedAuthService{
		session: func(context.Context, string) (auth.SessionView, error) { return testSessionView(), nil },
		login:   func(context.Context, string, string, string, string) (auth.SessionView, error) { return created, nil },
	}
	invalidService := validService
	invalidService.login = func(context.Context, string, string, string, string) (auth.SessionView, error) {
		return auth.SessionView{}, auth.ErrInvalidCredentials
	}
	request := func(service AuthService) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://speedtest.local/api/auth/login", strings.NewReader(`{"email":"owner@example.com","password":"private password"}`))
		req.RemoteAddr = "203.0.113.90:4321"
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "current-token"})
		req.Header.Set("Origin", "http://speedtest.local")
		req.Header.Set("X-CSRF-Token", "test-csrf")
		(&serverAuth{service: service, events: events}).serve(response, req)
		return response
	}
	if response := request(validService); response.Code != http.StatusOK {
		t.Fatalf("successful login = %d %s", response.Code, response.Body.String())
	}
	if events.signIns != 1 || events.userKey != identity.ID || events.signInIP != "203.0.113.90" {
		t.Fatalf("new sign-in event = %d/%q/%q", events.signIns, events.userKey, events.signInIP)
	}
	response := request(invalidService)
	if response.Code != http.StatusUnauthorized || strings.Contains(response.Body.String(), "private password") {
		t.Fatalf("failed login response = %d %s", response.Code, response.Body.String())
	}
	if events.failures != 1 || events.failureIP != "203.0.113.90" || events.signIns != 1 {
		t.Fatalf("failed-login event = failures:%d ip:%q sign-ins:%d", events.failures, events.failureIP, events.signIns)
	}
}

func TestAuthHTTPRequiresSameOriginAndCSRF(t *testing.T) {
	handler := newAuthHandler(t, 5)
	bootstrap := httptest.NewRecorder()
	handler.ServeHTTP(bootstrap, httptest.NewRequest(http.MethodGet, "http://speedtest.local/api/auth/me", nil))
	cookie := bootstrap.Result().Cookies()[0]
	var session authEnvelope
	if err := json.Unmarshal(bootstrap.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		origin string
		csrf   string
		method string
		path   string
		want   int
	}{
		{name: "missing origin", csrf: session.Data.CSRFToken, method: http.MethodPost, path: "/api/auth/login", want: http.StatusForbidden},
		{name: "foreign origin", origin: "https://attacker.example", csrf: session.Data.CSRFToken, method: http.MethodPost, path: "/api/auth/login", want: http.StatusForbidden},
		{name: "missing csrf", origin: "http://speedtest.local", method: http.MethodPost, path: "/api/auth/logout", want: http.StatusForbidden},
		{name: "wrong csrf", origin: "http://speedtest.local", csrf: "wrong", method: http.MethodPost, path: "/api/auth/password", want: http.StatusForbidden},
		{name: "anonymous password change", origin: "http://speedtest.local", csrf: session.Data.CSRFToken, method: http.MethodPost, path: "/api/auth/password", want: http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			request := httptest.NewRequest(tc.method, "http://speedtest.local"+tc.path, nil)
			request.AddCookie(cookie)
			if tc.origin != "" {
				request.Header.Set("Origin", tc.origin)
			}
			if tc.csrf != "" {
				request.Header.Set("X-CSRF-Token", tc.csrf)
			}
			handler.ServeHTTP(response, request)
			if response.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", response.Code, tc.want, response.Body.String())
			}
			if response.Header().Get("Content-Type") != "application/problem+json" {
				t.Fatalf("Content-Type = %q, want application/problem+json", response.Header().Get("Content-Type"))
			}
		})
	}
}

func TestAuthHTTPGenericLoginFailuresAndRateLimit(t *testing.T) {
	handler := newAuthHandler(t, 2)
	bootstrap := httptest.NewRecorder()
	handler.ServeHTTP(bootstrap, httptest.NewRequest(http.MethodGet, "http://speedtest.local/api/auth/me", nil))
	cookie := bootstrap.Result().Cookies()[0]
	var session authEnvelope
	if err := json.Unmarshal(bootstrap.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	postLogin := func(email string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "http://speedtest.local/api/auth/login", strings.NewReader(`{"email":"`+email+`","password":"wrong password"}`))
		request.AddCookie(cookie)
		request.Header.Set("Origin", "http://speedtest.local")
		request.Header.Set("X-CSRF-Token", session.Data.CSRFToken)
		handler.ServeHTTP(response, request)
		return response
	}
	wrongPassword := postLogin("owner@example.com")
	unknownAccount := postLogin("missing@example.com")
	if wrongPassword.Code != http.StatusUnauthorized || unknownAccount.Code != http.StatusUnauthorized {
		t.Fatalf("failure statuses = %d and %d", wrongPassword.Code, unknownAccount.Code)
	}
	var wrong, unknown authEnvelope
	if err := json.Unmarshal(wrongPassword.Body.Bytes(), &wrong); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(unknownAccount.Body.Bytes(), &unknown); err != nil {
		t.Fatal(err)
	}
	if wrong.Type != unknown.Type || wrong.Title != unknown.Title || wrong.Status != unknown.Status || wrong.Detail != unknown.Detail {
		t.Fatalf("login failures reveal account existence: %+v versus %+v", wrong, unknown)
	}
	limited := postLogin("another@example.com")
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Retry-After") == "" {
		t.Fatalf("rate limited login = %d with Retry-After %q", limited.Code, limited.Header().Get("Retry-After"))
	}
}

func TestAuthHTTPHandlesMalformedAndUnknownRequests(t *testing.T) {
	handler := newAuthHandler(t, 5)
	for _, tc := range []struct {
		method string
		path   string
		want   int
		allow  string
	}{
		{method: http.MethodPatch, path: "/api/auth/me", want: http.StatusMethodNotAllowed, allow: http.MethodGet},
		{method: http.MethodGet, path: "/api/auth/unknown", want: http.StatusNotFound},
		{method: http.MethodGet, path: "/api/nodes", want: http.StatusMethodNotAllowed, allow: http.MethodPost},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(tc.method, "http://speedtest.local"+tc.path, nil))
		if response.Code != tc.want || tc.allow != "" && response.Header().Get("Allow") != tc.allow {
			t.Errorf("%s %s = %d Allow=%q, want %d Allow=%q", tc.method, tc.path, response.Code, response.Header().Get("Allow"), tc.want, tc.allow)
		}
	}

	bootstrap := httptest.NewRecorder()
	handler.ServeHTTP(bootstrap, httptest.NewRequest(http.MethodGet, "http://speedtest.local/api/auth/me", nil))
	cookie := bootstrap.Result().Cookies()[0]
	var session authEnvelope
	if err := json.Unmarshal(bootstrap.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"{", `{"email":"owner@example.com","password":"valid password","unknown":true}`} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "http://speedtest.local/api/auth/login", strings.NewReader(body))
		request.AddCookie(cookie)
		request.Header.Set("Origin", "http://speedtest.local")
		request.Header.Set("X-CSRF-Token", session.Data.CSRFToken)
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Errorf("malformed login body status = %d, want 400", response.Code)
		}
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://speedtest.local/api/auth/me", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "missing-session"})
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("invalid session cookie status = %d, want 401", response.Code)
	}
}

func TestAuthHTTPSCookieAndOriginCanonicalization(t *testing.T) {
	handler := newAuthHandlerWithHTTPS(t, 5, false)
	request := httptest.NewRequest(http.MethodGet, "http://speedtest.local/api/auth/me", nil)
	request.RemoteAddr = "127.0.0.1:8080"
	request.Header.Set("X-Forwarded-Proto", "https")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if cookies := response.Result().Cookies(); len(cookies) != 1 || !cookies[0].Secure {
		t.Fatal("HTTPS request did not set Secure on the session cookie")
	}

	for _, tc := range []struct {
		origin string
		host   string
		xfp    string
		want   bool
	}{
		{origin: "http://example.test", host: "example.test:80", want: true},
		{origin: "http://example.test:8080", host: "example.test:8080", want: true},
		{origin: "http://:80", host: "example.test", want: false},
		{origin: "http://example.test", host: ":80", want: false},
		{origin: "https://example.test", host: "example.test", xfp: "https", want: true},
		{origin: "https://example.test", host: "example.test", want: false},
		{origin: "http://example.test", host: "attacker.test", want: false},
		{origin: "null", host: "example.test", want: false},
		{origin: "http://user@example.test", host: "example.test", want: false},
		{origin: "http://example.test/path", host: "example.test", want: false},
	} {
		r := httptest.NewRequest(http.MethodPost, "http://example.test/", nil)
		r.RemoteAddr = "127.0.0.1:8080"
		r.Host = tc.host
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("X-Forwarded-Proto", tc.xfp)
		if got := (&serverAuth{trustedProxyCIDRs: "127.0.0.0/8"}).sameOrigin(r); got != tc.want {
			t.Errorf("sameOrigin(%q, %q, %q) = %v, want %v", tc.origin, tc.host, tc.xfp, got, tc.want)
		}
	}
}

func TestAuthRequiresHTTPSOutsideLocalAndTest(t *testing.T) {
	handler := newAuthHandlerWithHTTPS(t, 5, true)
	plain := httptest.NewRecorder()
	handler.ServeHTTP(plain, httptest.NewRequest(http.MethodGet, "http://speedtest.local/api/auth/me", nil))
	if plain.Code != http.StatusForbidden || len(plain.Result().Cookies()) != 0 {
		t.Fatalf("plain HTTP auth request = %d, want 403 without a session", plain.Code)
	}
	spoofedRequest := httptest.NewRequest(http.MethodGet, "http://speedtest.local/api/auth/me", nil)
	spoofedRequest.Header.Set("X-Forwarded-Proto", "https")
	spoofed := httptest.NewRecorder()
	handler.ServeHTTP(spoofed, spoofedRequest)
	if spoofed.Code != http.StatusForbidden || len(spoofed.Result().Cookies()) != 0 {
		t.Fatalf("untrusted forwarded HTTPS request = %d, want 403 without a session", spoofed.Code)
	}
	secureRequest := httptest.NewRequest(http.MethodGet, "http://speedtest.local/api/auth/me", nil)
	secureRequest.RemoteAddr = "127.0.0.1:8080"
	secureRequest.Header.Set("X-Forwarded-Proto", "https")
	secure := httptest.NewRecorder()
	handler.ServeHTTP(secure, secureRequest)
	if secure.Code != http.StatusOK || len(secure.Result().Cookies()) != 1 || !secure.Result().Cookies()[0].Secure {
		t.Fatalf("forwarded HTTPS auth request = %d, Secure cookie missing", secure.Code)
	}
}

func TestRemoteIPTrustsForwardedChainOnlyFromConfiguredProxies(t *testing.T) {
	trusted := "127.0.0.0/8,10.0.0.0/8"
	if got := remoteIP("127.0.0.1:8080", "203.0.113.9, 10.0.0.4", trusted); got != "203.0.113.9" {
		t.Fatalf("trusted forwarded client IP = %q, want 203.0.113.9", got)
	}
	if got := remoteIP("198.51.100.4:8080", "203.0.113.9", trusted); got != "198.51.100.4" {
		t.Fatalf("untrusted forwarded client IP = %q, want socket peer", got)
	}
	if got := remoteIP("127.0.0.1:8080", "not-an-ip", trusted); got != "127.0.0.1" {
		t.Fatalf("malformed forwarded client IP = %q, want socket peer", got)
	}
	if got := remoteIP("127.0.0.1:8080", "203.0.113.9", ""); got != "127.0.0.1" {
		t.Fatalf("unconfigured proxy forwarded client IP = %q, want socket peer", got)
	}
	if got := remoteIP("not-a-socket-address", "203.0.113.9", trusted); got != "not-a-socket-address" {
		t.Fatalf("unparseable remote peer = %q", got)
	}
	if got := remoteIP("127.0.0.1:8080", "", trusted); got != "127.0.0.1" {
		t.Fatalf("trusted peer without forwarded chain = %q", got)
	}
	if got := remoteIP("127.0.0.1:8080", "10.0.0.4", trusted); got != "127.0.0.1" {
		t.Fatalf("all-trusted forwarded chain = %q, want socket peer", got)
	}
}

func TestAuthErrorMappingKeepsStorageErrorsSafe(t *testing.T) {
	if errors.Is(errors.New("database details"), auth.ErrInvalidSession) {
		t.Fatal("unexpected error match")
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://speedtest.local/api/auth/me", nil)
	writeAuthInternalError(response, request)
	if strings.Contains(response.Body.String(), "database details") || response.Code != http.StatusInternalServerError {
		t.Fatal("internal auth error response was not generic")
	}
}

type scriptedAuthService struct {
	bootstrap func(context.Context) (auth.SessionView, error)
	session   func(context.Context, string) (auth.SessionView, error)
	login     func(context.Context, string, string, string, string) (auth.SessionView, error)
	logout    func(context.Context, string) error
	password  func(context.Context, string, string, string) (auth.SessionView, error)
	// loginCode sees the second-factor code; login (without it) is used when unset.
	loginCode func(context.Context, string, string, string, string, string) (auth.SessionView, error)
	status    func(context.Context, string) (auth.TwoFactorStatus, error)
	begin     func(context.Context, string, string) (auth.TwoFactorSetup, error)
	enable    func(context.Context, string, string) ([]string, error)
	disable   func(context.Context, string, string, string) error
}

func (s scriptedAuthService) Bootstrap(ctx context.Context) (auth.SessionView, error) {
	if s.bootstrap != nil {
		return s.bootstrap(ctx)
	}
	return testSessionView(), nil
}

func (s scriptedAuthService) Session(ctx context.Context, token string) (auth.SessionView, error) {
	if s.session != nil {
		return s.session(ctx, token)
	}
	return testSessionView(), nil
}

func (s scriptedAuthService) LoginWithCode(ctx context.Context, token, email, password, code, ip string) (auth.SessionView, error) {
	if s.loginCode != nil {
		return s.loginCode(ctx, token, email, password, code, ip)
	}
	if s.login != nil {
		return s.login(ctx, token, email, password, ip)
	}
	return testSessionView(), nil
}

func (s scriptedAuthService) TwoFactorStatus(ctx context.Context, token string) (auth.TwoFactorStatus, error) {
	if s.status != nil {
		return s.status(ctx, token)
	}
	return auth.TwoFactorStatus{Available: true}, nil
}

func (s scriptedAuthService) BeginTwoFactor(ctx context.Context, token, password string) (auth.TwoFactorSetup, error) {
	if s.begin != nil {
		return s.begin(ctx, token, password)
	}
	return auth.TwoFactorSetup{Secret: "SECRET", URI: "otpauth://totp/x", QR: "data:image/png;base64,AA"}, nil
}

func (s scriptedAuthService) EnableTwoFactor(ctx context.Context, token, code string) ([]string, error) {
	if s.enable != nil {
		return s.enable(ctx, token, code)
	}
	return []string{"aaaaa-bbbbb"}, nil
}

func (s scriptedAuthService) DisableTwoFactor(ctx context.Context, token, password, code string) error {
	if s.disable != nil {
		return s.disable(ctx, token, password, code)
	}
	return nil
}

func (s scriptedAuthService) Logout(ctx context.Context, token string) error {
	if s.logout != nil {
		return s.logout(ctx, token)
	}
	return nil
}

func (s scriptedAuthService) ChangePassword(ctx context.Context, token, current, next string) (auth.SessionView, error) {
	if s.password != nil {
		return s.password(ctx, token, current, next)
	}
	return testSessionView(), nil
}

func testSessionView() auth.SessionView {
	return auth.SessionView{Token: "rotated-token", CSRFToken: "test-csrf", ExpiresAt: time.Now().Add(time.Hour)}
}

type rateLimitWithoutRetry struct{}

func (rateLimitWithoutRetry) Error() string { return "rate limited" }
func (rateLimitWithoutRetry) Unwrap() error { return auth.ErrRateLimited }

type rateLimitWithRetry struct{ wait time.Duration }

func (e rateLimitWithRetry) Error() string             { return "rate limited" }
func (e rateLimitWithRetry) Unwrap() error             { return auth.ErrRateLimited }
func (e rateLimitWithRetry) RetryAfter() time.Duration { return e.wait }

func serveAuthTestRequest(service AuthService, method, path, body string, withCookie bool) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(method, "http://speedtest.local"+path, strings.NewReader(body))
	if withCookie {
		request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "current-token"})
	}
	request.Header.Set("Origin", "http://speedtest.local")
	request.Header.Set("X-CSRF-Token", "test-csrf")
	(&serverAuth{service: service}).serve(response, request)
	return response
}

func TestAuthRoutesMapServiceErrors(t *testing.T) {
	internal := errors.New("private database error")
	identity := &auth.Identity{ID: "owner-id", Email: "owner@example.com"}
	identifiedSession := testSessionView()
	identifiedSession.Identity = identity

	tests := []struct {
		name    string
		method  string
		path    string
		body    string
		service scriptedAuthService
		cookie  bool
		want    int
		retry   bool
	}{
		{name: "bootstrap internal error", method: http.MethodGet, path: "/api/auth/me", service: scriptedAuthService{bootstrap: func(context.Context) (auth.SessionView, error) { return auth.SessionView{}, internal }}, want: http.StatusInternalServerError},
		{name: "session invalid", method: http.MethodGet, path: "/api/auth/me", service: scriptedAuthService{session: func(context.Context, string) (auth.SessionView, error) {
			return auth.SessionView{}, auth.ErrInvalidSession
		}}, cookie: true, want: http.StatusUnauthorized},
		{name: "session storage error", method: http.MethodGet, path: "/api/auth/me", service: scriptedAuthService{session: func(context.Context, string) (auth.SessionView, error) { return auth.SessionView{}, internal }}, cookie: true, want: http.StatusInternalServerError},
		{name: "authenticated login conflict", method: http.MethodPost, path: "/api/auth/login", body: `{"email":"owner@example.com","password":"valid password"}`, service: scriptedAuthService{session: func(context.Context, string) (auth.SessionView, error) { return identifiedSession, nil }}, cookie: true, want: http.StatusConflict},
		{name: "invalid session during login", method: http.MethodPost, path: "/api/auth/login", body: `{"email":"owner@example.com","password":"valid password"}`, service: scriptedAuthService{login: func(context.Context, string, string, string, string) (auth.SessionView, error) {
			return auth.SessionView{}, auth.ErrInvalidSession
		}}, cookie: true, want: http.StatusUnauthorized},
		{name: "invalid credentials", method: http.MethodPost, path: "/api/auth/login", body: `{"email":"owner@example.com","password":"valid password"}`, service: scriptedAuthService{login: func(context.Context, string, string, string, string) (auth.SessionView, error) {
			return auth.SessionView{}, auth.ErrInvalidCredentials
		}}, cookie: true, want: http.StatusUnauthorized},
		{name: "rate limit retry header", method: http.MethodPost, path: "/api/auth/login", body: `{"email":"owner@example.com","password":"valid password"}`, service: scriptedAuthService{login: func(context.Context, string, string, string, string) (auth.SessionView, error) {
			return auth.SessionView{}, rateLimitWithoutRetry{}
		}}, cookie: true, want: http.StatusTooManyRequests, retry: true},
		{name: "session conflict during login", method: http.MethodPost, path: "/api/auth/login", body: `{"email":"owner@example.com","password":"valid password"}`, service: scriptedAuthService{login: func(context.Context, string, string, string, string) (auth.SessionView, error) {
			return auth.SessionView{}, auth.ErrSessionConflict
		}}, cookie: true, want: http.StatusConflict},
		{name: "login storage error", method: http.MethodPost, path: "/api/auth/login", body: `{"email":"owner@example.com","password":"valid password"}`, service: scriptedAuthService{login: func(context.Context, string, string, string, string) (auth.SessionView, error) {
			return auth.SessionView{}, internal
		}}, cookie: true, want: http.StatusInternalServerError},
		{name: "invalid session during logout", method: http.MethodPost, path: "/api/auth/logout", service: scriptedAuthService{logout: func(context.Context, string) error { return auth.ErrInvalidSession }}, cookie: true, want: http.StatusUnauthorized},
		{name: "logout storage error", method: http.MethodPost, path: "/api/auth/logout", service: scriptedAuthService{logout: func(context.Context, string) error { return internal }}, cookie: true, want: http.StatusInternalServerError},
		{name: "anonymous password change", method: http.MethodPost, path: "/api/auth/password", body: `{"currentPassword":"current password","newPassword":"new valid password"}`, cookie: true, want: http.StatusUnauthorized},
		{name: "invalid session during password change", method: http.MethodPost, path: "/api/auth/password", body: `{"currentPassword":"current password","newPassword":"new valid password"}`, service: scriptedAuthService{session: func(context.Context, string) (auth.SessionView, error) { return identifiedSession, nil }, password: func(context.Context, string, string, string) (auth.SessionView, error) {
			return auth.SessionView{}, auth.ErrInvalidSession
		}}, cookie: true, want: http.StatusUnauthorized},
		{name: "wrong current password", method: http.MethodPost, path: "/api/auth/password", body: `{"currentPassword":"current password","newPassword":"new valid password"}`, service: scriptedAuthService{session: func(context.Context, string) (auth.SessionView, error) { return identifiedSession, nil }, password: func(context.Context, string, string, string) (auth.SessionView, error) {
			return auth.SessionView{}, auth.ErrInvalidCurrentPass
		}}, cookie: true, want: http.StatusUnauthorized},
		{name: "password policy", method: http.MethodPost, path: "/api/auth/password", body: `{"currentPassword":"current password","newPassword":"new valid password"}`, service: scriptedAuthService{session: func(context.Context, string) (auth.SessionView, error) { return identifiedSession, nil }, password: func(context.Context, string, string, string) (auth.SessionView, error) {
			return auth.SessionView{}, auth.ErrPasswordPolicy
		}}, cookie: true, want: http.StatusUnprocessableEntity},
		{name: "password storage error", method: http.MethodPost, path: "/api/auth/password", body: `{"currentPassword":"current password","newPassword":"new valid password"}`, service: scriptedAuthService{session: func(context.Context, string) (auth.SessionView, error) { return identifiedSession, nil }, password: func(context.Context, string, string, string) (auth.SessionView, error) {
			return auth.SessionView{}, internal
		}}, cookie: true, want: http.StatusInternalServerError},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := test.service
			if service.session == nil && test.cookie {
				service.session = func(context.Context, string) (auth.SessionView, error) { return testSessionView(), nil }
			}
			response := serveAuthTestRequest(service, test.method, test.path, test.body, test.cookie)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.want, response.Body.String())
			}
			if test.retry && response.Header().Get("Retry-After") != "1" {
				t.Fatalf("Retry-After = %q, want 1", response.Header().Get("Retry-After"))
			}
			if test.want == http.StatusInternalServerError && strings.Contains(response.Body.String(), "private database error") {
				t.Fatal("response exposed an internal error")
			}
		})
	}
}

func TestAuthRoutesValidateBodiesAndCookies(t *testing.T) {
	for _, body := range []string{
		`{"email":"bad address","password":"valid password"}`,
		`{"email":"owner@example.com","password":""}`,
		`{"email":"owner@example.com","password":"` + strings.Repeat("x", 1025) + `"}`,
	} {
		response := serveAuthTestRequest(scriptedAuthService{}, http.MethodPost, "/api/auth/login", body, true)
		if response.Code != http.StatusUnprocessableEntity {
			t.Errorf("invalid login body status = %d, want 422", response.Code)
		}
	}

	for _, body := range []string{
		`{"currentPassword":"","newPassword":"a valid new password"}`,
		`{"currentPassword":"current password","newPassword":""}`,
	} {
		service := scriptedAuthService{session: func(context.Context, string) (auth.SessionView, error) {
			view := testSessionView()
			view.Identity = &auth.Identity{ID: "owner-id", Email: "owner@example.com"}
			return view, nil
		}}
		response := serveAuthTestRequest(service, http.MethodPost, "/api/auth/password", body, true)
		if response.Code != http.StatusUnprocessableEntity {
			t.Errorf("invalid password body status = %d, want 422", response.Code)
		}
	}
	passwordService := scriptedAuthService{session: func(context.Context, string) (auth.SessionView, error) {
		view := testSessionView()
		view.Identity = &auth.Identity{ID: "owner-id", Email: "owner@example.com"}
		return view, nil
	}}
	if response := serveAuthTestRequest(passwordService, http.MethodPost, "/api/auth/password", `{`, true); response.Code != http.StatusBadRequest {
		t.Fatalf("malformed password body status = %d, want 400", response.Code)
	}

	largeBody := `{"email":"owner@example.com","password":"` + strings.Repeat("x", maxAuthBodyBytes) + `"}`
	if response := serveAuthTestRequest(scriptedAuthService{}, http.MethodPost, "/api/auth/login", largeBody, true); response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize login body status = %d, want 413", response.Code)
	}
	for _, body := range []string{`{"email":"owner@example.com","password":"valid password"} {}`, `{"email":"owner@example.com","password":"valid password"} trailing`} {
		if response := serveAuthTestRequest(scriptedAuthService{}, http.MethodPost, "/api/auth/login", body, true); response.Code != http.StatusBadRequest {
			t.Errorf("trailing JSON status = %d, want 400", response.Code)
		}
	}

	for _, header := range []string{"speedtest_session=", "speedtest_session=first; speedtest_session=second"} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "http://speedtest.local/api/auth/me", nil)
		request.Header.Set("Cookie", header)
		(&serverAuth{service: scriptedAuthService{}}).serve(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Errorf("invalid cookie header %q status = %d, want 401", header, response.Code)
		}
	}
}

func TestAuthOriginCookieAndReadinessEdges(t *testing.T) {
	for _, origins := range [][]string{
		nil,
		{"http://speedtest.local", "http://speedtest.local"},
		{"http://speedtest.local?x=1"},
		{"http://speedtest.local#fragment"},
	} {
		request := httptest.NewRequest(http.MethodPost, "http://speedtest.local/api/auth/logout", nil)
		for _, origin := range origins {
			request.Header.Add("Origin", origin)
		}
		if sameOrigin(request) {
			t.Errorf("sameOrigin accepted invalid Origin values %v", origins)
		}
	}
	for _, host := range []string{"", ":80", "example.test:", "example.test:abc", "example.test:99999", "example.test/path", "user@example.test"} {
		if _, ok := canonicalAuthority(host, "http"); ok {
			t.Errorf("canonicalAuthority(%q) unexpectedly succeeded", host)
		}
	}

	for _, header := range []string{"", "speedtest_session=first; speedtest_session=second"} {
		request := httptest.NewRequest(http.MethodGet, "http://speedtest.local/", nil)
		if header != "" {
			request.Header.Set("Cookie", header)
		}
		_, present, invalid := readSessionCookie(request)
		if header == "" && (present || invalid) || header != "" && (!present || !invalid) {
			t.Errorf("readSessionCookie(%q) = present %v, invalid %v", header, present, invalid)
		}
	}

	request := httptest.NewRequest(http.MethodGet, "http://speedtest.local/api/auth/me", nil)
	(&serverAuth{}).serve(httptest.NewRecorder(), request)
	response := httptest.NewRecorder()
	(&serverAuth{service: scriptedAuthService{}, requireHTTPS: true}).serve(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("non-HTTPS request status = %d, want 403", response.Code)
	}
}

func TestAuthMethodGuardsAndSessionCSRFFailures(t *testing.T) {
	for _, path := range []string{"/api/auth/login", "/api/auth/logout", "/api/auth/password"} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "http://speedtest.local"+path, nil)
		(&serverAuth{service: scriptedAuthService{}}).serve(response, request)
		if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodPost {
			t.Errorf("GET %s status/Allow = %d/%q", path, response.Code, response.Header().Get("Allow"))
		}
	}
	if response := serveAuthTestRequest(scriptedAuthService{}, http.MethodPost, "/api/auth/other", "", true); response.Code != http.StatusNotFound {
		t.Fatalf("unknown auth route status = %d, want 404", response.Code)
	}
	if response := serveAuthTestRequest(scriptedAuthService{}, http.MethodPost, "/api/auth/login", `{"email":"owner@example.com","password":"valid password"}`, false); response.Code != http.StatusUnauthorized {
		t.Fatalf("login without a session cookie status = %d, want 401", response.Code)
	}
	for _, path := range []string{"/api/auth/login", "/api/auth/logout", "/api/auth/password"} {
		request := httptest.NewRequest(http.MethodPost, "http://speedtest.local"+path, strings.NewReader(`{"email":"owner@example.com","password":"valid password","currentPassword":"current password","newPassword":"a valid new password"}`))
		request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "current-token"})
		request.Header.Set("Origin", "https://attacker.example")
		request.Header.Set("X-CSRF-Token", "test-csrf")
		response := httptest.NewRecorder()
		(&serverAuth{service: scriptedAuthService{}}).serve(response, request)
		if response.Code != http.StatusForbidden {
			t.Errorf("foreign-origin %s status = %d, want 403", path, response.Code)
		}
	}

	tests := []struct {
		name    string
		cookie  string
		session func(context.Context, string) (auth.SessionView, error)
		csrf    string
		want    int
	}{
		{name: "missing cookie", want: http.StatusUnauthorized},
		{name: "duplicate cookie", cookie: "speedtest_session=one; speedtest_session=two", want: http.StatusUnauthorized},
		{name: "invalid session", cookie: "speedtest_session=one", session: func(context.Context, string) (auth.SessionView, error) {
			return auth.SessionView{}, auth.ErrInvalidSession
		}, csrf: "test-csrf", want: http.StatusUnauthorized},
		{name: "session storage error", cookie: "speedtest_session=one", session: func(context.Context, string) (auth.SessionView, error) {
			return auth.SessionView{}, errors.New("storage error")
		}, csrf: "test-csrf", want: http.StatusInternalServerError},
		{name: "missing csrf", cookie: "speedtest_session=one", csrf: "", want: http.StatusForbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://speedtest.local/api/auth/logout", nil)
			request.Header.Set("Origin", "http://speedtest.local")
			request.Header.Set("X-CSRF-Token", test.csrf)
			if test.cookie != "" {
				request.Header.Set("Cookie", test.cookie)
			}
			service := scriptedAuthService{session: test.session}
			response := httptest.NewRecorder()
			(&serverAuth{service: service}).serve(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.want, response.Body.String())
			}
		})
	}
}

func TestAuthSuccessfulPasswordRotationAndRetryDuration(t *testing.T) {
	identity := &auth.Identity{ID: "owner-id", Email: "owner@example.com"}
	service := scriptedAuthService{
		session: func(context.Context, string) (auth.SessionView, error) {
			view := testSessionView()
			view.Identity = identity
			return view, nil
		},
		password: func(context.Context, string, string, string) (auth.SessionView, error) {
			view := testSessionView()
			view.Identity = identity
			return view, nil
		},
		login: func(context.Context, string, string, string, string) (auth.SessionView, error) {
			return auth.SessionView{}, rateLimitWithRetry{wait: 3500 * time.Millisecond}
		},
	}
	password := serveAuthTestRequest(service, http.MethodPost, "/api/auth/password", `{"currentPassword":"current password","newPassword":"a new valid password"}`, true)
	if password.Code != http.StatusNoContent || len(password.Result().Cookies()) != 1 || password.Result().Cookies()[0].Value != "rotated-token" || password.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("successful password rotation = %d with headers %v", password.Code, password.Header())
	}

	loginService := scriptedAuthService{login: service.login}
	login := serveAuthTestRequest(loginService, http.MethodPost, "/api/auth/login", `{"email":"owner@example.com","password":"valid password"}`, true)
	if login.Code != http.StatusTooManyRequests || login.Header().Get("Retry-After") != "4" {
		t.Fatalf("rate-limit response = %d Retry-After=%q, want 429/4", login.Code, login.Header().Get("Retry-After"))
	}
}

func TestAuthCookieHTTPSAndExpiryBounds(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://speedtest.local/", nil)
	request.TLS = &tls.ConnectionState{}
	response := httptest.NewRecorder()
	setSessionCookie(response, request, auth.SessionView{Token: "token", ExpiresAt: time.Now().Add(-time.Hour)})
	if cookies := response.Result().Cookies(); len(cookies) != 1 || !cookies[0].Secure || cookies[0].MaxAge != 1 {
		t.Fatalf("expired TLS cookie attributes = %+v", cookies)
	}
	if !requestIsHTTPS(request, "") {
		t.Fatal("TLS request was not detected as HTTPS")
	}
}

// The shared envelope of the contract requires a meta object on every success; the browser client refuses a response
// without it, so a missing meta makes the whole dashboard show "invalid response".
func TestAuthSuccessEnvelopeCarriesAMetaObject(t *testing.T) {
	handler := newAuthHandler(t, 5)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://center.test/api/auth/me", nil))
	var body map[string]json.RawMessage
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
		t.Fatalf("me = %d %s", rec.Code, rec.Body.String())
	}
	if string(body["meta"]) != "{}" || string(body["errors"]) != "null" || string(body["success"]) != "true" {
		t.Fatalf("envelope = %s", rec.Body.String())
	}
}
