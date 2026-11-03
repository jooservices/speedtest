//go:build integration

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/auth"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/nodes"
	"github.com/jooservices/speedtest/internal/center/ownerapi"
)

// TestOwnerNodesHTTPFlowIntegration drives the whole owner node API over HTTP against PostgreSQL:
// sign in, create, list, read, conditional update, commands, pause rules and deletion.
func TestOwnerNodesHTTPFlowIntegration(t *testing.T) {
	pool, ctx, appKey := openServerIntegrationPool(t)
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
	store, err := auth.NewPostgresStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	authService, err := auth.NewService(store, auth.Config{SessionTTL: time.Hour, LoginMaxAttempts: 5, LoginWindow: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(Options{Auth: authService, NodeCredentials: nodes.NewService(pool, appKey), OwnerNodes: &ownerapi.Store{DB: pool}})
	session, csrf := loginIntegrationOwner(t, handler)

	call := func(method, path, body string, write bool, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://center.test"+path, strings.NewReader(body))
		if write {
			ownerHTTPWrite(req, session, csrf)
		} else {
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	created := call(http.MethodPost, "/api/nodes", `{"name":"edge-1","label":"Home","expectedDown":300,"expectedUp":100}`, true, map[string]string{"Idempotency-Key": "flow-create-1"})
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", created.Code, created.Body.String())
	}
	id := responseString(t, created.Body.Bytes(), "data", "node", "id")
	call(http.MethodPost, "/api/nodes", `{"name":"edge-0","expectedDown":500,"expectedUp":500}`, true, map[string]string{"Idempotency-Key": "flow-create-0"})

	list := call(http.MethodGet, "/api/nodes?size=1", "", false, nil)
	if list.Code != http.StatusOK {
		t.Fatalf("list = %d %s", list.Code, list.Body.String())
	}
	var page struct {
		Data []map[string]any `json:"data"`
		Meta struct {
			Pagination map[string]any `json:"pagination"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &page); err != nil || len(page.Data) != 1 || page.Data[0]["name"] != "edge-0" || page.Meta.Pagination["total"] != float64(2) || page.Meta.Pagination["last_page"] != float64(2) {
		t.Fatalf("list body = %s (%v)", list.Body.String(), err)
	}
	if page.Data[0]["status"] != "offline" || page.Data[0]["server"] != "auto" || page.Data[0]["latest"] != nil {
		t.Fatalf("a node that never reported must be offline with no result: %v", page.Data[0])
	}
	if anonymous := httptest.NewRecorder(); true {
		handler.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, "http://center.test/api/nodes", nil))
		if anonymous.Code != http.StatusUnauthorized {
			t.Fatalf("list without a session = %d", anonymous.Code)
		}
	}

	detail := call(http.MethodGet, "/api/nodes/"+id, "", false, nil)
	etag := detail.Header().Get("ETag")
	if detail.Code != http.StatusOK || etag == "" || responseString(t, detail.Body.Bytes(), "data", "label") != "Home" {
		t.Fatalf("detail = %d etag %q", detail.Code, etag)
	}
	patched := call(http.MethodPatch, "/api/nodes/"+id, `{"label":"Office","threads":8}`, true, map[string]string{"If-Match": etag})
	if patched.Code != http.StatusOK || patched.Header().Get("ETag") == etag || responseString(t, patched.Body.Bytes(), "data", "label") != "Office" {
		t.Fatalf("patch = %d %s", patched.Code, patched.Body.String())
	}
	if rec := call(http.MethodPatch, "/api/nodes/"+id, `{"label":"again"}`, true, map[string]string{"If-Match": etag}); rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("patch with the old ETag = %d, want 412", rec.Code)
	}
	if rec := call(http.MethodPatch, "/api/nodes/"+id, `{"label":"again"}`, true, nil); rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("patch without If-Match = %d, want 428", rec.Code)
	}
	if rec := call(http.MethodPatch, "/api/nodes/"+id, `{"name":"edge-0"}`, true, map[string]string{"If-Match": patched.Header().Get("ETag")}); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate name = %d, want 409", rec.Code)
	}

	run := call(http.MethodPost, "/api/nodes/"+id+"/run", "", true, map[string]string{"Idempotency-Key": "flow-run-1"})
	if run.Code != http.StatusAccepted || !strings.HasPrefix(run.Header().Get("Location"), "/api/nodes/"+id+"/commands/") {
		t.Fatalf("run = %d %s", run.Code, run.Body.String())
	}
	if rec := call(http.MethodPost, "/api/nodes/"+id+"/run", "", true, map[string]string{"Idempotency-Key": "flow-run-2"}); rec.Code != http.StatusConflict {
		t.Fatalf("a second manual run while one is queued = %d, want 409", rec.Code)
	}
	progress := call(http.MethodGet, run.Header().Get("Location"), "", false, nil)
	if progress.Code != http.StatusOK || responseString(t, progress.Body.Bytes(), "data", "status") != "pending" {
		t.Fatalf("command progress = %d %s", progress.Code, progress.Body.String())
	}
	if rec := call(http.MethodPost, "/api/nodes/"+id+"/commands", `{"type":"pause"}`, true, map[string]string{"Idempotency-Key": "flow-pause"}); rec.Code != http.StatusAccepted {
		t.Fatalf("pause = %d %s", rec.Code, rec.Body.String())
	}
	paused := call(http.MethodGet, "/api/nodes/"+id, "", false, nil)
	if responseString(t, paused.Body.Bytes(), "data", "status") != "paused" {
		t.Fatalf("a paused node must be shown as paused: %s", paused.Body.String())
	}
	if rec := call(http.MethodPost, "/api/nodes/"+id+"/commands", `{"type":"reboot"}`, true, map[string]string{"Idempotency-Key": "flow-bad"}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown command = %d, want 422", rec.Code)
	}

	current := call(http.MethodGet, "/api/nodes/"+id, "", false, nil).Header().Get("ETag")
	if rec := call(http.MethodDelete, "/api/nodes/"+id, "", true, map[string]string{"If-Match": current}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("delete without confirm = %d, want 422", rec.Code)
	}
	if rec := call(http.MethodDelete, "/api/nodes/"+id+"?confirm=true", "", true, map[string]string{"If-Match": current}); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	if rec := call(http.MethodGet, "/api/nodes/"+id, "", false, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("deleted node = %d, want 404", rec.Code)
	}
	if rec := call(http.MethodDelete, "/api/nodes/"+id+"?confirm=true", "", true, map[string]string{"If-Match": current}); rec.Code != http.StatusNotFound {
		t.Fatalf("repeated delete = %d, want 404", rec.Code)
	}
	t.Log("PASS: owner node API over HTTP: session reads, CSRF writes, pagination, ETag preconditions, commands and deletion")
}
