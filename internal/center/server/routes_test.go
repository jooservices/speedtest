package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRouteTableOrderNilAreasAndFallback(t *testing.T) {
	var hits []string
	mark := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits = append(hits, name); w.WriteHeader(204) })
	}
	var table routeTable
	table.add(nil, onPath(func(string) bool { return true })) // an area without a store is left out
	table.add(mark("specific"), onPath(func(p string) bool { return p == "/api/x/special" }))
	table.add(mark("general"), onPath(func(p string) bool { return p == "/api/x/special" || p == "/api/x/other" }))
	if len(table) != 2 {
		t.Fatalf("a nil handler was registered: %d routes", len(table))
	}
	ui := mark("ui")
	h := dispatcher(table, ui)
	for _, tc := range []struct{ path, want string }{{"/api/x/special", "specific"}, {"/api/x/other", "general"}, {"/index.html", "ui"}} {
		hits = nil
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, tc.path, nil))
		if len(hits) != 1 || hits[0] != tc.want {
			t.Errorf("%s served by %v, want %s", tc.path, hits, tc.want)
		}
	}
	if table.handlerFor(httptest.NewRequest(http.MethodGet, "/nothing", nil)) != nil {
		t.Fatal("no route should match")
	}
}

func TestAccessTokenRouteMatchesOnlyItsCollection(t *testing.T) {
	for path, want := range map[string]bool{
		"/api/access-tokens": true,
		"/api/access-tokens/123e4567-e89b-12d3-a456-426614174000": true,
		"/api/access-tokens-extra":                                false,
		"/api/other":                                              false,
	} {
		if got := accessTokenRoute(path); got != want {
			t.Errorf("accessTokenRoute(%q) = %t, want %t", path, got, want)
		}
	}
}

func TestOwnerReaderGuardsRunInOrder(t *testing.T) {
	called := 0
	next := func(http.ResponseWriter, *http.Request) { called++ }
	guarded := ownerGuards(newNodeRateLimiter(60, 2), next)
	if rec := do(guarded, http.MethodGet, "/x", "", nil); rec.Code != http.StatusUnauthorized || called != 0 {
		t.Fatalf("without an owner: %d", rec.Code)
	}
	owner := func(r *http.Request) { *r = *r.WithContext(withOwner(r)) }
	if rec := do(guarded, http.MethodPost, "/x", "", owner); rec.Code != http.StatusMethodNotAllowed || called != 0 {
		t.Fatalf("POST: %d", rec.Code)
	}
	// The POST and the first GET use the burst of 2, the next is rate limited, and the handler ran once.
	if rec := do(guarded, http.MethodGet, "/x", "", owner); rec.Code != http.StatusOK || called != 1 {
		t.Fatalf("GET: %d called %d", rec.Code, called)
	}
	if rec := do(guarded, http.MethodGet, "/x", "", owner); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second GET = %d, want 429", rec.Code)
	}
}

func withOwner(r *http.Request) context.Context {
	return context.WithValue(r.Context(), ownerIdentityContextKey{}, "test-owner")
}
