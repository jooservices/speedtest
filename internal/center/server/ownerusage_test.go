package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/usage"
)

type fakeOwnerUsage struct {
	query       usage.Query
	budget      usage.Budget
	scope       string
	engine      string
	ifMatch     string
	interval    int
	budgetBytes int64
	call        string
	err         error
}

func (f *fakeOwnerUsage) Usage(_ context.Context, q usage.Query) (usage.Response, error) {
	f.query, f.call = q, "usage"
	return usage.Response{Group: q.Group}, f.err
}
func (f *fakeOwnerUsage) Budgets(context.Context) ([]usage.Budget, string, error) {
	f.call = "budgets"
	return []usage.Budget{}, `"usage-budgets-test"`, f.err
}
func (f *fakeOwnerUsage) ReplaceBudget(_ context.Context, b usage.Budget, tag string) ([]usage.Budget, string, error) {
	f.budget, f.ifMatch, f.call = b, tag, "replace"
	return []usage.Budget{b}, `"usage-budgets-next"`, f.err
}
func (f *fakeOwnerUsage) DeleteBudget(_ context.Context, scope, tag string) ([]usage.Budget, string, error) {
	f.scope, f.ifMatch, f.call = scope, tag, "delete"
	return []usage.Budget{}, `"usage-budgets-next"`, f.err
}
func (f *fakeOwnerUsage) Advice(_ context.Context, node, engine string, interval int, budget int64) (usage.Advice, error) {
	f.scope, f.engine, f.interval, f.budgetBytes, f.call = node, engine, interval, budget, "advice"
	return usage.Advice{NodeID: node, Engine: engine, IntervalMinutes: interval, BudgetBytes: budget}, f.err
}

func usageHandlerFor(store OwnerUsageStore) http.Handler {
	return newNodeHandler(Options{OwnerUsage: store, NodeCredentials: &fakeNodeCredentialService{}})
}

func TestOwnerUsageAuthenticationAndRoutes(t *testing.T) {
	store := &fakeOwnerUsage{}
	h := usageHandlerFor(store)
	for _, path := range []string{"/api/usage", "/api/usage/budgets", "/api/usage/advice?node=" + nodeID + "&engine=ookla&interval=60"} {
		if rec := do(h, http.MethodGet, path, "", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a session = %d", path, rec.Code)
		}
	}
	for _, path := range []string{"/api/usage", "/api/usage/advice", "/api/usage/budgets/global"} {
		if !ownerUsageRoute(path) {
			t.Errorf("route not matched: %s", path)
		}
	}
	if ownerUsageRoute("/api/usage/budgets/global/extra") || ownerUsageRoute("/api/usage/unknown") {
		t.Fatal("invalid path matched usage area")
	}
	ispID := "00000000-0000-4000-8000-000000000001"
	if rec := do(h, http.MethodGet, "/api/usage?engine=librespeed&group=week&node="+nodeID+"&isp="+ispID+"&from=2026-11-01T00:00:00Z&to=2026-11-02T00:00:00Z", "", ownerRead); rec.Code != http.StatusOK || store.query.Group != "week" || store.query.Engine != "librespeed" || store.query.NodeID != nodeID || store.query.ISPID != ispID {
		t.Fatalf("usage filters = %d %+v", rec.Code, store.query)
	}
	if rec := do(h, http.MethodGet, "/api/usage/budgets", "", ownerRead); rec.Code != http.StatusOK || rec.Header().Get("ETag") != `"usage-budgets-test"` {
		t.Fatalf("budgets response = %d %q", rec.Code, rec.Header().Get("ETag"))
	}
	if rec := do(h, http.MethodPost, "/api/usage", "", ownerRead); rec.Code != http.StatusForbidden && rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST usage = %d", rec.Code)
	}
}

func TestOwnerUsageFilterAndAdviceValidation(t *testing.T) {
	store := &fakeOwnerUsage{}
	h := usageHandlerFor(store)
	for _, path := range []string{
		"/api/usage?node=invalid", "/api/usage?isp=invalid", "/api/usage?engine=ookla&engine=librespeed", "/api/usage?from=2026-11-01T00:00:00Z",
		"/api/usage?from=bad&to=2026-11-02T00:00:00Z", "/api/usage?from=2026-11-02T00:00:00Z&to=2026-11-01T00:00:00Z",
		"/api/usage?group=year", "/api/usage?to=2026-11-02T00:00:00Z&to=2026-11-03T00:00:00Z",
		"/api/usage/advice?node=" + nodeID + "&engine=ookla&interval=x", "/api/usage/advice?node=" + nodeID + "&engine=ookla&interval=60&budgetBytes=-1",
		"/api/usage/advice?node=" + nodeID + "&engine=ookla&engine=librespeed&interval=60",
	} {
		if rec := do(h, http.MethodGet, path, "", ownerRead); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("invalid query %s = %d", path, rec.Code)
		}
	}
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(367 * 24 * time.Hour)
	path := "/api/usage?from=" + from.Format(time.RFC3339) + "&to=" + to.Format(time.RFC3339)
	if rec := do(h, http.MethodGet, path, "", ownerRead); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("oversized range = %d", rec.Code)
	}
	path = "/api/usage/advice?node=" + nodeID + "&engine=librespeed&interval=30&budgetBytes=123456"
	if rec := do(h, http.MethodGet, path, "", ownerRead); rec.Code != http.StatusOK || store.call != "advice" || store.interval != 30 || store.budgetBytes != 123456 || store.engine != "librespeed" {
		t.Fatalf("advice request = %d %+v", rec.Code, store)
	}
}

