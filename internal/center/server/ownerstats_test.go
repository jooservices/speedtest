package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/stats"
)

type fakeOwnerStats struct {
	query   stats.Query
	queries map[string]stats.Query
	nodeID  string
	calls   map[string]int
	err     error
	heatmap stats.Heatmap
}

func (f *fakeOwnerStats) Summary(_ context.Context, q stats.Query) ([]stats.SummaryComparison, error) {
	f.query = q
	f.count("summary")
	f.queries["summary"] = q
	return []stats.SummaryComparison{}, f.err
}

func (f *fakeOwnerStats) Trend(_ context.Context, q stats.Query) ([]stats.Series, error) {
	f.query = q
	f.count("trend")
	f.queries["trend"] = q
	return []stats.Series{}, f.err
}

func (f *fakeOwnerStats) HourOfDay(_ context.Context, q stats.Query) ([]stats.HourSeries, error) {
	f.query = q
	f.count("hour-of-day")
	f.queries["hour-of-day"] = q
	return []stats.HourSeries{}, f.err
}

func (f *fakeOwnerStats) Heatmap(_ context.Context, q stats.Query, nodeID string) (stats.Heatmap, error) {
	f.query, f.nodeID = q, nodeID
	f.count("heatmap")
	f.queries["heatmap"] = q
	return f.heatmap, f.err
}

func (f *fakeOwnerStats) Distribution(_ context.Context, q stats.Query) ([]stats.Distribution, error) {
	f.query = q
	f.count("distribution")
	f.queries["distribution"] = q
	return []stats.Distribution{}, f.err
}

func (f *fakeOwnerStats) count(name string) {
	if f.calls == nil {
		f.calls = make(map[string]int)
	}
	if f.queries == nil {
		f.queries = make(map[string]stats.Query)
	}
	f.calls[name]++
}

func statsHandlerFor(store *fakeOwnerStats) http.Handler {
	return newNodeHandler(Options{OwnerStats: store})
}

func TestOwnerStatsEndpointsRequireSession(t *testing.T) {
	h := statsHandlerFor(&fakeOwnerStats{})
	for _, path := range []string{"/api/stats/summary", "/api/stats/trend", "/api/stats/hour-of-day", "/api/stats/heatmap", "/api/stats/distribution"} {
		if rec := do(h, http.MethodGet, path, "", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a session = %d, want 401", path, rec.Code)
		}
	}
}

func TestOwnerStatsRoutesParseAndDispatch(t *testing.T) {
	store := &fakeOwnerStats{heatmap: stats.Heatmap{NodeID: nodeID, Metric: stats.Download, Timezone: "UTC", Cells: []stats.HeatCell{}}}
	h := statsHandlerFor(store)
	from, to := "2026-11-01T00:00:00Z", "2026-11-02T00:00:00Z"
	cases := []struct {
		path, query, method, name string
	}{
		{"summary", "?from=" + from + "&to=" + to + "&nodes=" + nodeID, http.MethodGet, "summary"},
		{"trend", "?from=" + from + "&to=" + to + "&nodes=" + nodeID + "&metric=up&timezone=Asia%2FHo_Chi_Minh&bucket=hour&combined=true", http.MethodGet, "trend"},
		{"hour-of-day", "?range=24h&metric=ping&timezone=UTC", http.MethodGet, "hour-of-day"},
		{"heatmap", "?node=" + nodeID + "&metric=loss&timezone=UTC", http.MethodGet, "heatmap"},
		{"distribution", "?from=" + from + "&to=" + to + "&bins=5&metric=jitter", http.MethodGet, "distribution"},
	}
	for _, tc := range cases {
		rec := do(h, tc.method, "/api/stats/"+tc.path+tc.query, "", ownerRead)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", tc.path, rec.Code, rec.Body.String())
		}
		if store.calls[tc.name] != 1 {
			t.Errorf("%s store calls = %d", tc.name, store.calls[tc.name])
		}
	}
	if store.queries["distribution"].Metric != stats.Jitter || store.queries["distribution"].Bins != 5 {
		t.Fatalf("distribution query = %+v", store.query)
	}
	if store.nodeID != nodeID || store.queries["heatmap"].Metric != stats.Loss || store.queries["heatmap"].To.Sub(store.queries["heatmap"].From) != 30*24*time.Hour {
		t.Fatalf("heatmap query node=%s window=%s metric=%s", store.nodeID, store.queries["heatmap"].To.Sub(store.queries["heatmap"].From), store.queries["heatmap"].Metric)
	}
	rec := do(h, http.MethodGet, "/api/stats/summary?nodes="+strings.ToUpper(nodeID)+",,"+nodeID, "", ownerRead)
	if rec.Code != http.StatusOK || len(store.query.NodeIDs) != 1 || store.query.NodeIDs[0] != nodeID {
		t.Fatalf("duplicate and empty node IDs = %d %+v", rec.Code, store.query.NodeIDs)
	}
	rec = do(h, http.MethodGet, "/api/stats/trend?range=6h&combined=false", "", ownerRead)
	if rec.Code != http.StatusOK || store.query.Bucket != "day" || store.query.Combined {
		t.Fatalf("default trend bucket or false combined = %d %+v", rec.Code, store.query)
	}
	if wrong := do(h, http.MethodPost, "/api/stats/summary", "", ownerRead); wrong.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST summary = %d, want 405", wrong.Code)
	}
}

