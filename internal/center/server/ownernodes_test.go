package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jooservices/speedtest/internal/center/ownerapi"
)

const (
	nodeID    = "00000000-0000-4000-8000-0000000000a1"
	commandID = "00000000-0000-4000-8000-0000000000c1"
)

type fakeOwnerNodes struct {
	listErr, getErr, updateErr, deleteErr, queueErr, commandErr error
	page, size                                                  int
	total                                                       int
	patch                                                       map[string]json.RawMessage
	ifMatch                                                     string
	deleted                                                     string
	queuedType, queuedVersion                                   string
}

func (f *fakeOwnerNodes) List(_ context.Context, page, size int) ([]map[string]any, int, error) {
	f.page, f.size = page, size
	return []map[string]any{{"id": nodeID, "name": "VM1"}}, f.total, f.listErr
}
func (f *fakeOwnerNodes) Get(_ context.Context, id string) (map[string]any, string, error) {
	return map[string]any{"id": id, "name": "VM1"}, `"node-3"`, f.getErr
}
func (f *fakeOwnerNodes) Update(_ context.Context, id string, patch map[string]json.RawMessage, ifMatch string) (map[string]any, string, error) {
	f.patch, f.ifMatch = patch, ifMatch
	return map[string]any{"id": id, "name": "VM1b"}, `"node-4"`, f.updateErr
}
func (f *fakeOwnerNodes) Delete(_ context.Context, id, ifMatch string) error {
	f.deleted, f.ifMatch = id, ifMatch
	return f.deleteErr
}
func (f *fakeOwnerNodes) QueueCommand(_ context.Context, id, typ, version string) (map[string]any, error) {
	f.queuedType, f.queuedVersion = typ, version
	return map[string]any{"id": commandID, "nodeId": id, "type": typ, "status": "pending"}, f.queueErr
}
func (f *fakeOwnerNodes) Command(_ context.Context, id, cid string) (map[string]any, error) {
	return map[string]any{"id": cid, "nodeId": id, "status": "running"}, f.commandErr
}

func ownerRead(r *http.Request) {
	r.AddCookie(&http.Cookie{Name: "speedtest_session", Value: "owner-session"})
}

func do(handler http.Handler, method, path, body string, prepare func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://center.test"+path, strings.NewReader(body))
	if prepare != nil {
		prepare(req)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func ownerNodesHandlerFor(store OwnerNodesStore) http.Handler {
	return newNodeHandler(Options{OwnerNodes: store, NodeCredentials: &fakeNodeCredentialService{}})
}

func TestOwnerNodesReadsNeedASessionButNoCSRF(t *testing.T) {
	h := ownerNodesHandlerFor(&fakeOwnerNodes{total: 1})
	if rec := do(h, http.MethodGet, "/api/nodes", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("list without a session = %d", rec.Code)
	}
	rec := do(h, http.MethodGet, "/api/nodes", "", ownerRead)
	if rec.Code != http.StatusOK {
		t.Fatalf("list with a session = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodGet, "/api/nodes/"+nodeID, "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("detail without a session = %d", rec.Code)
	}
}

func TestOwnerNodesWritesNeedSessionCSRFAndOrigin(t *testing.T) {
	h := ownerNodesHandlerFor(&fakeOwnerNodes{})
	for name, prepare := range map[string]func(*http.Request){
		"no session": func(r *http.Request) {
			r.Header.Set("Origin", "http://center.test")
			r.Header.Set("X-CSRF-Token", "csrf-value")
		},
		"no csrf": func(r *http.Request) { ownerRead(r); r.Header.Set("Origin", "http://center.test") },
		"wrong origin": func(r *http.Request) {
			ownerRead(r)
			r.Header.Set("Origin", "http://attacker.test")
			r.Header.Set("X-CSRF-Token", "csrf-value")
		},
	} {
		rec := do(h, http.MethodPatch, "/api/nodes/"+nodeID, `{"note":"x"}`, func(r *http.Request) { prepare(r); r.Header.Set("If-Match", `"node-3"`) })
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 401 or 403", name, rec.Code)
		}
	}
}

func TestOwnerNodesListPagination(t *testing.T) {
	store := &fakeOwnerNodes{total: 51}
	h := ownerNodesHandlerFor(store)
	rec := do(h, http.MethodGet, "/api/nodes?page=2&size=10", "", ownerRead)
	var env struct {
		Data []map[string]any `json:"data"`
		Meta struct {
			Pagination struct {
				Current, Per, Total, Last int
			} `json:"pagination"`
		} `json:"meta"`
	}
	raw := rec.Body.Bytes()
	if err := json.Unmarshal(raw, &env); err != nil || len(env.Data) != 1 || store.page != 2 || store.size != 10 {
		t.Fatalf("list response %s (store %d/%d): %v", raw, store.page, store.size, err)
	}
	var generic map[string]any
	_ = json.Unmarshal(raw, &generic)
	p := generic["meta"].(map[string]any)["pagination"].(map[string]any)
	if p["current_page"] != float64(2) || p["per_page"] != float64(10) || p["total"] != float64(51) || p["last_page"] != float64(6) {
		t.Fatalf("pagination = %v", p)
	}
	empty := ownerNodesHandlerFor(&fakeOwnerNodes{total: 0})
	rec = do(empty, http.MethodGet, "/api/nodes", "", ownerRead)
	_ = json.Unmarshal(rec.Body.Bytes(), &generic)
	if generic["meta"].(map[string]any)["pagination"].(map[string]any)["last_page"] != float64(1) {
		t.Fatalf("last_page of an empty list must be 1: %s", rec.Body.String())
	}
	for _, bad := range []string{"page=0", "size=0", "size=101", "page=x", "size=-1"} {
		if rec := do(h, http.MethodGet, "/api/nodes?"+bad, "", ownerRead); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422", bad, rec.Code)
		}
	}
}

func TestOwnerNodesDetailAndETag(t *testing.T) {
	h := ownerNodesHandlerFor(&fakeOwnerNodes{})
	rec := do(h, http.MethodGet, "/api/nodes/"+nodeID, "", ownerRead)
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") != `"node-3"` {
		t.Fatalf("detail = %d etag %q", rec.Code, rec.Header().Get("ETag"))
	}
	if rec := do(h, http.MethodGet, "/api/nodes/not-a-uuid", "", ownerRead); rec.Code != http.StatusNotFound {
		t.Fatalf("malformed id = %d, want 404", rec.Code)
	}
	missing := ownerNodesHandlerFor(&fakeOwnerNodes{getErr: ownerapi.ErrNotFound})
	if rec := do(missing, http.MethodGet, "/api/nodes/"+nodeID, "", ownerRead); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown node = %d", rec.Code)
	}
}

