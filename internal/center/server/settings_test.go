package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jooservices/speedtest/internal/center/settings"
)

// validSettingsPayload is a complete, schema-valid PUT /api/settings body.
const validSettingsPayload = `{"theme":"light","language":"en","range":"24h","units":"auto","timezone":"browser","hour12":false,"decimals":1,"retentionDays":365,"geoProvider":"geolite2","geoFallback":"ip-api","geoAutoUpdate":true,"engine":"ookla","defaultThreads":4,"serverMode":"auto","maxConcurrent":1,"stagger":true,"quietStart":"","quietEnd":"","centerUrl":"https://speedtest.example.com","notify":{"email":{"on":true,"to":"ops@example.com"},"telegram":{"on":false,"to":""},"slack":{"on":false,"to":""},"webhook":{"on":false,"to":""}}}`

// fakeStore records calls and returns canned responses.
type fakeStore struct {
	getView     settings.Settings
	getETag     string
	getErr      error
	replaceView settings.Settings
	replaceETag string
	replaceErr  error
	issued      settings.IssuedKey
	rotateErr   error
	lastIfMatch string
	lastInput   settings.SettingsInput
}

func (f *fakeStore) Get(context.Context) (settings.Settings, string, error) {
	return f.getView, f.getETag, f.getErr
}

func (f *fakeStore) Replace(_ context.Context, ifMatch string, input settings.SettingsInput) (settings.Settings, string, error) {
	f.lastIfMatch = ifMatch
	f.lastInput = input
	return f.replaceView, f.replaceETag, f.replaceErr
}

func (f *fakeStore) RotateEnrollmentKey(context.Context) (settings.IssuedKey, error) {
	return f.issued, f.rotateErr
}

// settingsHandler signs every request in as the owner (session, CSRF token and origin); the tests that cover
// authentication call newSettingsHandler directly.
func settingsHandler(t *testing.T, store settings.Store) http.Handler {
	t.Helper()
	inner := newSettingsHandler(store)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ownerWrite(r)
		inner.ServeHTTP(w, r)
	})
}

func newSettingsHandler(store settings.Store) http.Handler {
	return newNodeHandler(Options{Settings: store, NodeCredentials: &fakeNodeCredentialService{}})
}

func TestGetSettings(t *testing.T) {
	store := &fakeStore{
		getView: settings.Defaults(""),
		getETag: `"3"`,
	}
	recorder := httptest.NewRecorder()
	settingsHandler(t, store).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/settings", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := recorder.Header().Get("ETag"); got != `"3"` {
		t.Errorf("ETag = %q, want %q", got, `"3"`)
	}
	var body successResponse
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Success || body.Code != http.StatusOK {
		t.Fatalf("envelope = %+v", body)
	}
	if body.Data == nil {
		t.Fatal("data is nil")
	}
}

func TestPutSettings(t *testing.T) {
	store := &fakeStore{
		replaceView: settings.Defaults(""),
		replaceETag: `"4"`,
	}
	request := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(validSettingsPayload))
	request.Header.Set("If-Match", `"3"`)
	recorder := httptest.NewRecorder()
	settingsHandler(t, store).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if store.lastIfMatch != `"3"` {
		t.Errorf("If-Match forwarded = %q, want %q", store.lastIfMatch, `"3"`)
	}
	if store.lastInput.Theme != "light" {
		t.Errorf("input theme = %q, want light", store.lastInput.Theme)
	}
	if got := recorder.Header().Get("ETag"); got != `"4"` {
		t.Errorf("ETag = %q, want %q", got, `"4"`)
	}
}

func TestPutSettingsValidationError(t *testing.T) {
	payload := `{"theme":"light","range":"24h","units":"auto","timezone":"browser","hour12":false,"decimals":1,"retentionDays":7,"geoProvider":"geolite2","geoFallback":"ip-api","geoAutoUpdate":true,"engine":"ookla","defaultThreads":4,"serverMode":"auto","maxConcurrent":1,"stagger":true,"quietStart":"","quietEnd":"","centerUrl":"https://speedtest.example.com","notify":{"email":{"on":true,"to":"ops@example.com"},"telegram":{"on":false,"to":""},"slack":{"on":false,"to":""},"webhook":{"on":false,"to":""}}}`
	request := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(payload))
	request.Header.Set("If-Match", `"1"`)
	recorder := httptest.NewRecorder()
	settingsHandler(t, &fakeStore{}).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", got)
	}
	var body problemResponse
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Type != "/problems/validation" || body.Status != http.StatusUnprocessableEntity {
		t.Fatalf("problem = %+v", body)
	}
	if body.Errors == nil || len(body.Errors["retentionDays"]) == 0 {
		t.Fatalf("errors = %+v, want retentionDays message", body.Errors)
	}
}

func TestPutSettingsMalformedJSON(t *testing.T) {
	request := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(`{"theme":`))
	request.Header.Set("If-Match", `"1"`)
	recorder := httptest.NewRecorder()
	settingsHandler(t, &fakeStore{}).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
}

