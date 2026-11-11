package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/apitoken"
	"github.com/jooservices/speedtest/internal/center/isp"
)

type fakeOwnerISP struct {
	list, compare, trend, plans []map[string]any
	get, plan                   map[string]any
	err                         error
}

func (f *fakeOwnerISP) List(context.Context, isp.Query) ([]map[string]any, error) {
	return f.list, f.err
}
func (f *fakeOwnerISP) Get(context.Context, string, isp.Query) (map[string]any, error) {
	return f.get, f.err
}
func (f *fakeOwnerISP) Trend(context.Context, string, isp.Query) ([]map[string]any, error) {
	return f.trend, f.err
}
func (f *fakeOwnerISP) Compare(context.Context, isp.Query) ([]map[string]any, error) {
	return f.compare, f.err
}
func (f *fakeOwnerISP) ListPlans(context.Context, string) ([]map[string]any, error) {
	return f.plans, f.err
}
func (f *fakeOwnerISP) CreatePlan(context.Context, string, string, float64, float64) (map[string]any, error) {
	return f.plan, f.err
}
func (f *fakeOwnerISP) UpdatePlan(context.Context, string, string, string, float64, float64) (map[string]any, error) {
	return f.plan, f.err
}
func (f *fakeOwnerISP) DeletePlan(context.Context, string, string) error { return f.err }

func ispHandler(store *fakeOwnerISP) http.Handler {
	return newNodeHandler(Options{OwnerISP: store})
}

func TestOwnerISPRoutesRequireSession(t *testing.T) {
	h := ispHandler(&fakeOwnerISP{list: []map[string]any{}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/isps?range=7d", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestOwnerISPRoutesHonorAccessTokenScopes(t *testing.T) {
	tokens := &tokenAuthStore{principal: apitoken.Principal{TokenID: "isp-token", UserID: "owner-1", Scope: apitoken.ReadOnly}}
	auth := &serverAuth{tokens: tokens, tokenLimiter: newNodeRateLimiter(120, 20)}
	routes := ownerAreas(Options{OwnerISP: &fakeOwnerISP{list: []map[string]any{}}}, auth)
	handler := withAPITokenAuth(dispatcher(routes, http.NotFoundHandler()), routes, auth)

	for _, test := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodGet, "/api/isps?range=7d", "", http.StatusOK},
		{http.MethodPost, "/api/isps/00000000-0000-4000-8000-000000000001/plans", `{"name":"Fiber","advertisedDown":300,"advertisedUp":100}`, http.StatusForbidden},
	} {
		request := httptest.NewRequest(test.method, "https://center.test"+test.path, strings.NewReader(test.body))
		request.Header.Set("Authorization", "Bearer valid-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.want {
			t.Errorf("%s %s status = %d, want %d: %s", test.method, test.path, response.Code, test.want, response.Body.String())
		}
	}
}

func TestOwnerISPListAndCompare(t *testing.T) {
	store := &fakeOwnerISP{
		list:    []map[string]any{{"id": "00000000-0000-4000-8000-000000000001", "name": "Viettel", "asn": 7552, "nodeCount": 1}},
		compare: []map[string]any{{"id": "00000000-0000-4000-8000-000000000001", "name": "Viettel", "asn": 7552, "nodeCount": 1}},
		get:     map[string]any{"id": "00000000-0000-4000-8000-000000000001", "name": "Viettel", "asn": 7552, "nodeCount": 1},
		trend:   []map[string]any{{"ts": time.Now().UTC().Format(time.RFC3339Nano), "n": 1}},
		plans:   []map[string]any{},
	}
	h := ispHandler(store)
	for _, path := range []string{
		"/api/isps?range=7d",
		"/api/isps/compare?range=7d&isps=00000000-0000-4000-8000-000000000001",
		"/api/isps/00000000-0000-4000-8000-000000000001?range=7d",
		"/api/isps/00000000-0000-4000-8000-000000000001/trend?range=7d",
		"/api/isps/00000000-0000-4000-8000-000000000001/plans",
	} {
		rec := do(h, http.MethodGet, path, "", ownerRead)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s -> %d %s", path, rec.Code, rec.Body.String())
		}
		var env map[string]any
		if json.Unmarshal(rec.Body.Bytes(), &env) != nil || env["success"] != true {
			t.Fatalf("envelope %s", rec.Body.String())
		}
	}
}

