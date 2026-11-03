package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jooservices/speedtest/internal/center/ownerapi"
)

type fakeOwnerSchedules struct {
	err     error
	id      string
	input   ownerapi.ScheduleInput
	rules   ownerapi.ScheduleRules
	ifMatch string
	calls   []string
}

func (f *fakeOwnerSchedules) Schedules(context.Context) (map[string]any, string, error) {
	f.calls = append(f.calls, "list")
	return map[string]any{"rules": map[string]any{}, "nodes": []any{}, "clashes": []any{}}, `"schedules-1"`, f.err
}
func (f *fakeOwnerSchedules) Schedule(_ context.Context, id string) (map[string]any, string, error) {
	f.id = id
	return map[string]any{"nodeId": id}, `"schedule-2"`, f.err
}
func (f *fakeOwnerSchedules) ReplaceSchedule(_ context.Context, id string, in ownerapi.ScheduleInput, ifMatch string) (map[string]any, string, error) {
	f.id, f.input, f.ifMatch = id, in, ifMatch
	return map[string]any{"nodeId": id}, `"schedule-3"`, f.err
}
func (f *fakeOwnerSchedules) ReplaceRules(_ context.Context, in ownerapi.ScheduleRules, ifMatch string) (map[string]any, string, error) {
	f.rules, f.ifMatch = in, ifMatch
	return map[string]any{"stagger": in.Stagger}, `"schedules-2"`, f.err
}
func (f *fakeOwnerSchedules) Stagger(_ context.Context, ifMatch string) (map[string]any, string, error) {
	f.ifMatch = ifMatch
	f.calls = append(f.calls, "stagger")
	return map[string]any{"rules": map[string]any{}, "nodes": []any{}, "clashes": []any{}}, `"schedules-3"`, f.err
}

func schedulesHandlerFor(store OwnerSchedulesStore) http.Handler {
	return newNodeHandler(Options{OwnerSchedules: store, NodeCredentials: &fakeNodeCredentialService{}})
}

const goodRules = `{"stagger":true,"maxConcurrent":2,"quiet":{"quietStart":"22:00","quietEnd":"06:00","timezone":"Asia/Ho_Chi_Minh"}}`

func TestOwnerSchedulesReadsAndWritesNeedTheRightCredentials(t *testing.T) {
	h := schedulesHandlerFor(&fakeOwnerSchedules{})
	for _, path := range []string{"/api/schedules", "/api/schedules/" + nodeID} {
		if rec := do(h, http.MethodGet, path, "", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a session = %d", path, rec.Code)
		}
		if rec := do(h, http.MethodGet, path, "", ownerRead); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d", path, rec.Code)
		}
	}
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPut, "/api/schedules/" + nodeID, `{"interval":30,"offset":0,"enabled":true}`},
		{http.MethodPut, "/api/schedules/rules", goodRules},
		{http.MethodPost, "/api/schedules/stagger", ""},
	} {
		rec := do(h, tc.method, tc.path, tc.body, func(r *http.Request) { ownerRead(r); r.Header.Set("If-Match", `"x"`) })
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
			t.Errorf("%s %s without CSRF = %d, want 401/403", tc.method, tc.path, rec.Code)
		}
	}
}

func TestOwnerSchedulesETagsAndPreconditions(t *testing.T) {
	store := &fakeOwnerSchedules{}
	h := schedulesHandlerFor(store)
	if rec := do(h, http.MethodGet, "/api/schedules", "", ownerRead); rec.Header().Get("ETag") != `"schedules-1"` {
		t.Fatalf("collection ETag = %q", rec.Header().Get("ETag"))
	}
	if rec := do(h, http.MethodGet, "/api/schedules/"+nodeID, "", ownerRead); rec.Header().Get("ETag") != `"schedule-2"` || store.id != nodeID {
		t.Fatalf("node ETag = %q", rec.Header().Get("ETag"))
	}
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPut, "/api/schedules/" + nodeID, `{"interval":30,"offset":0,"enabled":true}`},
		{http.MethodPut, "/api/schedules/rules", goodRules},
		{http.MethodPost, "/api/schedules/stagger", ""},
	} {
		if rec := do(h, tc.method, tc.path, tc.body, ownerWriteWith(nil)); rec.Code != http.StatusPreconditionRequired {
			t.Errorf("%s %s without If-Match = %d, want 428", tc.method, tc.path, rec.Code)
		}
		rec := do(h, tc.method, tc.path, tc.body, ownerWriteWith(map[string]string{"If-Match": `"abc"`}))
		if rec.Code != http.StatusOK || store.ifMatch != `"abc"` || rec.Header().Get("ETag") == "" {
			t.Errorf("%s %s = %d (ifMatch %q etag %q)", tc.method, tc.path, rec.Code, store.ifMatch, rec.Header().Get("ETag"))
		}
		store.err = ownerapi.ErrStale
		if rec := do(h, tc.method, tc.path, tc.body, ownerWriteWith(map[string]string{"If-Match": `"abc"`})); rec.Code != http.StatusPreconditionFailed {
			t.Errorf("%s %s stale = %d, want 412", tc.method, tc.path, rec.Code)
		}
		store.err = nil
	}
}