func ownerWriteWith(headers map[string]string) func(*http.Request) {
	return func(r *http.Request) {
		ownerRead(r)
		r.Header.Set("Origin", "http://center.test")
		r.Header.Set("X-CSRF-Token", "csrf-value")
		for k, v := range headers {
			r.Header.Set(k, v)
		}
	}
}

func TestOwnerNodesPatch(t *testing.T) {
	store := &fakeOwnerNodes{}
	h := ownerNodesHandlerFor(store)
	if rec := do(h, http.MethodPatch, "/api/nodes/"+nodeID, `{"note":"x"}`, ownerWriteWith(nil)); rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("PATCH without If-Match = %d, want 428", rec.Code)
	}
	rec := do(h, http.MethodPatch, "/api/nodes/"+nodeID, `{"note":"x","enabled":false}`, ownerWriteWith(map[string]string{"If-Match": `"node-3"`}))
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") != `"node-4"` || store.ifMatch != `"node-3"` || string(store.patch["note"]) != `"x"` {
		t.Fatalf("PATCH = %d etag %q ifMatch %q patch %v", rec.Code, rec.Header().Get("ETag"), store.ifMatch, store.patch)
	}
	for name, tc := range map[string]struct {
		err  error
		want int
		body string
	}{
		"stale":    {ownerapi.ErrStale, http.StatusPreconditionFailed, `{"note":"x"}`},
		"conflict": {ownerapi.ErrConflict, http.StatusConflict, `{"name":"dup"}`},
		"invalid":  {ownerapi.ErrInvalid, http.StatusUnprocessableEntity, `{"threads":0}`},
		"missing":  {ownerapi.ErrNotFound, http.StatusNotFound, `{"note":"x"}`},
		"failure":  {errors.New("boom"), http.StatusInternalServerError, `{"note":"x"}`},
	} {
		hh := ownerNodesHandlerFor(&fakeOwnerNodes{updateErr: tc.err})
		rec := do(hh, http.MethodPatch, "/api/nodes/"+nodeID, tc.body, ownerWriteWith(map[string]string{"If-Match": `"node-3"`}))
		if rec.Code != tc.want || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/problem+json") {
			t.Errorf("%s: status %d content type %q, want %d problem+json", name, rec.Code, rec.Header().Get("Content-Type"), tc.want)
		}
	}
	if rec := do(h, http.MethodPatch, "/api/nodes/"+nodeID, `{not json`, ownerWriteWith(map[string]string{"If-Match": `"node-3"`})); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed JSON = %d", rec.Code)
	}
}