func TestOwnerISPErrors(t *testing.T) {
	h := ispHandler(&fakeOwnerISP{err: isp.ErrNotFound})
	rec := do(h, http.MethodGet, "/api/isps/00000000-0000-4000-8000-000000000099?range=7d", "", ownerRead)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d", rec.Code)
	}
	h = ispHandler(&fakeOwnerISP{err: isp.ErrInvalid})
	rec = do(h, http.MethodGet, "/api/isps/compare?range=7d&isps=not-a-uuid", "", ownerRead)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code=%d", rec.Code)
	}
	h = ispHandler(&fakeOwnerISP{err: errors.New("db")})
	rec = do(h, http.MethodGet, "/api/isps?range=7d", "", ownerRead)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestOwnerISPRouteMatcher(t *testing.T) {
	if !ownerISPRoute("/api/isps") || !ownerISPRoute("/api/isps/compare") || !ownerISPRoute("/api/isps/x/plans") {
		t.Fatal("expected ISP routes")
	}
	if ownerISPRoute("/api/stats/summary") {
		t.Fatal("stats is not an ISP route")
	}
}

func TestOwnerISPPlansCRUD(t *testing.T) {
	ispID := "00000000-0000-4000-8000-000000000001"
	planID := "00000000-0000-4000-8000-0000000000aa"
	store := &fakeOwnerISP{
		plans: []map[string]any{{"id": planID, "name": "Fiber"}},
		plan:  map[string]any{"id": planID, "name": "Fiber", "advertisedDown": 300.0, "advertisedUp": 100.0},
	}
	h := ispHandler(store)
	body := `{"name":"Fiber","advertisedDown":300,"advertisedUp":100}`
	create := do(h, http.MethodPost, "/api/isps/"+ispID+"/plans", body, ownerWriteWith(nil))
	if create.Code != http.StatusCreated {
		t.Fatalf("create=%d %s", create.Code, create.Body.String())
	}
	upd := do(h, http.MethodPut, "/api/isps/"+ispID+"/plans/"+planID, body, ownerWriteWith(nil))
	if upd.Code != http.StatusOK {
		t.Fatalf("update=%d %s", upd.Code, upd.Body.String())
	}
	del := do(h, http.MethodDelete, "/api/isps/"+ispID+"/plans/"+planID, "", ownerWriteWith(nil))
	if del.Code != http.StatusNoContent {
		t.Fatalf("delete=%d %s", del.Code, del.Body.String())
	}
}

func TestOwnerISPPlanValidationAndErrors(t *testing.T) {
	ispID := "00000000-0000-4000-8000-000000000001"
	planID := "00000000-0000-4000-8000-0000000000aa"
	h := ispHandler(&fakeOwnerISP{plan: map[string]any{"id": planID}})
	if rec := do(h, http.MethodPost, "/api/isps/"+ispID+"/plans", `{"name":"x"}`, ownerWriteWith(nil)); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing speeds=%d", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/api/isps/"+ispID+"/plans", `{bad`, ownerWriteWith(nil)); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad json=%d", rec.Code)
	}
	broken := ispHandler(&fakeOwnerISP{err: isp.ErrInvalid})
	if rec := do(broken, http.MethodPost, "/api/isps/"+ispID+"/plans", `{"name":"x","advertisedDown":1,"advertisedUp":1}`, ownerWriteWith(nil)); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid create=%d", rec.Code)
	}
	if rec := do(broken, http.MethodPut, "/api/isps/"+ispID+"/plans/"+planID, `{"name":"x","advertisedDown":1,"advertisedUp":1}`, ownerWriteWith(nil)); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid update=%d", rec.Code)
	}
	missing := ispHandler(&fakeOwnerISP{err: isp.ErrNotFound})
	if rec := do(missing, http.MethodDelete, "/api/isps/"+ispID+"/plans/"+planID, "", ownerWriteWith(nil)); rec.Code != http.StatusNotFound {
		t.Fatalf("missing delete=%d", rec.Code)
	}
	if rec := do(h, http.MethodPatch, "/api/isps/"+ispID+"/plans", "", ownerWriteWith(nil)); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("plans patch=%d", rec.Code)
	}
	if rec := do(h, http.MethodPatch, "/api/isps/"+ispID+"/plans/"+planID, "", ownerWriteWith(nil)); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("plan patch=%d", rec.Code)
	}
}