func TestOwnerStatsRouteAndDirectServeGuards(t *testing.T) {
	for _, path := range []string{"/api/stats/summary", "/api/stats/trend", "/api/stats/hour-of-day", "/api/stats/heatmap", "/api/stats/distribution"} {
		if !ownerStatsRoute(path) {
			t.Errorf("ownerStatsRoute(%q) = false", path)
		}
	}
	if ownerStatsRoute("/api/stats/unknown") {
		t.Fatal("unknown path matched an owner statistics route")
	}

	h := &ownerStats{store: &fakeOwnerStats{}, limiter: newNodeRateLimiter(60, 10), now: time.Now}
	serve := ownerGuards(h.limiter, h.serve)
	directOwner := func(r *http.Request) {
		*r = *r.WithContext(context.WithValue(r.Context(), ownerIdentityContextKey{}, "test-owner"))
	}
	if rec := do(serve, http.MethodGet, "/api/stats/summary", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("direct unauthenticated request = %d", rec.Code)
	}
	if rec := do(serve, http.MethodPost, "/api/stats/summary", "", directOwner); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("direct POST = %d", rec.Code)
	}
	if rec := do(serve, http.MethodGet, "/api/stats/unknown", "", directOwner); rec.Code != http.StatusNotFound {
		t.Fatalf("direct unknown route = %d", rec.Code)
	}
	if rec := do(serve, http.MethodGet, "/api/stats/summary", "", directOwner); rec.Code != http.StatusOK {
		t.Fatalf("first limited request = %d", rec.Code)
	}
	limitedStats := &ownerStats{store: &fakeOwnerStats{}, limiter: newNodeRateLimiter(60, 1), now: time.Now}
	limited := ownerGuards(limitedStats.limiter, limitedStats.serve)
	if rec := do(limited, http.MethodGet, "/api/stats/summary", "", directOwner); rec.Code != http.StatusOK {
		t.Fatalf("first rate-limited request = %d", rec.Code)
	}
	if rec := do(limited, http.MethodGet, "/api/stats/summary", "", directOwner); rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("rate-limited request = %d, retry-after=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
}

func TestOwnerStatsSummaryBuildsAdjacentPeriodsAndEmptyNodeFilter(t *testing.T) {
	store := &fakeOwnerStats{}
	h := statsHandlerFor(store)
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(48 * time.Hour)
	path := "/api/stats/summary?from=" + from.Format(time.RFC3339) + "&to=" + to.Format(time.RFC3339)
	rec := do(h, http.MethodGet, path, "", ownerRead)
	if rec.Code != http.StatusOK {
		t.Fatalf("summary = %d %s", rec.Code, rec.Body.String())
	}
	if !store.query.From.Equal(from) || !store.query.To.Equal(to) || !store.query.PreviousFrom.Equal(from.Add(-48*time.Hour)) || !store.query.PreviousTo.Equal(from) {
		t.Fatalf("summary periods = %+v", store.query)
	}
	rec = do(h, http.MethodGet, "/api/stats/summary?nodes=", "", ownerRead)
	if rec.Code != http.StatusOK || !store.query.NodesSet || len(store.query.NodeIDs) != 0 {
		t.Fatalf("empty node filter = %d %+v", rec.Code, store.query)
	}
}

