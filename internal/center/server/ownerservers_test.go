package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jooservices/speedtest/internal/center/ownerapi"
)

type fakeOwnerServers struct {
	catalogErr, selectErr, queueErr error
	engine, tag                     string
	serverID                        *int64
	queued                          bool
}

func (f *fakeOwnerServers) ServerCatalog(_ context.Context, _, engine string) (map[string]any, string, error) {
	f.engine = engine
	return map[string]any{"engine": engine, "servers": []any{}}, `"node-3"`, f.catalogErr
}

func (f *fakeOwnerServers) SelectServer(_ context.Context, _, engine string, id *int64, tag string) (map[string]any, string, error) {
	f.engine, f.serverID, f.tag = engine, id, tag
	return map[string]any{"engine": engine, "serverId": id}, `"node-4"`, f.selectErr
}

func (f *fakeOwnerServers) QueueServerList(_ context.Context, _, engine string) (map[string]any, error) {
	f.engine, f.queued = engine, true
	return map[string]any{"id": commandID, "engine": engine, "status": "pending"}, f.queueErr
}

func ownerServersHandlerFor(store OwnerServersStore) http.Handler {
	return newNodeHandler(Options{OwnerServers: store})
}

func TestOwnerServerCatalogReadAndQueue(t *testing.T) {
	store := &fakeOwnerServers{}
	h := ownerServersHandlerFor(store)
	path := "/api/nodes/" + nodeID + "/servers?engine=librespeed"
	if rec := do(h, http.MethodGet, path, "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET without session = %d", rec.Code)
	}
	read := do(h, http.MethodGet, path, "", ownerRead)
	if read.Code != http.StatusOK || read.Header().Get("ETag") != `"node-3"` || store.engine != "librespeed" {
		t.Fatalf("GET catalog = %d %q %s", read.Code, read.Header().Get("ETag"), read.Body.String())
	}
	queued := do(h, http.MethodPost, path, "", ownerWriteWith(nil))
	if queued.Code != http.StatusAccepted || !store.queued || store.engine != "librespeed" || queued.Header().Get("Location") != "/api/nodes/"+nodeID+"/commands/"+commandID {
		t.Fatalf("POST catalog refresh = %d %q %s", queued.Code, queued.Header().Get("Location"), queued.Body.String())
	}
	for _, bad := range []string{"", "?engine=other", "?engine=ookla&engine=librespeed", "?engine=ookla&extra=x"} {
		if rec := do(h, http.MethodGet, "/api/nodes/"+nodeID+"/servers"+bad, "", ownerRead); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("bad query %q = %d", bad, rec.Code)
		}
	}
}

func TestOwnerServerSelectionValidatesAndUsesETag(t *testing.T) {
	store := &fakeOwnerServers{}
	h := ownerServersHandlerFor(store)
	path := "/api/nodes/" + nodeID + "/server-selection"
	if rec := do(h, http.MethodPut, path, `{"engine":"ookla","serverId":12}`, ownerWriteWith(nil)); rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("PUT without If-Match = %d", rec.Code)
	}
	write := ownerWriteWith(map[string]string{"If-Match": `"node-3"`})
	for _, tc := range []struct {
		body   string
		engine string
		id     *int64
	}{{`{"engine":"ookla","serverId":"auto"}`, "ookla", nil}, {`{"engine":"librespeed","serverId":12}`, "librespeed", int64Ptr(12)}} {
		rec := do(h, http.MethodPut, path, tc.body, write)
		if rec.Code != http.StatusOK || store.tag != `"node-3"` {
			t.Fatalf("PUT %s = %d %s", tc.body, rec.Code, rec.Body.String())
		}
		if store.engine != tc.engine || (tc.id == nil) != (store.serverID == nil) || tc.id != nil && *store.serverID != *tc.id {
			t.Fatalf("selection body %s stored %s/%v", tc.body, store.engine, store.serverID)
		}
	}
	for _, body := range []string{`{"engine":"ookla","serverId":0}`, `{"engine":"ookla","serverId":1.5}`, `{"engine":"ookla","serverId":"other"}`, `{"engine":"","serverId":"auto"}`, `{"engine":"ookla"}`} {
		if rec := do(h, http.MethodPut, path, body, ownerWriteWith(map[string]string{"If-Match": `"node-3"`})); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("invalid selection %s = %d", body, rec.Code)
		}
	}
	store.selectErr = ownerapi.ErrStale
	if rec := do(h, http.MethodPut, path, `{"engine":"ookla","serverId":"auto"}`, ownerWriteWith(map[string]string{"If-Match": `"node-3"`})); rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale ETag = %d", rec.Code)
	}
	store.selectErr = errors.New("private database failure")
	if rec := do(h, http.MethodPut, path, `{"engine":"ookla","serverId":"auto"}`, ownerWriteWith(map[string]string{"If-Match": `"node-3"`})); rec.Code != http.StatusInternalServerError {
		t.Fatalf("storage error = %d", rec.Code)
	}
}

func int64Ptr(value int64) *int64 { return &value }

func TestOwnerServerRoutesRemainScoped(t *testing.T) {
	for path, want := range map[string]bool{
		"/api/nodes/" + nodeID + "/servers": true, "/api/nodes/" + nodeID + "/server-selection": true,
		"/api/nodes/" + nodeID + "/servers/extra": false, "/api/nodes/bad/servers": false,
		"/api/nodes/" + nodeID + "/commands": false,
	} {
		if got := ownerServersRoute(path); got != want {
			t.Errorf("ownerServersRoute(%q) = %v", path, got)
		}
	}
}

func TestOwnerServerDispatchAndStoreErrors(t *testing.T) {
	store := &fakeOwnerServers{}
	h := &ownerServers{store: store, limiter: newOwnerRateLimiter()}
	for _, path := range []string{"/wrong", "/api/nodes/" + nodeID + "/unknown"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if path != "/wrong" {
			req = req.WithContext(context.WithValue(req.Context(), ownerIdentityContextKey{}, "owner"))
		}
		rec := httptest.NewRecorder()
		h.serve(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d", path, rec.Code)
		}
	}
	handler := ownerServersHandlerFor(store)
	path := "/api/nodes/" + nodeID + "/servers?engine=ookla"
	store.catalogErr = ownerapi.ErrNotFound
	if rec := do(handler, http.MethodGet, path, "", ownerRead); rec.Code != http.StatusNotFound {
		t.Errorf("missing catalog node = %d", rec.Code)
	}
	store.queueErr = ownerapi.ErrConflict
	if rec := do(handler, http.MethodPost, path, "", ownerWriteWith(nil)); rec.Code != http.StatusConflict {
		t.Errorf("duplicate discovery = %d", rec.Code)
	}
	if rec := do(handler, http.MethodPut, path, "", ownerWriteWith(nil)); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT catalog = %d", rec.Code)
	}
	selection := "/api/nodes/" + nodeID + "/server-selection"
	if rec := do(handler, http.MethodGet, selection, "", ownerRead); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET selection = %d", rec.Code)
	}
	store.selectErr = ownerapi.ErrInvalid
	if rec := do(handler, http.MethodPut, selection, `{"engine":"ookla","serverId":"auto"}`, ownerWriteWith(map[string]string{"If-Match": `"node-3"`})); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("invalid store selection = %d", rec.Code)
	}
}