func TestOwnerSchedulesReplaceValidation(t *testing.T) {
	store := &fakeOwnerSchedules{}
	h := schedulesHandlerFor(store)
	put := func(path, body string) int {
		return do(h, http.MethodPut, path, body, ownerWriteWith(map[string]string{"If-Match": `"abc"`})).Code
	}
	if code := put("/api/schedules/"+nodeID, `{"interval":15,"offset":14,"enabled":false}`); code != http.StatusOK ||
		store.input.Interval != 15 || store.input.Offset != 14 || store.input.Enabled {
		t.Fatalf("valid replace = %d %+v", code, store.input)
	}
	for _, body := range []string{
		`{"interval":4,"offset":0,"enabled":true}`, `{"interval":1441,"offset":0,"enabled":true}`, `{"interval":30,"offset":-1,"enabled":true}`,
		`{"interval":30,"offset":1440,"enabled":true}`, `{"interval":30,"enabled":true}`, `{"offset":0,"enabled":true}`, `{"interval":30,"offset":0}`,
	} {
		if code := put("/api/schedules/"+nodeID, body); code != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d, want 422", body, code)
		}
	}
	for _, body := range []string{`{"interval":30,"offset":0,"enabled":true,"extra":1}`, `not json`, ``} {
		if code := put("/api/schedules/"+nodeID, body); code != http.StatusBadRequest {
			t.Errorf("%q = %d, want 400", body, code)
		}
	}
	if code := put("/api/schedules/not-a-uuid", `{"interval":30,"offset":0,"enabled":true}`); code != http.StatusNotFound {
		t.Errorf("bad id = %d", code)
	}
	store.err = ownerapi.ErrNotFound
	if code := put("/api/schedules/"+nodeID, `{"interval":30,"offset":0,"enabled":true}`); code != http.StatusNotFound {
		t.Errorf("unknown node = %d", code)
	}
}

func TestOwnerSchedulesRulesValidation(t *testing.T) {
	store := &fakeOwnerSchedules{}
	h := schedulesHandlerFor(store)
	put := func(body string) int {
		return do(h, http.MethodPut, "/api/schedules/rules", body, ownerWriteWith(map[string]string{"If-Match": `"abc"`})).Code
	}
	if code := put(goodRules); code != http.StatusOK || !store.rules.Stagger || store.rules.MaxConcurrent != 2 || store.rules.Quiet.Start != "22:00" || store.rules.Quiet.Timezone != "Asia/Ho_Chi_Minh" {
		t.Fatalf("valid rules = %d %+v", code, store.rules)
	}
	for name, body := range map[string]string{
		"missing quiet":         `{"stagger":true,"maxConcurrent":1}`,
		"missing stagger":       `{"maxConcurrent":1,"quiet":{"quietStart":"","quietEnd":"","timezone":"UTC"}}`,
		"missing maxConcurrent": `{"stagger":true,"quiet":{"quietStart":"","quietEnd":"","timezone":"UTC"}}`,
		"zero concurrent":       `{"stagger":true,"maxConcurrent":0,"quiet":{"quietStart":"","quietEnd":"","timezone":"UTC"}}`,
		"six concurrent":        `{"stagger":true,"maxConcurrent":6,"quiet":{"quietStart":"","quietEnd":"","timezone":"UTC"}}`,
		"one quiet end":         `{"stagger":true,"maxConcurrent":1,"quiet":{"quietStart":"22:00","quietEnd":"","timezone":"UTC"}}`,
		"equal ends":            `{"stagger":true,"maxConcurrent":1,"quiet":{"quietStart":"22:00","quietEnd":"22:00","timezone":"UTC"}}`,
		"bad clock":             `{"stagger":true,"maxConcurrent":1,"quiet":{"quietStart":"25:00","quietEnd":"06:00","timezone":"UTC"}}`,
		"bad zone":              `{"stagger":true,"maxConcurrent":1,"quiet":{"quietStart":"22:00","quietEnd":"06:00","timezone":"Mars/Base"}}`,
	} {
		if code := put(body); code != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d, want 422", name, code)
		}
	}
}

func TestOwnerSchedulesRoutingAndFailures(t *testing.T) {
	store := &fakeOwnerSchedules{}
	h := schedulesHandlerFor(store)
	for method, path := range map[string]string{http.MethodPost: "/api/schedules", http.MethodGet: "/api/schedules/rules"} {
		if rec := do(h, method, path, "", ownerWriteWith(map[string]string{"If-Match": `"x"`})); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", method, path, rec.Code)
		}
	}
	if rec := do(h, http.MethodDelete, "/api/schedules/"+nodeID, "", ownerWriteWith(nil)); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE node schedule = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/api/schedules/stagger", "", ownerRead); rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
		t.Errorf("GET stagger = %d", rec.Code)
	}
	store.err = errors.New("db down")
	rec := do(h, http.MethodGet, "/api/schedules", "", ownerRead)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "db down") {
		t.Fatalf("store failure = %d %s", rec.Code, rec.Body.String())
	}
	for path, want := range map[string]bool{"/api/schedules": true, "/api/schedules/rules": true, "/api/schedules/" + nodeID: true, "/api/schedules/a/b": false, "/api/schedulesx": false} {
		if ownerSchedulesRoute(path) != want {
			t.Errorf("route %s = %v", path, !want)
		}
	}
}