func TestOwnerStatsValidationStopsBeforeStore(t *testing.T) {
	longNodes := make([]string, 101)
	for i := range longNodes {
		longNodes[i] = fmt.Sprintf("00000000-0000-4000-8000-%012x", i+1)
	}
	tooManyDays := "2025-10-01T00:00:00Z"
	cases := []struct{ name, path string }{
		{"unpaired range", "/api/stats/summary?from=2026-11-01T00:00:00Z"},
		{"repeated from", "/api/stats/summary?from=2026-11-01T00:00:00Z&from=2026-11-02T00:00:00Z&to=2026-11-03T00:00:00Z"},
		{"repeated to", "/api/stats/summary?from=2026-11-01T00:00:00Z&to=2026-11-02T00:00:00Z&to=2026-11-03T00:00:00Z"},
		{"repeated range", "/api/stats/summary?range=6h&range=7d"},
		{"range with explicit dates", "/api/stats/summary?from=2026-11-01T00:00:00Z&to=2026-11-02T00:00:00Z&range=7d"},
		{"invalid from timestamp", "/api/stats/summary?from=bad&to=2026-11-02T00:00:00Z"},
		{"invalid to timestamp", "/api/stats/summary?from=2026-11-01T00:00:00Z&to=bad"},
		{"reversed dates", "/api/stats/summary?from=2026-11-02T00:00:00Z&to=2026-11-01T00:00:00Z"},
		{"range over 366 days", "/api/stats/summary?from=" + tooManyDays + "&to=2026-11-03T00:00:00Z"},
		{"unsupported range", "/api/stats/summary?range=90d"},
		{"invalid node", "/api/stats/summary?nodes=not-a-uuid"},
		{"too many nodes", "/api/stats/summary?nodes=" + strings.Join(longNodes, ",")},
		{"repeated trend nodes", "/api/stats/trend?nodes=" + nodeID + "&nodes=" + nodeID},
		{"invalid trend range", "/api/stats/trend?range=90d"},
		{"invalid metric", "/api/stats/trend?metric=throughput"},
		{"repeated trend metric", "/api/stats/trend?metric=down&metric=up"},
		{"invalid timezone", "/api/stats/trend?timezone=Not/AZone"},
		{"repeated trend timezone", "/api/stats/trend?timezone=UTC&timezone=Asia/Tokyo"},
		{"invalid bucket", "/api/stats/trend?bucket=week"},
		{"repeated bucket", "/api/stats/trend?bucket=hour&bucket=day"},
		{"invalid combined", "/api/stats/trend?combined=yes"},
		{"repeated combined", "/api/stats/trend?combined=true&combined=false"},
		{"combined latency", "/api/stats/trend?combined=true&metric=ping"},
		{"too many trend buckets", "/api/stats/trend?from=2026-01-01T00:00:00Z&to=2026-11-01T00:00:00Z"},
		{"invalid hour range", "/api/stats/hour-of-day?range=90d"},
		{"invalid hour nodes", "/api/stats/hour-of-day?nodes=not-a-uuid"},
		{"repeated hour metric", "/api/stats/hour-of-day?metric=down&metric=up"},
		{"invalid hour timezone", "/api/stats/hour-of-day?timezone=Not/AZone"},
		{"repeated hour timezone", "/api/stats/hour-of-day?timezone=UTC&timezone=Asia/Tokyo"},
		{"missing heatmap node", "/api/stats/heatmap"},
		{"invalid heatmap metric", "/api/stats/heatmap?metric=throughput"},
		{"invalid heatmap timezone", "/api/stats/heatmap?timezone=Not/AZone"},
		{"invalid heatmap node", "/api/stats/heatmap?node=bad"},
		{"repeated heatmap node", "/api/stats/heatmap?node=" + nodeID + "&node=" + nodeID},
		{"repeated distribution nodes", "/api/stats/distribution?nodes=" + nodeID + "&nodes=" + nodeID},
		{"invalid distribution range", "/api/stats/distribution?range=90d"},
		{"repeated distribution metric", "/api/stats/distribution?metric=down&metric=up"},
		{"invalid distribution timezone", "/api/stats/distribution?timezone=Not/AZone"},
		{"repeated distribution timezone", "/api/stats/distribution?timezone=UTC&timezone=Asia/Tokyo"},
		{"invalid bins", "/api/stats/distribution?bins=0"},
		{"invalid bins integer", "/api/stats/distribution?bins=1.5"},
		{"too many bins", "/api/stats/distribution?bins=101"},
		{"repeated bins", "/api/stats/distribution?bins=5&bins=6"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeOwnerStats{}
			rec := do(statsHandlerFor(store), http.MethodGet, tc.path, "", ownerRead)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("status = %d, want 422: %s", rec.Code, rec.Body.String())
			}
			if len(store.calls) != 0 {
				t.Errorf("invalid request reached store: %v", store.calls)
			}
		})
	}
}

