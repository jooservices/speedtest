//go:build integration

package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/auth"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/nodes"
	"github.com/jooservices/speedtest/internal/center/usage"
)

type ownerUsageCaller func(string, string, string, map[string]string, bool) *httptest.ResponseRecorder

func TestOwnerUsageHTTPFlowIntegration(t *testing.T) {
	call, node := setupOwnerUsageHTTP(t)
	verifyUsageReads(t, call)
	etag := verifyBudgetWrites(t, call)
	verifyUsageAdvice(t, call, node)
	verifyBudgetDelete(t, call, etag)
	t.Log("PASS: authenticated usage/advice reads, validation, global budget create with ETag, stale-write rejection and deletion over HTTP/PostgreSQL")
}

func setupOwnerUsageHTTP(t *testing.T) (ownerUsageCaller, string) {
	pool, ctx, appKey := openServerIntegrationPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	password, err := auth.HashPassword("integration test owner password")
	if err != nil {
		t.Fatal("could not prepare integration owner")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,email,password_hash) VALUES('00000000-0000-4000-8000-000000000901','owner@example.test',$1)`, password); err != nil {
		t.Fatal(err)
	}
	authStore, err := auth.NewPostgresStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	authService, err := auth.NewService(authStore, auth.Config{SessionTTL: time.Hour, LoginMaxAttempts: 5, LoginWindow: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	const node = "00000000-0000-4000-8000-000000000a81"
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES($1,'usage-node',100,50)`, node); err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(Options{Auth: authService, NodeCredentials: nodes.NewService(pool, appKey), OwnerUsage: &usage.Store{DB: pool}})
	session, csrf := loginIntegrationOwner(t, handler)
	call := func(method, path, body string, headers map[string]string, withSession bool) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, "http://center.test"+path, strings.NewReader(body))
		if withSession {
			if method == http.MethodGet {
				request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
			} else {
				ownerHTTPWrite(request, session, csrf)
			}
		}
		for key, value := range headers {
			request.Header.Set(key, value)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	return call, node
}

func verifyUsageReads(t *testing.T, call ownerUsageCaller) {
	t.Helper()
	if response := call(http.MethodGet, "/api/usage?group=month", "", nil, false); response.Code != http.StatusUnauthorized {
		t.Fatalf("usage without session = %d", response.Code)
	}
	response := call(http.MethodGet, "/api/usage?group=month", "", nil, true)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"group":"month"`) || !strings.Contains(response.Body.String(), `"totalBytes":0`) {
		t.Fatalf("usage query = %d %s", response.Code, response.Body.String())
	}
	if response := call(http.MethodGet, "/api/usage?group=hour", "", nil, true); response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid usage group = %d %s", response.Code, response.Body.String())
	}
}

func verifyBudgetWrites(t *testing.T, call ownerUsageCaller) string {
	t.Helper()
	collection := call(http.MethodGet, "/api/usage/budgets", "", nil, true)
	if collection.Code != http.StatusOK || collection.Header().Get("ETag") == "" {
		t.Fatalf("budget collection = %d %s", collection.Code, collection.Body.String())
	}
	firstTag := collection.Header().Get("ETag")
	created := call(http.MethodPut, "/api/usage/budgets/global", `{"monthlyBytes":1000,"action":"pause"}`, map[string]string{"If-Match": firstTag}, true)
	if created.Code != http.StatusOK || created.Header().Get("ETag") == firstTag || !strings.Contains(created.Body.String(), `"scopeKey":"global"`) {
		t.Fatalf("create global budget = %d %s", created.Code, created.Body.String())
	}
	stale := call(http.MethodPut, "/api/usage/budgets/global", `{"monthlyBytes":2000,"action":"widen","widenMultiplier":2}`, map[string]string{"If-Match": firstTag}, true)
	if stale.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale budget update = %d %s", stale.Code, stale.Body.String())
	}
	return created.Header().Get("ETag")
}

func verifyUsageAdvice(t *testing.T, call ownerUsageCaller, node string) {
	t.Helper()
	response := call(http.MethodGet, "/api/usage/advice?node="+node+"&engine=ookla&interval=60", "", nil, true)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"estimatedBytesPerMonth":0`) {
		t.Fatalf("usage advice = %d %s", response.Code, response.Body.String())
	}
}

func verifyBudgetDelete(t *testing.T, call ownerUsageCaller, etag string) {
	t.Helper()
	response := call(http.MethodDelete, "/api/usage/budgets/global", "", map[string]string{"If-Match": etag}, true)
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete budget = %d %s", response.Code, response.Body.String())
	}
}