func TestOwnerISPPlanSchemaBoundaries(t *testing.T) {
	const ispID = "00000000-0000-4000-8000-000000000001"
	const planID = "00000000-0000-4000-8000-0000000000aa"
	h := ispHandler(&fakeOwnerISP{plan: map[string]any{"id": planID}})
	path := "/api/isps/" + ispID + "/plans"
	item := path + "/" + planID
	valid := func(name string) string { return `{"name":"` + name + `","advertisedDown":300,"advertisedUp":100}` }
	for method, target := range map[string]string{http.MethodPost: path, http.MethodPut: item} {
		if got := do(h, method, target, valid(strings.Repeat("x", 200)), ownerWriteWith(nil)).Code; got != map[string]int{http.MethodPost: http.StatusCreated, http.MethodPut: http.StatusOK}[method] {
			t.Errorf("%s accepts 200-rune name: %d", method, got)
		}
		for label, body := range map[string]string{
			"unknown property": valid("x")[:len(valid("x"))-1] + `,"unexpected":true}`,
			"long name":        valid(strings.Repeat("x", 201)),
		} {
			if got := do(h, method, target, body, ownerWriteWith(nil)).Code; got != http.StatusUnprocessableEntity {
				t.Errorf("%s %s -> %d, want 422", method, label, got)
			}
		}
	}
}

func TestOwnerISPCompareAndResourceEdges(t *testing.T) {
	h := ispHandler(&fakeOwnerISP{compare: []map[string]any{}})
	if rec := do(h, http.MethodGet, "/api/isps/compare?range=7d", "", ownerRead); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty isps=%d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/api/isps/compare?range=7d&isps=,,,", "", ownerRead); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("blank isps=%d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/api/isps/not-a-uuid", "", ownerRead); rec.Code != http.StatusNotFound {
		t.Fatalf("bad id=%d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/api/isps/00000000-0000-4000-8000-000000000001/other", "", ownerRead); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown sub=%d", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/api/isps", "", ownerWriteWith(nil)); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post list=%d", rec.Code)
	}
	store := &fakeOwnerISP{trend: []map[string]any{}}
	h = ispHandler(store)
	if rec := do(h, http.MethodGet, "/api/isps/00000000-0000-4000-8000-000000000001/trend?range=7d", "", ownerRead); rec.Code != http.StatusOK {
		t.Fatalf("trend=%d", rec.Code)
	}
	store.err = errors.New("db")
	if rec := do(h, http.MethodGet, "/api/isps/00000000-0000-4000-8000-000000000001/trend?range=7d", "", ownerRead); rec.Code != http.StatusInternalServerError {
		t.Fatalf("trend err=%d", rec.Code)
	}
	store.err = isp.ErrNotFound
	if rec := do(h, http.MethodGet, "/api/isps/00000000-0000-4000-8000-000000000001/plans", "", ownerRead); rec.Code != http.StatusNotFound {
		t.Fatalf("plans missing=%d", rec.Code)
	}
}
