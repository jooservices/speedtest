package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/dashboard"
	"github.com/jooservices/speedtest/internal/center/stats"
)

type fakeOwnerDashboard struct {
	query     stats.Query
	percent   bool
	maxPoints int
	err       error
}

func (f *fakeOwnerDashboard) Snapshot(_ context.Context, q stats.Query) (dashboard.Snapshot, error) {
	f.query = q
	return dashboard.Snapshot{Nodes: []dashboard.NodeCard{}}, f.err
}
func (f *fakeOwnerDashboard) Series(_ context.Context, q stats.Query, percent bool, maxPoints int) ([]stats.Series, error) {
	f.query, f.percent, f.maxPoints = q, percent, maxPoints
	return []stats.Series{}, f.err
}

func dashboardHandlerFor(store OwnerDashboardStore) http.Handler {
	return newNodeHandler(Options{OwnerDashboard: store, NodeCredentials: &fakeNodeCredentialService{}})
}

func TestOwnerDashboardNeedsASessionAndGet(t *testing.T) {
	h := dashboardHandlerFor(&fakeOwnerDashboard{})
	for _, path := range []string{"/api/dashboard", "/api/series"} {
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
}

func TestOwnerDashboardWindow(t *testing.T) {
	store := &fakeOwnerDashboard{}
	h := dashboardHandlerFor(store)
	do(h, http.MethodGet, "/api/dashboard?range=24h", "", ownerRead)
	if d := store.query.To.Sub(store.query.From); d != 24*time.Hour || store.query.PreviousTo != store.query.From || store.query.From.Sub(store.query.PreviousFrom) != d {
		t.Fatalf("24h window = %+v", store.query)
	}
	do(h, http.MethodGet, "/api/dashboard", "", ownerRead)
	if store.query.To.Sub(store.query.From) != 7*24*time.Hour {
		t.Fatalf("default window = %+v", store.query)
	}
	for _, q := range []string{"range=1y", "range=24h&from=2026-11-01T00:00:00Z&to=2026-11-02T00:00:00Z", "from=2026-11-01T00:00:00Z"} {
		if rec := do(h, http.MethodGet, "/api/dashboard?"+q, "", ownerRead); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d, want 422", q, rec.Code)
		}
	}
	if rec := do(h, http.MethodGet, "/api/dashboard", "", ownerRead); strings.Contains(rec.Body.String(), `"nodes":null`) {
		t.Fatal("an empty fleet has empty arrays, never null")
	}
}

func TestOwnerSeriesParameters(t *testing.T) {
	store := &fakeOwnerDashboard{}
	h := dashboardHandlerFor(store)
	rec := do(h, http.MethodGet, "/api/series?nodes="+nodeID+"&metric=up&mode=percent&maxPoints=48&timezone=Asia/Ho_Chi_Minh&range=6h", "", ownerRead)
	if rec.Code != http.StatusOK || !store.percent || store.maxPoints != 48 || store.query.Metric != stats.Upload || !store.query.NodesSet || len(store.query.NodeIDs) != 1 {
		t.Fatalf("series = %d %+v percent %v max %d", rec.Code, store.query, store.percent, store.maxPoints)
	}
	do(h, http.MethodGet, "/api/series", "", ownerRead)
	if store.percent || store.maxPoints != 300 || store.query.Metric != stats.Download || store.query.NodesSet {
		t.Fatalf("defaults = %+v percent %v max %d", store.query, store.percent, store.maxPoints)
	}
	for _, q := range []string{"mode=relative", "metric=ping&mode=percent", "metric=bogus", "maxPoints=0", "maxPoints=301", "maxPoints=x", "nodes=nope", "timezone=Mars/Base", "mode=percent&mode=absolute"} {
		if rec := do(h, http.MethodGet, "/api/series?"+q, "", ownerRead); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d, want 422", q, rec.Code)
		}
	}
	for path, want := range map[string]bool{"/api/dashboard": true, "/api/series": true, "/api/series/x": false} {
		if ownerDashboardRoute(path) != want {
			t.Errorf("route %s", path)
		}
	}
}

func TestOwnerDashboardStoreFailureDoesNotLeak(t *testing.T) {
	h := dashboardHandlerFor(&fakeOwnerDashboard{err: errors.New("db down")})
	for _, path := range []string{"/api/dashboard", "/api/series"} {
		rec := do(h, http.MethodGet, path, "", ownerRead)
		if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "db down") {
			t.Errorf("%s = %d %s", path, rec.Code, rec.Body.String())
		}
	}
}