func TestOwnerStatsStoreErrorsAreSafe(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code int
	}{
		{"internal", errors.New("password should not be exposed"), http.StatusInternalServerError},
		{"missing node", stats.ErrNotFound, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(statsHandlerFor(&fakeOwnerStats{err: tc.err}), http.MethodGet, "/api/stats/heatmap?node="+nodeID, "", ownerRead)
			if rec.Code != tc.code || strings.Contains(rec.Body.String(), "password") {
				t.Fatalf("error response = %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestEveryOwnerStatsEndpointHandlesStoreErrors(t *testing.T) {
	from, to := "2026-11-01T00:00:00Z", "2026-11-02T00:00:00Z"
	for _, path := range []string{
		"/api/stats/summary?from=" + from + "&to=" + to,
		"/api/stats/trend?from=" + from + "&to=" + to,
		"/api/stats/hour-of-day?from=" + from + "&to=" + to,
		"/api/stats/heatmap?node=" + nodeID,
		"/api/stats/distribution?from=" + from + "&to=" + to,
	} {
		t.Run(path, func(t *testing.T) {
			rec := do(statsHandlerFor(&fakeOwnerStats{err: errors.New("database details must not be returned")}), http.MethodGet, path, "", ownerRead)
			if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "database details") {
				t.Fatalf("store error response = %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestCalendarBucketLimitUsesLocalWallTime(t *testing.T) {
	location := "America/New_York"
	fallStart := time.Date(2026, time.November, 1, 4, 0, 0, 0, time.UTC)
	fallEnd := time.Date(2026, time.November, 1, 8, 0, 0, 0, time.UTC)
	if got := countCalendarBuckets(fallStart, fallEnd, "hour", location); got != 3 {
		t.Errorf("fall DST local buckets = %d, want 3 (repeated hour merges)", got)
	}
	springStart := time.Date(2026, time.March, 8, 5, 0, 0, 0, time.UTC)
	springEnd := time.Date(2026, time.March, 8, 9, 0, 0, 0, time.UTC)
	if got := countCalendarBuckets(springStart, springEnd, "hour", location); got != 4 {
		t.Errorf("spring DST local buckets = %d, want 4 (missing hour omitted)", got)
	}
	dstLocation, _ := time.LoadLocation(location)
	dayStart := time.Date(2026, time.March, 8, 0, 0, 0, 0, dstLocation)
	dayEnd := dayStart.AddDate(0, 0, 1)
	if got := countCalendarBuckets(dayStart, dayEnd, "day", location); got != 1 {
		t.Errorf("spring DST local days = %d, want 1 calendar day", got)
	}
	if got := countCalendarBuckets(dayStart, dayEnd, "hour", "Not/AZone"); got != 0 {
		t.Errorf("invalid timezone buckets = %d, want 0", got)
	}
}

func TestCanonicalZoneMapsLegacyAliasesToPostgresNames(t *testing.T) {
	for alias, want := range map[string]string{"Asia/Saigon": "Asia/Ho_Chi_Minh", "Asia/Calcutta": "Asia/Kolkata", "Europe/Kiev": "Europe/Kyiv", "UTC": "UTC", "Asia/Ho_Chi_Minh": "Asia/Ho_Chi_Minh"} {
		if got := canonicalZone(alias); got != want {
			t.Errorf("canonicalZone(%q) = %q, want %q", alias, got, want)
		}
	}
	for alias, canonical := range zoneAliases {
		if _, err := time.LoadLocation(canonical); err != nil {
			t.Errorf("alias %q targets %q which Go cannot load", alias, canonical)
		}
	}
}
