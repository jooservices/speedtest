package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jooservices/speedtest/internal/center/apitoken"
	"github.com/jooservices/speedtest/internal/center/audit"
)

type fakeAudit struct {
	mu      sync.Mutex
	entries []audit.Entry
	listErr error
	failAdd bool
	gotPage [2]int
	filter  audit.Filter
}

func (f *fakeAudit) Record(_ context.Context, e audit.Entry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAdd {
		return errors.New("disk full")
	}
	f.entries = append(f.entries, e)
	return nil
}

func (f *fakeAudit) List(_ context.Context, filter audit.Filter, page, size int) ([]audit.Entry, int, error) {
	f.filter, f.gotPage = filter, [2]int{page, size}
	if f.listErr != nil {
		return nil, 0, f.listErr
	}
	return append([]audit.Entry{}, f.entries...), len(f.entries), nil
}

func (f *fakeAudit) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.entries) }

func TestAuditRecordsSuccessfulOwnerWritesOnly(t *testing.T) {
	store := &fakeAudit{}
	h := newNodeHandler(Options{Audit: store, OwnerNodes: &fakeOwnerNodes{}, NodeCredentials: &fakeNodeCredentialService{}})
	// A successful change is recorded with who, what and where from.
	rec := do(h, http.MethodPatch, "/api/nodes/"+nodeID, `{"note":"x"}`, ownerWriteWith(map[string]string{"If-Match": `"node-3"`, "X-Forwarded-For": "198.51.100.9"}))
	if rec.Code != http.StatusOK || store.count() != 1 {
		t.Fatalf("PATCH = %d, entries = %d", rec.Code, store.count())
	}
	got := store.entries[0]
	if got.Actor != "owner@example.test" || got.AuthMethod != audit.AuthMethodSession || got.Action != "node.update" || got.TargetID != nodeID || got.Remote == "" || got.Summary == "" {
		t.Fatalf("entry = %+v", got)
	}
	// Reads, refused writes and failed writes leave nothing behind.
	do(h, http.MethodGet, "/api/nodes/"+nodeID, "", ownerRead)
	do(h, http.MethodPatch, "/api/nodes/"+nodeID, `{"note":"x"}`, ownerWriteWith(nil)) // 428: no If-Match
	do(h, http.MethodPatch, "/api/nodes/"+nodeID, `{"note":"x"}`, nil)                 // no session
	if store.count() != 1 {
		t.Fatalf("only the successful write is recorded, got %d entries", store.count())
	}
	// A failing audit store never changes the answer to the owner.
	store.failAdd = true
	rec = do(h, http.MethodPatch, "/api/nodes/"+nodeID, `{"note":"y"}`, ownerWriteWith(map[string]string{"If-Match": `"node-3"`}))
	if rec.Code != http.StatusOK {
		t.Fatalf("audit failure leaked into the response: %d", rec.Code)
	}
}

func TestAuditDistinguishesBearerCredentials(t *testing.T) {
	store := &fakeAudit{}
	write := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) })
	handler := withAudit(write, store, &serverAuth{}, nil)
	credentials := []string{"10000000-0000-4000-8000-000000000101", "10000000-0000-4000-8000-000000000102"}
	for _, credentialID := range credentials {
		request := httptest.NewRequest(http.MethodPost, "/api/nodes", nil)
		request.Header.Set("Authorization", "Bearer secret-"+credentialID)
		request = request.WithContext(apitoken.WithPrincipal(request.Context(), apitoken.Principal{
			TokenID: credentialID, UserID: "owner-id", Email: "owner@example.test", Scope: apitoken.ReadWrite,
		}))
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}
	if len(store.entries) != 2 {
		t.Fatalf("successful writes = %d, want 2", len(store.entries))
	}
	for i, entry := range store.entries {
		if entry.Actor != "owner@example.test" || entry.AuthMethod != audit.AuthMethodBearer || entry.CredentialID != credentials[i] {
			t.Errorf("entry %d does not identify its bearer credential: %+v", i, entry)
		}
		encoded, err := json.Marshal(entry)
		if err != nil || strings.Contains(string(encoded), "secret-") {
			t.Errorf("audit entry disclosed bearer value: %s", encoded)
		}
	}
}

func TestAuditRecordsCreatedTokenTarget(t *testing.T) {
	store := &fakeAudit{}
	createdID := "10000000-0000-4000-8000-000000000103"
	create := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setAuditTargetID(r.Context(), createdID)
		w.WriteHeader(http.StatusCreated)
	})
	handler := withAudit(create, store, &serverAuth{service: nodeTestAuth{}}, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/access-tokens", nil)
	ownerWrite(request)
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if len(store.entries) != 1 || store.entries[0].TargetID != createdID || store.entries[0].AuthMethod != audit.AuthMethodSession {
		t.Fatalf("token creation entry = %+v", store.entries)
	}
}

func TestAuditEndpointListsForSignedInOwnersOnly(t *testing.T) {
	store := &fakeAudit{entries: []audit.Entry{{ID: "a", Actor: "owner@example.test", Action: "settings.update", Summary: "Changed settings"}}}
	h := newNodeHandler(Options{Audit: store, NodeCredentials: &fakeNodeCredentialService{}})
	if rec := do(h, http.MethodGet, "/api/audit", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("without a session = %d", rec.Code)
	}
	rec := do(h, http.MethodGet, "/api/audit?page=2&size=10&action=node.", "", ownerRead)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"settings.update"`) || !strings.Contains(rec.Body.String(), `"pagination"`) {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	if store.gotPage != [2]int{2, 10} || store.filter.Action != "node." {
		t.Fatalf("page/filter passed on: %v %+v", store.gotPage, store.filter)
	}
	if rec := do(h, http.MethodPost, "/api/audit", "", ownerWriteWith(nil)); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/api/audit?action="+strings.Repeat("a", 61), "", ownerRead); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("overlong filter = %d", rec.Code)
	}
	store.listErr = errors.New("db down")
	rec = do(h, http.MethodGet, "/api/audit", "", ownerRead)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "db down") {
		t.Fatalf("store failure = %d %s", rec.Code, rec.Body.String())
	}
	if !auditRoute("/api/audit") || auditRoute("/api/audit/x") {
		t.Fatal("route matching")
	}
}

func TestAuditActorNeedsASession(t *testing.T) {
	var none *serverAuth
	if none.actor(httptest.NewRequest(http.MethodPost, "/api/nodes", nil)) != "" {
		t.Fatal("no auth service, no actor")
	}
	a := &serverAuth{service: nodeTestAuth{}}
	if a.actor(httptest.NewRequest(http.MethodPost, "/api/nodes", nil)) != "" {
		t.Fatal("no cookie, no actor")
	}
}
