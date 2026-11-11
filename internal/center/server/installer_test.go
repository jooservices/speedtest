package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/apitoken"
	"github.com/jooservices/speedtest/internal/center/auth"
	"github.com/jooservices/speedtest/internal/center/installer"
)

const (
	installerTestNodeID = "00000000-0000-4000-8000-000000000043"
	installerTestJobID  = "00000000-0000-4000-8000-000000000044"
	installerTestEvent  = "00000000-0000-4000-8000-000000000045"
)

type fakeInstallerService struct {
	serviceErr error
	probeInput installer.ProbeInput
	probeOwner string
	install    installer.InstallInput
	installKey string
	installFor string
	uninstalls int
}

func (f *fakeInstallerService) Probe(_ context.Context, principal, key string, input installer.ProbeInput) (installer.SSHProbe, error) {
	if f.serviceErr != nil {
		return installer.SSHProbe{}, f.serviceErr
	}
	f.probeInput, f.probeOwner = input, principal
	return installer.SSHProbe{ID: installerTestEvent, Fingerprint: "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", ExpiresAt: time.Now().Add(time.Minute)}, nil
}

func (f *fakeInstallerService) Start(_ context.Context, principal, key, nodeID string, input installer.InstallInput) (installer.Installation, error) {
	if f.serviceErr != nil {
		return installer.Installation{}, f.serviceErr
	}
	f.install, f.installKey, f.installFor = input, key, nodeID
	return installer.Installation{ID: installerTestJobID, NodeID: nodeID, Status: "pending", Step: "queued"}, nil
}

func (f *fakeInstallerService) StartUninstall(_ context.Context, _, key, nodeID string, input installer.InstallInput) (installer.Installation, error) {
	if f.serviceErr != nil {
		return installer.Installation{}, f.serviceErr
	}
	f.uninstalls++
	f.install, f.installKey, f.installFor = input, key, nodeID
	return installer.Installation{ID: installerTestJobID, NodeID: nodeID, Status: "pending", Step: "queued"}, nil
}

func (f *fakeInstallerService) Get(_ context.Context, id string) (installer.Installation, error) {
	if f.serviceErr != nil {
		return installer.Installation{}, f.serviceErr
	}
	return installer.Installation{ID: id, NodeID: installerTestNodeID, Status: "succeeded", Step: "complete"}, nil
}

func (f *fakeInstallerService) Events(_ context.Context, id, cursor string) ([]installer.Event, installer.Installation, error) {
	if f.serviceErr != nil {
		return nil, installer.Installation{}, f.serviceErr
	}
	return []installer.Event{{
		ID: installerTestEvent, TS: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC),
		Step: "complete", Message: "The agent is installed and its first heartbeat was received.",
		Pct: 100, Status: "succeeded",
	}}, installer.Installation{ID: id, Status: "succeeded"}, nil
}

