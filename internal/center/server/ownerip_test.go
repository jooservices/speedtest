package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jooservices/speedtest/internal/center/geo"
	"github.com/jooservices/speedtest/internal/center/ipapi"
)

type fakeOwnerIP struct {
	node, ip   string
	page, size int
	err        error
	geo        map[string]any
	providers  []geo.ProviderStatus
}

func (f *fakeOwnerIP) History(_ context.Context, node string, page, size int) ([]map[string]any, int, error) {
	f.node, f.page, f.size = node, page, size
	return []map[string]any{{"id": "h1"}}, 51, f.err
}
func (f *fakeOwnerIP) Addresses(_ context.Context, node string, page, size int) ([]map[string]any, int, error) {
	f.node, f.page, f.size = node, page, size
	return []map[string]any{{"ip": "203.0.113.5"}}, 0, f.err
}
func (f *fakeOwnerIP) Map(_ context.Context, node string) ([]map[string]any, error) {
	f.node = node
	return []map[string]any{{"nodeId": nodeID}}, f.err
}
func (f *fakeOwnerIP) Lookup(_ context.Context, ip string) (map[string]any, error) {
	f.ip = ip
	if f.err == nil && strings.Contains(ip, ".") && strings.HasPrefix(ip, "x") {
		return nil, ipapi.ErrInvalid
	}
	return f.geo, f.err
}
func (f *fakeOwnerIP) Providers(context.Context) ([]geo.ProviderStatus, error) {
	return f.providers, f.err
}

func ipHandlerFor(store OwnerIPStore) http.Handler {
	return newNodeHandler(Options{OwnerIP: store, NodeCredentials: &fakeNodeCredentialService{}})
}

func TestOwnerIPNeedsASessionAndOnlyReads(t *testing.T) {
	h := ipHandlerFor(&fakeOwnerIP{})
	for _, path := range []string{"/api/ip/history", "/api/ip/addresses", "/api/ip/map", "/api/ip/lookup?ip=203.0.113.5", "/api/ip/providers"} {
		if rec := do(h, http.MethodGet, path, "", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a session = %d", path, rec.Code)
		}
		if rec := do(h, http.MethodGet, path, "", ownerRead); rec.Code != http.StatusOK {
			t.Errorf("%s = %d", path, rec.Code)
		}
		if rec := do(h, http.MethodPost, path, "", ownerWriteWith(nil)); rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
			t.Errorf("POST %s = %d", path, rec.Code)
		}
	}
	for path, want := range map[string]bool{"/api/ip/history": true, "/api/ip/providers": true, "/api/ip/service": false, "/api/ip/refreshes": false, "/api/ip": false} {
		if ownerIPRoute(path) != want {
			t.Errorf("route %s = %v", path, !want)
		}
	}
}

func TestOwnerIPListsPaginateAndFilterByNode(t *testing.T) {
	store := &fakeOwnerIP{}
	h := ipHandlerFor(store)
	rec := do(h, http.MethodGet, "/api/ip/history?node="+nodeID+"&page=2&size=10", "", ownerRead)
	if rec.Code != http.StatusOK || store.node != nodeID || store.page != 2 || store.size != 10 || !strings.Contains(rec.Body.String(), `"last_page":6`) || !strings.Contains(rec.Body.String(), `"total":51`) {
		t.Fatalf("history = %d %s (node %q page %d size %d)", rec.Code, rec.Body.String(), store.node, store.page, store.size)
	}
	do(h, http.MethodGet, "/api/ip/addresses", "", ownerRead)
	if store.node != "" || store.page != 1 || store.size != 25 {
		t.Fatalf("defaults = %q %d %d", store.node, store.page, store.size)
	}
	if rec := do(h, http.MethodGet, "/api/ip/addresses", "", ownerRead); !strings.Contains(rec.Body.String(), `"last_page":1`) {
		t.Fatalf("an empty list has one page: %s", rec.Body.String())
	}
	for _, q := range []string{"/api/ip/history?node=nope", "/api/ip/map?node=nope", "/api/ip/history?node=" + nodeID + "&node=" + nodeID, "/api/ip/history?page=0", "/api/ip/addresses?size=101"} {
		if rec := do(h, http.MethodGet, q, "", ownerRead); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d, want 422", q, rec.Code)
		}
	}
	do(h, http.MethodGet, "/api/ip/map?node="+nodeID, "", ownerRead)
	if store.node != nodeID {
		t.Fatal("map node filter")
	}
}

func TestOwnerIPLookup(t *testing.T) {
	store := &fakeOwnerIP{geo: map[string]any{"ip": "203.0.113.5", "version": 4, "source": "GeoLite2"}}
	h := ipHandlerFor(store)
	rec := do(h, http.MethodGet, "/api/ip/lookup?ip=203.0.113.5", "", ownerRead)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"source":"GeoLite2"`) || store.ip != "203.0.113.5" {
		t.Fatalf("lookup = %d %s", rec.Code, rec.Body.String())
	}
	store.geo = nil
	rec = do(h, http.MethodGet, "/api/ip/lookup?ip=203.0.113.5", "", ownerRead)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"data":null`) {
		t.Fatalf("an unknown address is data null, not an error: %d %s", rec.Code, rec.Body.String())
	}
	for _, q := range []string{"/api/ip/lookup", "/api/ip/lookup?ip=a&ip=b", "/api/ip/lookup?ip="} {
		if rec := do(h, http.MethodGet, q, "", ownerRead); rec.Code != http.StatusUnprocessableEntity && rec.Code != http.StatusOK {
			t.Errorf("%s = %d", q, rec.Code)
		}
	}
	if rec := do(h, http.MethodGet, "/api/ip/lookup?ip=x1.example", "", ownerRead); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "never resolved") {
		t.Fatalf("host name = %d %s", rec.Code, rec.Body.String())
	}
	store.err = errors.New("provider down at 10.1.2.3")
	rec = do(h, http.MethodGet, "/api/ip/lookup?ip=203.0.113.5", "", ownerRead)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "10.1.2.3") {
		t.Fatalf("failure = %d %s (no internal detail may leak)", rec.Code, rec.Body.String())
	}
	for _, path := range []string{"/api/ip/history", "/api/ip/map"} {
		if rec := do(h, http.MethodGet, path, "", ownerRead); rec.Code != http.StatusInternalServerError {
			t.Errorf("%s with a failing store = %d", path, rec.Code)
		}
	}
}

func TestOwnerIPProviderStatusIsReadOnlyAndSessionProtected(t *testing.T) {
	store := &fakeOwnerIP{providers: []geo.ProviderStatus{{Name: "ipwho", Enabled: true, RequestsToday: 3, DailyBudget: 1000}}}
	h := ipHandlerFor(store)
	if rec := do(h, http.MethodGet, "/api/ip/providers", "", ownerRead); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"requestsToday":3`) || strings.Contains(rec.Body.String(), "token") {
		t.Fatalf("provider status = %d %s", rec.Code, rec.Body.String())
	}
}
