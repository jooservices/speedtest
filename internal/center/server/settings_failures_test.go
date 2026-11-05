package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSettingsUnavailableAndStorageFailures(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, body string
		store                    *fakeStore
		want                     int
	}{
		{"get", "GET", "/api/settings", "", &fakeStore{getErr: errors.New("private database detail")}, 500},
		{"put", "PUT", "/api/settings", validSettingsPayload, &fakeStore{replaceErr: errors.New("private database detail")}, 500},
		{"rotate", "POST", "/api/settings/enrolment-key", "", &fakeStore{rotateErr: errors.New("private database detail")}, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if tc.method == "POST" {
				serveEnrolmentKey(w, r, tc.store)
			} else {
				serveSettings(w, r, tc.store)
			}
			if w.Code != tc.want || strings.Contains(w.Body.String(), "private database detail") || !strings.Contains(w.Body.String(), "Internal failure") {
				t.Fatalf("response %d %s", w.Code, w.Body.String())
			}
		})
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, "/api/settings", nil)
		if method == http.MethodPost {
			serveEnrolmentKey(w, r, nil)
		} else {
			serveSettings(w, r, nil)
		}
		if w.Code != 503 || !strings.Contains(w.Body.String(), "not configured") {
			t.Fatalf("unconfigured: %d %s", w.Code, w.Body.String())
		}
	}
}