func newInstallerHTTPTestHandler(t *testing.T, service InstallerService) http.Handler {
	t.Helper()
	store := newAuthMemoryStore()
	hash, err := auth.HashPassword("owner test password")
	if err != nil {
		t.Fatal(err)
	}
	store.users["owner@example.test"] = auth.Credential{
		Identity: auth.Identity{ID: "owner-id", Email: "owner@example.test"}, PasswordHash: hash,
	}
	authService, err := auth.NewService(store, auth.Config{SessionTTL: time.Hour, LoginMaxAttempts: 5, LoginWindow: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return NewHandler(Options{
		Auth: authService, Installer: service,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func loginInstallerHTTPTestOwner(t *testing.T, handler http.Handler) (*http.Cookie, string) {
	t.Helper()
	bootstrap := httptest.NewRecorder()
	handler.ServeHTTP(bootstrap, httptest.NewRequest(http.MethodGet, "http://center.test/api/auth/me", nil))
	if bootstrap.Code != http.StatusOK {
		t.Fatalf("bootstrap status = %d: %s", bootstrap.Code, bootstrap.Body.String())
	}
	var session authEnvelope
	if err := jsonUnmarshal(bootstrap.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRequest(http.MethodPost, "http://center.test/api/auth/login", strings.NewReader(`{"email":"owner@example.test","password":"owner test password"}`))
	login.AddCookie(bootstrap.Result().Cookies()[0])
	login.Header.Set("Origin", "http://center.test")
	login.Header.Set("X-CSRF-Token", session.Data.CSRFToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, login)
	if response.Code != http.StatusOK {
		t.Fatalf("owner login status = %d: %s", response.Code, response.Body.String())
	}
	var loggedIn authEnvelope
	if err := jsonUnmarshal(response.Body.Bytes(), &loggedIn); err != nil {
		t.Fatal(err)
	}
	return response.Result().Cookies()[0], loggedIn.Data.CSRFToken
}

func jsonUnmarshal(raw []byte, target any) error {
	return json.Unmarshal(raw, target)
}

func TestInstallerAPIRequiresOwnerSessionAndCSRF(t *testing.T) {
	fake := &fakeInstallerService{}
	handler := newInstallerHTTPTestHandler(t, fake)
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "http://center.test/api/installations/"+installerTestJobID, nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET status = %d, want 401", unauthorized.Code)
	}

	cookie, csrf := loginInstallerHTTPTestOwner(t, handler)
	body := `{"host":"node.example.test","port":22}`
	withoutCSRF := httptest.NewRequest(http.MethodPost, "http://center.test/api/installer/probes", strings.NewReader(body))
	withoutCSRF.AddCookie(cookie)
	withoutCSRF.Header.Set("Origin", "http://center.test")
	withoutCSRF.Header.Set("Idempotency-Key", "probe-key")
	blocked := httptest.NewRecorder()
	handler.ServeHTTP(blocked, withoutCSRF)
	if blocked.Code != http.StatusForbidden {
		t.Fatalf("POST without CSRF status = %d, want 403", blocked.Code)
	}

	withCSRF := httptest.NewRequest(http.MethodPost, "http://center.test/api/installer/probes", strings.NewReader(body))
	withCSRF.AddCookie(cookie)
	withCSRF.Header.Set("Origin", "http://center.test")
	withCSRF.Header.Set("X-CSRF-Token", csrf)
	withCSRF.Header.Set("Idempotency-Key", "probe-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, withCSRF)
	if response.Code != http.StatusCreated {
		t.Fatalf("authenticated probe status = %d: %s", response.Code, response.Body.String())
	}
	if fake.probeOwner != "owner-id" || fake.probeInput.Host != "node.example.test" {
		t.Fatalf("probe service input = owner %q, %+v", fake.probeOwner, fake.probeInput)
	}
}

func TestInstallerAPIAcceptsCredentialsOnceAndStreamsSafeProgress(t *testing.T) {
	fake := &fakeInstallerService{}
	handler := newInstallerHTTPTestHandler(t, fake)
	cookie, csrf := loginInstallerHTTPTestOwner(t, handler)
	const sshSecret = "fixture-ssh-password"
	const sudoSecret = "fixture-sudo-password"
	body := `{"probeId":"` + installerTestEvent + `","fingerprint":"SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","ssh":{"host":"node.example.test","port":22,"user":"root","auth":"password","secret":"` + sshSecret + `","sudoPassword":"` + sudoSecret + `"}}`
	request := httptest.NewRequest(http.MethodPost, "http://center.test/api/nodes/"+installerTestNodeID+"/installations", strings.NewReader(body))
	request.AddCookie(cookie)
	request.Header.Set("Origin", "http://center.test")
	request.Header.Set("X-CSRF-Token", csrf)
	request.Header.Set("Idempotency-Key", "install-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("POST installation status = %d: %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Location") != "/api/installations/"+installerTestJobID {
		t.Fatalf("Location = %q", response.Header().Get("Location"))
	}
	if fake.install.SSH.Secret != sshSecret || fake.install.SSH.SudoPassword != sudoSecret || fake.installKey != "install-key" || fake.installFor != installerTestNodeID {
		t.Fatal("installer request was not passed to the owner-authorized service")
	}
	if strings.Contains(response.Body.String(), sshSecret) || strings.Contains(response.Body.String(), sudoSecret) {
		t.Fatal("installation response contains credentials")
	}

	status := httptest.NewRecorder()
	getRequest := httptest.NewRequest(http.MethodGet, "http://center.test/api/installations/"+installerTestJobID, nil)
	getRequest.AddCookie(cookie)
	handler.ServeHTTP(status, getRequest)
	if status.Code != http.StatusOK || strings.Contains(status.Body.String(), sshSecret) || strings.Contains(status.Body.String(), sudoSecret) {
		t.Fatalf("GET installation status = %d: %s", status.Code, status.Body.String())
	}

	stream := httptest.NewRecorder()
	streamRequest := httptest.NewRequest(http.MethodGet, "http://center.test/api/installations/"+installerTestJobID+"/events", nil)
	streamRequest.AddCookie(cookie)
	handler.ServeHTTP(stream, streamRequest)
	if stream.Code != http.StatusOK || !strings.Contains(stream.Body.String(), "event: progress\n") ||
		!strings.Contains(stream.Body.String(), "The agent is installed and its first heartbeat was received.") ||
		strings.Contains(stream.Body.String(), sshSecret) || strings.Contains(stream.Body.String(), sudoSecret) {
		t.Fatalf("SSE response = %d: %s", stream.Code, stream.Body.String())
	}
}

func TestInstallerAPIRejectsMalformedInputAndWrongMethods(t *testing.T) {
	handler := newInstallerHTTPTestHandler(t, &fakeInstallerService{})
	cookie, csrf := loginInstallerHTTPTestOwner(t, handler)
	request := httptest.NewRequest(http.MethodPost, "http://center.test/api/installer/probes", strings.NewReader(`{"host":"node.example.test","port":22,"secret":"unexpected"}`))
	request.AddCookie(cookie)
	request.Header.Set("Origin", "http://center.test")
	request.Header.Set("X-CSRF-Token", csrf)
	request.Header.Set("Idempotency-Key", "malformed-probe")
	bad := httptest.NewRecorder()
	handler.ServeHTTP(bad, request)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("unknown JSON field status = %d, want 400", bad.Code)
	}

	method := httptest.NewRequest(http.MethodGet, "http://center.test/api/installer/probes", nil)
	method.AddCookie(cookie)
	wrongMethod := httptest.NewRecorder()
	handler.ServeHTTP(wrongMethod, method)
	if wrongMethod.Code != http.StatusMethodNotAllowed || wrongMethod.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("wrong method response = %d Allow %q", wrongMethod.Code, wrongMethod.Header().Get("Allow"))
	}
}

func TestInstallerAPISafeErrorsAndBodyLimit(t *testing.T) {
	fake := &fakeInstallerService{}
	handler := newInstallerHTTPTestHandler(t, fake)
	cookie, csrf := loginInstallerHTTPTestOwner(t, handler)
	for _, test := range []struct {
		err    error
		status int
	}{
		{installer.ErrInvalid, 422}, {installer.ErrConflict, 409}, {installer.ErrCursorExpired, 409},
		{installer.ErrNotFound, 404}, {errors.New("secret-test-value raw database error"), 500},
	} {
		fake.serviceErr = test.err
		req := httptest.NewRequest("GET", "http://center.test/api/installations/"+installerTestJobID, nil)
		req.AddCookie(cookie)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != test.status || strings.Contains(response.Body.String(), "secret-test-value") {
			t.Fatalf("unsafe error mapping: %d %s", response.Code, response.Body.String())
		}
	}
	fake.serviceErr = nil
	req := httptest.NewRequest("POST", "http://center.test/api/installer/probes", strings.NewReader(`{"host":"`+strings.Repeat("a", maxInstallerRequestBody)+`","port":22}`))
	req.AddCookie(cookie)
	req.Header.Set("Origin", "http://center.test")
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("Idempotency-Key", "oversize")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize: %d", response.Code)
	}
}

func TestInstallerAPIReadRateLimit(t *testing.T) {
	handler := newInstallerHTTPTestHandler(t, &fakeInstallerService{})
	cookie, _ := loginInstallerHTTPTestOwner(t, handler)
	for range defaultAgentBurst {
		req := httptest.NewRequest("GET", "http://center.test/api/installations/"+installerTestJobID, nil)
		req.AddCookie(cookie)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != 200 {
			t.Fatalf("read before burst exhausted: %d", response.Code)
		}
	}
	req := httptest.NewRequest("GET", "http://center.test/api/installations/"+installerTestJobID, nil)
	req.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != 429 || response.Header().Get("Retry-After") == "" {
		t.Fatalf("missing read rate limit: %d", response.Code)
	}
}

func TestInstallerAPIUninstallRoute(t *testing.T) {
	fake := &fakeInstallerService{}
	handler := newInstallerHTTPTestHandler(t, fake)
	cookie, csrf := loginInstallerHTTPTestOwner(t, handler)
	body := `{"probeId":"` + installerTestEvent + `","fingerprint":"SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","ssh":{"host":"node.example.test","port":22,"user":"root","auth":"password","secret":"fixture-ssh-password"}}`
	path := "http://center.test/api/nodes/" + installerTestNodeID + "/uninstallations"
	post := func(prepare func(*http.Request)) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		prepare(request)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, request)
		return rec
	}
	if rec := post(func(r *http.Request) {}); rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("without credentials = %d", rec.Code)
	}
	signed := func(r *http.Request) {
		r.AddCookie(cookie)
		r.Header.Set("Origin", "http://center.test")
		r.Header.Set("X-CSRF-Token", csrf)
		r.Header.Set("Idempotency-Key", "uninstall-key")
	}
	if rec := post(func(r *http.Request) { signed(r); r.Header.Del("X-CSRF-Token") }); rec.Code != http.StatusForbidden {
		t.Fatalf("without CSRF = %d", rec.Code)
	}
	rec := post(signed)
	if rec.Code != http.StatusAccepted || rec.Header().Get("Location") != "/api/installations/"+installerTestJobID || fake.uninstalls != 1 || fake.installKey != "uninstall-key" || fake.installFor != installerTestNodeID {
		t.Fatalf("uninstall = %d %v (%d calls) %s", rec.Code, rec.Header(), fake.uninstalls, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"meta":{}`) || strings.Contains(rec.Body.String(), "fixture-ssh-password") || !strings.Contains(rec.Body.String(), "removal") {
		t.Fatalf("envelope = %s", rec.Body.String())
	}
	get := httptest.NewRequest(http.MethodGet, path, nil)
	get.AddCookie(cookie)
	wrong := httptest.NewRecorder()
	handler.ServeHTTP(wrong, get)
	if wrong.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d", wrong.Code)
	}
	fake.serviceErr = installer.ErrInvalid
	if rec := post(signed); rec.Code != http.StatusUnprocessableEntity && rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid input = %d", rec.Code)
	}
}

func TestInstallerSessionValidity(t *testing.T) {
	store := newAuthMemoryStore()
	service, err := auth.NewService(store, auth.Config{SessionTTL: time.Hour, LoginMaxAttempts: 5, LoginWindow: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	h := &installerAPI{auth: &serverAuth{service: service}}
	r := httptest.NewRequest(http.MethodGet, "/api/installations/job/events", nil)
	if h.sessionValid(r) {
		t.Fatal("missing cookie accepted")
	}
	r.Header.Set("Cookie", "speedtest_session=invalid")
	if h.sessionValid(r) {
		t.Fatal("invalid session accepted")
	}
}

func TestInstallerStreamCredentialValidPreservesSessionChecks(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/installations/job/events", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "valid-session"})
	service := scriptedAuthService{session: func(context.Context, string) (auth.SessionView, error) {
		return auth.SessionView{Identity: &auth.Identity{ID: "owner"}}, nil
	}}
	h := &installerAPI{auth: &serverAuth{service: service}}
	if !h.streamCredentialValid(r) {
		t.Fatal("valid session stream credential rejected")
	}
	h.auth.service = scriptedAuthService{session: func(context.Context, string) (auth.SessionView, error) {
		return auth.SessionView{}, auth.ErrInvalidSession
	}}
	if h.streamCredentialValid(r) {
		t.Fatal("invalid session stream credential accepted")
	}
}

func TestInstallerSessionRequiresAuthenticatedIdentity(t *testing.T) {
	r := httptest.NewRequest("GET", "/events", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "token"})
	for _, tc := range []struct {
		identity *auth.Identity
		err      error
		want     bool
	}{
		{nil, nil, false}, {&auth.Identity{}, nil, false}, {&auth.Identity{ID: "owner"}, nil, true}, {&auth.Identity{ID: "owner"}, errors.New("session unavailable"), false},
	} {
		h := &installerAPI{auth: &serverAuth{service: scriptedAuthService{session: func(context.Context, string) (auth.SessionView, error) {
			return auth.SessionView{Identity: tc.identity}, tc.err
		}}}}
		if got := h.sessionValid(r); got != tc.want {
			t.Fatalf("session validity=%t want=%t", got, tc.want)
		}
	}
}

type streamTokenAuthStore struct {
	tokenAuthStore
	recheckErr error
	rechecks   chan error
}

func (s *streamTokenAuthStore) Authenticate(_ context.Context, raw string) (apitoken.Principal, error) {
	s.calls++
	if raw != "valid-token" {
		return apitoken.Principal{}, apitoken.ErrInvalidToken
	}
	if s.calls == 1 {
		return s.principal, nil
	}
	s.rechecks <- s.recheckErr
	if s.recheckErr != nil {
		return apitoken.Principal{}, s.recheckErr
	}
	return s.principal, nil
}

func TestInstallerStreamRevalidatesBearerCredential(t *testing.T) {
	for _, tc := range []struct {
		name        string
		recheckErr  error
		withSession bool
	}{
		{name: "bearer-only stream survives a recheck"},
		{name: "expired bearer closes stream despite session", recheckErr: apitoken.ErrInvalidToken, withSession: true},
		{name: "revoked bearer closes stream despite session", recheckErr: apitoken.ErrInvalidToken, withSession: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rechecks, done, cancel, response := startInstallerBearerStream(t, tc.recheckErr, tc.withSession)
			defer cancel()
			select {
			case err := <-rechecks:
				if err != tc.recheckErr {
					t.Fatalf("recheck error = %v, want %v", err, tc.recheckErr)
				}
			case <-time.After(time.Second):
				t.Fatal("stream did not revalidate bearer token")
			}
			if tc.recheckErr == nil {
				select {
				case <-done:
					t.Fatal("stream ended after a valid bearer recheck")
				case <-time.After(25 * time.Millisecond):
				}
			}
			if tc.recheckErr == nil {
				cancel()
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("stream did not finish after cancellation or invalidation")
			}
			if response.Code != http.StatusOK {
				t.Fatalf("stream response status = %d", response.Code)
			}
		})
	}
}

func startInstallerBearerStream(t *testing.T, recheckErr error, withSession bool) (<-chan error, <-chan struct{}, context.CancelFunc, *httptest.ResponseRecorder) {
	t.Helper()
	principal := apitoken.Principal{TokenID: "token-1", UserID: "owner-1", Scope: apitoken.ReadOnly}
	tokens := &streamTokenAuthStore{tokenAuthStore: tokenAuthStore{principal: principal}, recheckErr: recheckErr, rechecks: make(chan error, 64)}
	sessionService := scriptedAuthService{}
	if withSession {
		sessionService.session = func(context.Context, string) (auth.SessionView, error) {
			return auth.SessionView{Identity: &auth.Identity{ID: "session-owner"}}, nil
		}
	}
	auth := &serverAuth{service: sessionService, tokens: tokens, tokenLimiter: newNodeRateLimiter(1000, 1000)}
	api := &installerAPI{service: &pollingInstaller{}, auth: auth, limiter: newNodeRateLimiter(1000, 1000), sessionCheckInterval: 5 * time.Millisecond}
	read := auth.requireSession(http.HandlerFunc(api.serve))
	routes := routeTable{{matches: onPath(installerRoute), tokenScope: operationScope}}
	handler := withAPITokenAuth(read, routes, auth)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	request := httptest.NewRequest(http.MethodGet, "/api/installations/"+installerTestJobID+"/events", nil).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer valid-token")
	if withSession {
		request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "valid-session"})
	}
	response, done := httptest.NewRecorder(), make(chan struct{})
	go func() {
		handler.ServeHTTP(response, request)
		close(done)
	}()
	return tokens.rechecks, done, cancel, response
}

func TestInstallerMethodRoutingAndErrors(t *testing.T) {
	api := &installerAPI{service: &fakeInstallerService{}, limiter: newNodeRateLimiter(1000, 1000)}
	for _, path := range []string{"/api/installer/probes", "/api/nodes/" + installerTestNodeID + "/installations", "/api/nodes/" + installerTestNodeID + "/uninstallations", "/api/installations/" + installerTestJobID, "/api/installations/" + installerTestJobID + "/events"} {
		r := httptest.NewRequest(http.MethodDelete, path, nil)
		r = r.WithContext(context.WithValue(r.Context(), ownerIdentityContextKey{}, "owner-id"))
		w := httptest.NewRecorder()
		api.serve(w, r)
		if w.Code != 405 {
			t.Fatalf("method %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	for _, tc := range []struct {
		err   error
		probe bool
		want  int
	}{
		{installer.ErrInvalid, false, 422}, {installer.ErrNotFound, false, 404}, {installer.ErrConflict, false, 409}, {installer.ErrDisconnect, true, 422}, {installer.ErrDisconnect, false, 500},
	} {
		w := httptest.NewRecorder()
		api.writeError(w, httptest.NewRequest("GET", "/", nil), tc.err, tc.probe)
		if w.Code != tc.want {
			t.Fatalf("error %v: %d", tc.err, w.Code)
		}
	}
	for _, path := range []string{"/wrong", "/api/installations/a/b", "/api/installations/a/events/extra"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", path, nil)
		api.get(w, r)
		if w.Code != 404 {
			t.Fatalf("invalid path: %d", w.Code)
		}
	}
	for _, body := range []string{`{"unknown":true}`, `{} {}`, `{"host":"` + strings.Repeat("x", maxInstallerRequestBody) + `"}`, `{} ` + strings.Repeat(" ", maxInstallerRequestBody)} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		var input installer.ProbeInput
		if decodeInstallerJSON(w, r, &input) || (w.Code != 400 && w.Code != 413) {
			t.Fatalf("bad JSON accepted: %d", w.Code)
		}
	}
	api.service = &fakeInstallerService{serviceErr: installer.ErrNotFound}
	w := httptest.NewRecorder()
	api.get(w, httptest.NewRequest("GET", "/api/installations/"+installerTestJobID, nil))
	if w.Code != 404 {
		t.Fatalf("missing job: %d", w.Code)
	}
	w = httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/installations/"+installerTestJobID+"/events", nil)
	r.Header.Add("Last-Event-ID", "a")
	r.Header.Add("Last-Event-ID", "b")
	api.stream(w, r)
	if w.Code != 400 {
		t.Fatalf("multiple cursors: %d", w.Code)
	}
	r.Header.Del("Last-Event-ID")
	w = httptest.NewRecorder()
	api.stream(w, r)
	if w.Code != 404 {
		t.Fatalf("missing stream: %d", w.Code)
	}
}

type brokenEventWriter struct{ http.ResponseWriter }

func (brokenEventWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func TestInstallEventWriteFailures(t *testing.T) {
	event := installer.Event{ID: "event", Status: "running"}
	if writeInstallEvents(brokenEventWriter{httptest.NewRecorder()}, func() error { return nil }, []installer.Event{event}) {
		t.Fatal("write failure swallowed")
	}
	if writeInstallEvents(httptest.NewRecorder(), func() error { return io.ErrClosedPipe }, []installer.Event{event}) {
		t.Fatal("flush failure swallowed")
	}
	event.TS = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	if writeInstallEvents(httptest.NewRecorder(), func() error { return nil }, []installer.Event{event}) {
		t.Fatal("invalid event timestamp marshalled")
	}
	if supportsFlush(brokenEventWriter{httptest.NewRecorder()}) {
		t.Fatal("non-flushing writer accepted")
	}
}

type pollingInstaller struct {
	fakeInstallerService
	calls int
	fail  bool
}

func (s *pollingInstaller) Events(context.Context, string, string) ([]installer.Event, installer.Installation, error) {
	s.calls++
	if s.calls == 1 {
		return nil, installer.Installation{Status: "running"}, nil
	}
	if s.fail {
		return nil, installer.Installation{}, errors.New("private poll failure")
	}
	return []installer.Event{{ID: "next", Step: "complete", Status: "succeeded", Message: "Installed"}}, installer.Installation{Status: "succeeded"}, nil
}
func TestInstallerStreamPollsAndEndsOnCompletionOrFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		service := &pollingInstaller{fail: fail}
		h := &installerAPI{service: service}
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		r := httptest.NewRequest("GET", "/api/installations/"+installerTestJobID+"/events", nil).WithContext(ctx)
		w := httptest.NewRecorder()
		h.stream(w, r)
		cancel()
		if service.calls != 2 || w.Code != 200 || !strings.Contains(w.Body.String(), "event: progress") || strings.Contains(w.Body.String(), "private poll failure") {
			t.Fatalf("calls=%d response=%d %s", service.calls, w.Code, w.Body.String())
		}
		want := "Installed"
		if fail {
			want = "Reconnect"
		}
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("missing %s: %s", want, w.Body.String())
		}
	}
}
func TestInstallerStreamCancellationAndUnsupportedWriter(t *testing.T) {
	h := &installerAPI{service: &pollingInstaller{}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	w := httptest.NewRecorder()
	h.stream(w, httptest.NewRequest("GET", "/api/installations/"+installerTestJobID+"/events", nil).WithContext(ctx))
	if w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("cancelled stream: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	h.service = &fakeInstallerService{}
	h.stream(brokenEventWriter{w}, httptest.NewRequest("GET", "/api/installations/"+installerTestJobID+"/events", nil))
	if w.Code != 500 {
		t.Fatalf("unsupported stream writer: %d", w.Code)
	}
}