func TestPutSettingsUnknownField(t *testing.T) {
	request := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(`{"unexpected":true}`))
	request.Header.Set("If-Match", `"1"`)
	recorder := httptest.NewRecorder()
	settingsHandler(t, &fakeStore{}).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
}

func TestPutSettingsPreconditionMapping(t *testing.T) {
	t.Run("missing If-Match", func(t *testing.T) {
		store := &fakeStore{replaceErr: settings.ErrPreconditionRequired}
		request := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(validSettingsPayload))
		recorder := httptest.NewRecorder()
		settingsHandler(t, store).ServeHTTP(recorder, request)
		if recorder.Code != http.StatusPreconditionRequired {
			t.Fatalf("status = %d, want 428", recorder.Code)
		}
	})
	t.Run("stale", func(t *testing.T) {
		store := &fakeStore{replaceErr: settings.ErrStale}
		request := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(validSettingsPayload))
		request.Header.Set("If-Match", `"1"`)
		recorder := httptest.NewRecorder()
		settingsHandler(t, store).ServeHTTP(recorder, request)
		if recorder.Code != http.StatusPreconditionFailed {
			t.Fatalf("status = %d, want 412", recorder.Code)
		}
	})
}

func TestRotateEnrolmentKey(t *testing.T) {
	store := &fakeStore{issued: settings.IssuedKey{EnrollToken: "st_enroll_test", CreatedAt: "2026-11-03T00:00:00Z"}}
	recorder := httptest.NewRecorder()
	settingsHandler(t, store).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/settings/enrolment-key", nil))

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", recorder.Code)
	}
	var body successResponse
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Code != http.StatusCreated {
		t.Fatalf("code = %d, want 201", body.Code)
	}
	data, ok := body.Data.(map[string]any)
	if !ok || data["enrollToken"] != "st_enroll_test" {
		t.Fatalf("data = %+v, want enrolment token", body.Data)
	}
}

func TestSettingsMethodNotAllowed(t *testing.T) {
	for _, tt := range []struct {
		path, method, allow string
	}{
		{"/api/settings", http.MethodPost, "GET, PUT, PATCH"},
		{"/api/settings/enrolment-key", http.MethodGet, "POST"},
	} {
		recorder := httptest.NewRecorder()
		settingsHandler(t, &fakeStore{}).ServeHTTP(recorder, httptest.NewRequest(tt.method, tt.path, nil))
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s status = %d, want 405", tt.method, tt.path, recorder.Code)
		}
		if got := recorder.Header().Get("Allow"); got != tt.allow {
			t.Errorf("Allow = %q, want %q", got, tt.allow)
		}
	}
}

func TestSettingsNotConfigured(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewHandler(Options{}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when no settings store is configured", recorder.Code)
	}
}

func TestSettingsRequireTheRightCredentials(t *testing.T) {
	store := &fakeStore{getView: settings.Defaults(""), getETag: `"1"`, replaceView: settings.Defaults(""), replaceETag: `"2"`,
		issued: settings.IssuedKey{EnrollToken: "st_enroll_test"}}
	h := newSettingsHandler(store)
	for _, path := range []string{"/api/settings", "/api/settings/enrolment-key"} {
		if rec := do(h, http.MethodGet, path, "", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a session = %d, want 401", path, rec.Code)
		}
	}
	if rec := do(h, http.MethodGet, "/api/settings", "", ownerRead); rec.Code != http.StatusOK {
		t.Fatalf("a session may read settings: %d", rec.Code)
	}
	writes := []struct{ method, path, body string }{
		{http.MethodPut, "/api/settings", validSettingsPayload},
		{http.MethodPost, "/api/settings/enrolment-key", ""},
	}
	for _, tc := range writes {
		for name, prepare := range map[string]func(*http.Request){
			"no credentials": nil,
			"session only":   func(r *http.Request) { ownerRead(r); r.Header.Set("If-Match", `"1"`) },
			"wrong origin": func(r *http.Request) {
				ownerRead(r)
				r.Header.Set("Origin", "http://attacker.test")
				r.Header.Set("X-CSRF-Token", "csrf-value")
				r.Header.Set("If-Match", `"1"`)
			},
		} {
			if rec := do(h, tc.method, tc.path, tc.body, prepare); rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
				t.Errorf("%s %s with %s = %d, want 401 or 403", tc.method, tc.path, name, rec.Code)
			}
		}
		if rec := do(h, tc.method, tc.path, tc.body, ownerWriteWith(map[string]string{"If-Match": `"1"`})); rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Errorf("%s %s as the owner = %d", tc.method, tc.path, rec.Code)
		}
	}
	if ownerSettingsRoute("/api/settings/other") || !ownerSettingsRoute("/api/settings") {
		t.Fatal("route matching")
	}
}