func TestOwnerNodesDelete(t *testing.T) {
	store := &fakeOwnerNodes{}
	h := ownerNodesHandlerFor(store)
	if rec := do(h, http.MethodDelete, "/api/nodes/"+nodeID+"?confirm=true", "", ownerWriteWith(nil)); rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("DELETE without If-Match = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, "/api/nodes/"+nodeID, "", ownerWriteWith(map[string]string{"If-Match": `"node-3"`})); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("DELETE without confirm = %d, want 422", rec.Code)
	}
	if store.deleted != "" {
		t.Fatal("a node was deleted without confirmation")
	}
	rec := do(h, http.MethodDelete, "/api/nodes/"+nodeID+"?confirm=true", "", ownerWriteWith(map[string]string{"If-Match": `"node-3"`}))
	if rec.Code != http.StatusNoContent || store.deleted != nodeID || rec.Body.Len() != 0 {
		t.Fatalf("DELETE = %d deleted %q", rec.Code, store.deleted)
	}
	gone := ownerNodesHandlerFor(&fakeOwnerNodes{deleteErr: ownerapi.ErrNotFound})
	if rec := do(gone, http.MethodDelete, "/api/nodes/"+nodeID+"?confirm=true", "", ownerWriteWith(map[string]string{"If-Match": `"node-3"`})); rec.Code != http.StatusNotFound {
		t.Fatalf("repeated DELETE = %d, want 404", rec.Code)
	}
}

func TestOwnerNodesCommands(t *testing.T) {
	store := &fakeOwnerNodes{}
	h := ownerNodesHandlerFor(store)
	rec := do(h, http.MethodPost, "/api/nodes/"+nodeID+"/run", "", ownerWriteWith(nil))
	if rec.Code != http.StatusAccepted || store.queuedType != "run_now" || rec.Header().Get("Location") != "/api/nodes/"+nodeID+"/commands/"+commandID {
		t.Fatalf("run = %d type %q location %q", rec.Code, store.queuedType, rec.Header().Get("Location"))
	}
	rec = do(h, http.MethodPost, "/api/nodes/"+nodeID+"/commands", `{"type":"update_agent","version":"1.2.3"}`, ownerWriteWith(nil))
	if rec.Code != http.StatusAccepted || store.queuedType != "update_agent" || store.queuedVersion != "1.2.3" {
		t.Fatalf("commands = %d %q %q", rec.Code, store.queuedType, store.queuedVersion)
	}
	for name, tc := range map[string]struct {
		err  error
		want int
	}{"paused or busy": {ownerapi.ErrConflict, http.StatusConflict}, "invalid": {ownerapi.ErrInvalid, http.StatusUnprocessableEntity}, "unknown node": {ownerapi.ErrNotFound, http.StatusNotFound}} {
		hh := ownerNodesHandlerFor(&fakeOwnerNodes{queueErr: tc.err})
		if rec := do(hh, http.MethodPost, "/api/nodes/"+nodeID+"/run", "", ownerWriteWith(nil)); rec.Code != tc.want {
			t.Errorf("%s: run = %d, want %d", name, rec.Code, tc.want)
		}
	}
	if rec := do(h, http.MethodGet, "/api/nodes/"+nodeID+"/commands/"+commandID, "", ownerRead); rec.Code != http.StatusOK {
		t.Fatalf("command progress = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/api/nodes/"+nodeID+"/commands/bad", "", ownerRead); rec.Code != http.StatusNotFound {
		t.Fatalf("malformed command id = %d", rec.Code)
	}
	missing := ownerNodesHandlerFor(&fakeOwnerNodes{commandErr: ownerapi.ErrNotFound})
	if rec := do(missing, http.MethodGet, "/api/nodes/"+nodeID+"/commands/"+commandID, "", ownerRead); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown command = %d", rec.Code)
	}
}

func TestOwnerNodesMethodsAndRouting(t *testing.T) {
	h := ownerNodesHandlerFor(&fakeOwnerNodes{})
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/nodes/" + nodeID + "/commands/" + commandID},
		{http.MethodGet, "/api/nodes/" + nodeID + "/run"},
		{http.MethodPut, "/api/nodes/" + nodeID},
		{http.MethodDelete, "/api/nodes"},
	} {
		rec := do(h, tc.method, tc.path, "", ownerWriteWith(nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", tc.method, tc.path, rec.Code)
		}
	}
	// Creating a node and the credential routes still reach the enrolment handler.
	create := do(h, http.MethodPost, "/api/nodes", `{"name":"edge","expectedDown":300,"expectedUp":100}`, ownerWriteWith(map[string]string{"Idempotency-Key": "k1"}))
	if create.Code != http.StatusCreated {
		t.Fatalf("create node = %d %s", create.Code, create.Body.String())
	}
	for _, path := range []string{"/api/nodes/" + nodeID + "/credential/extra", "/api/nodes/" + nodeID + "/unknown"} {
		if rec := do(h, http.MethodGet, path, "", ownerRead); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
	if !ownerNodesRoute("/api/nodes/"+nodeID+"/commands/"+commandID) || ownerNodesRoute("/api/nodes/"+nodeID+"/credential") || ownerNodesRoute("/api/other") || ownerNodesRoute("/api/nodes/a/b/c/d") {
		t.Fatal("ownerNodesRoute classification is wrong")
	}
}

func TestOwnerNodesDisabledWithoutStore(t *testing.T) {
	h := newNodeHandler(Options{NodeCredentials: &fakeNodeCredentialService{}})
	if rec := do(h, http.MethodGet, "/api/nodes", "", ownerRead); rec.Code == http.StatusOK {
		t.Fatal("without a store the owner node API must not answer 200")
	}
}
