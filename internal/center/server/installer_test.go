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