func TestOwnerUsageBudgetPreconditionsAndWrites(t *testing.T) {
	store := &fakeOwnerUsage{}
	h := usageHandlerFor(store)
	path := "/api/usage/budgets/" + nodeID
	body := `{"monthlyBytes":1000000,"action":"widen","widenMultiplier":2}`
	if rec := do(h, http.MethodPut, path, body, ownerWriteWith(nil)); rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("missing If-Match = %d", rec.Code)
	}
	if rec := do(h, http.MethodPut, path, body, ownerWriteWith(map[string]string{"If-Match": `"before"`})); rec.Code != http.StatusOK || store.budget.NodeID != nodeID || store.ifMatch != `"before"` || rec.Header().Get("ETag") == "" {
		t.Fatalf("node budget PUT = %d %+v", rec.Code, store)
	}
	if rec := do(h, http.MethodPut, "/api/usage/budgets/global", `{"monthlyBytes":100,"action":"pause"}`, ownerWriteWith(map[string]string{"If-Match": `"before"`})); rec.Code != http.StatusOK || store.budget.NodeID != "" {
		t.Fatalf("global budget PUT = %d %+v", rec.Code, store.budget)
	}
	if rec := do(h, http.MethodPut, path, `{bad}`, ownerWriteWith(map[string]string{"If-Match": `"before"`})); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed budget = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, "/api/usage/budgets/global", "", ownerWriteWith(map[string]string{"If-Match": `"before"`})); rec.Code != http.StatusNoContent || store.scope != "global" {
		t.Fatalf("budget delete = %d", rec.Code)
	}
	for _, tc := range []struct {
		err  error
		want int
	}{{usage.ErrInvalid, 422}, {usage.ErrNotFound, 404}, {usage.ErrPrecondition, 428}, {usage.ErrStale, 412}, {errors.New("database failure"), 500}} {
		store.err = tc.err
		rec := do(h, http.MethodPut, path, body, ownerWriteWith(map[string]string{"If-Match": `"before"`}))
		if rec.Code != tc.want {
			t.Errorf("store error %v = %d, want %d", tc.err, rec.Code, tc.want)
		}
	}
	store.err = nil
	if rec := do(h, http.MethodPut, path, body, ownerWriteWith(map[string]string{"If-Match": `"before"`, "X-CSRF-Token": "bad"})); rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Errorf("write without valid CSRF = %d", rec.Code)
	}
}

func TestOwnerUsageMethodAndStoreErrors(t *testing.T) {
	methods := []struct{ path, method string }{
		{"/api/usage", http.MethodPost}, {"/api/usage/advice", http.MethodPost},
		{"/api/usage/budgets", http.MethodPost}, {"/api/usage/budgets/invalid", http.MethodGet},
		{"/api/usage/budgets/" + nodeID, http.MethodGet},
	}
	for _, tc := range methods {
		if rec := do(usageHandlerFor(&fakeOwnerUsage{}), tc.method, tc.path, "", ownerWriteWith(nil)); rec.Code != http.StatusMethodNotAllowed &&
			(tc.path != "/api/usage/budgets/invalid" || rec.Code != http.StatusNotFound) {
			t.Errorf("%s %s = %d", tc.method, tc.path, rec.Code)
		}
	}
	for _, path := range []string{
		"/api/usage?group=day",
		"/api/usage/advice?node=" + nodeID + "&engine=ookla&interval=60",
		"/api/usage/budgets",
	} {
		if rec := do(usageHandlerFor(&fakeOwnerUsage{err: errors.New("database unavailable")}), http.MethodGet, path, "", ownerRead); rec.Code != http.StatusInternalServerError {
			t.Errorf("store error for %s = %d", path, rec.Code)
		}
	}
}

func TestWriteUsageErrorMappings(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{{usage.ErrInvalid, 422}, {usage.ErrNotFound, 404}, {usage.ErrPrecondition, 428}, {usage.ErrStale, 412}, {errors.New("unknown"), 500}} {
		rec := do(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeUsageError(w, r, tc.err) }), http.MethodGet, "/", "", nil)
		if rec.Code != tc.want {
			t.Errorf("error %v status=%d want=%d", tc.err, rec.Code, tc.want)
		}
	}
}