// settingsWithLanguage returns the defaults with the given saved language and the ETag a client would hold.
func settingsWithLanguage(language string) *fakeStore {
	view := settings.Defaults("")
	view.Language = language
	return &fakeStore{getView: view, getETag: `"3"`, replaceView: view, replaceETag: `"4"`}
}

func patchSettings(t *testing.T, store *fakeStore, body, ifMatch string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPatch, "/api/settings", strings.NewReader(body))
	if ifMatch != "" {
		request.Header.Set("If-Match", ifMatch)
	}
	recorder := httptest.NewRecorder()
	settingsHandler(t, store).ServeHTTP(recorder, request)
	return recorder
}

// TestPatchSettingsAcceptsVietnamese covers AC1: a saved vi is stored and returned as vi.
func TestPatchSettingsAcceptsVietnamese(t *testing.T) {
	store := settingsWithLanguage("en")
	store.replaceView.Language = "vi"
	recorder := patchSettings(t, store, `{"language":"vi"}`, `"3"`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if store.lastInput.Language != "vi" {
		t.Errorf("stored language = %q, want vi", store.lastInput.Language)
	}
}

// TestPatchSettingsRejectsUnsupportedLanguage covers AC2: anything other than en or vi answers 422 and nothing is
// stored, so the saved language stays unchanged.
func TestPatchSettingsRejectsUnsupportedLanguage(t *testing.T) {
	tests := map[string]string{
		"fr":    `{"language":"fr"}`,
		"empty": `{"language":""}`,
		"null":  `{"language":null}`,
		"VI":    `{"language":"VI"}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			store := settingsWithLanguage("vi")
			recorder := patchSettings(t, store, body, `"3"`)

			if recorder.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422: %s", recorder.Code, recorder.Body.String())
			}
			if store.lastIfMatch != "" {
				t.Errorf("Replace was called for %s; the saved language must not change", name)
			}
			if !strings.Contains(recorder.Body.String(), `"language"`) {
				t.Errorf("422 body does not name the language field: %s", recorder.Body.String())
			}
		})
	}
}

// TestPatchSettingsKeepsLanguageWhenOmitted covers AC3: a PATCH without language keeps the saved value and still
// applies the fields it sends.
func TestPatchSettingsKeepsLanguageWhenOmitted(t *testing.T) {
	store := settingsWithLanguage("vi")
	recorder := patchSettings(t, store, `{"theme":"light"}`, `"3"`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if store.lastInput.Language != "vi" {
		t.Errorf("language = %q, want the saved vi", store.lastInput.Language)
	}
	if store.lastInput.Theme != "light" {
		t.Errorf("theme = %q, want the patched light", store.lastInput.Theme)
	}
	if store.lastInput.Range != "7d" {
		t.Errorf("range = %q, want the saved 7d", store.lastInput.Range)
	}
}

func TestPatchSettingsPreconditions(t *testing.T) {
	store := settingsWithLanguage("en")
	if recorder := patchSettings(t, store, `{"theme":"light"}`, ""); recorder.Code != http.StatusPreconditionRequired {
		t.Errorf("missing If-Match status = %d, want 428", recorder.Code)
	}
	if recorder := patchSettings(t, store, `{"theme":"light"}`, `"2"`); recorder.Code != http.StatusPreconditionFailed {
		t.Errorf("stale If-Match status = %d, want 412", recorder.Code)
	}
}

func TestPatchSettingsMalformedBody(t *testing.T) {
	store := settingsWithLanguage("en")
	for _, body := range []string{`null`, `[]`, `{"unexpected":true}`, `{"theme":`} {
		if recorder := patchSettings(t, store, body, `"3"`); recorder.Code != http.StatusBadRequest {
			t.Errorf("body %s status = %d, want 400", body, recorder.Code)
		}
	}
}

// TestPutSettingsKeepsLanguageWhenOmitted covers AC3 for PUT: a body without language keeps the saved language.
func TestPutSettingsKeepsLanguageWhenOmitted(t *testing.T) {
	store := settingsWithLanguage("vi")
	body := strings.Replace(validSettingsPayload, `"language":"en",`, "", 1)
	request := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(body))
	request.Header.Set("If-Match", `"3"`)
	recorder := httptest.NewRecorder()
	settingsHandler(t, store).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if store.lastInput.Language != "vi" {
		t.Errorf("language = %q, want the saved vi", store.lastInput.Language)
	}
}

// TestPutSettingsRejectsEmptyLanguage covers AC2 for PUT: an explicit empty language is not "omitted" and answers 422.
func TestPutSettingsRejectsEmptyLanguage(t *testing.T) {
	store := settingsWithLanguage("vi")
	body := strings.Replace(validSettingsPayload, `"language":"en"`, `"language":""`, 1)
	request := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(body))
	request.Header.Set("If-Match", `"3"`)
	recorder := httptest.NewRecorder()
	settingsHandler(t, store).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", recorder.Code, recorder.Body.String())
	}
	if store.lastIfMatch != "" {
		t.Error("Replace was called for an empty language")
	}
}
