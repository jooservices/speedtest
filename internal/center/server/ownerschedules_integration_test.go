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

// TestOwnerSchedulesHTTPFlowIntegration edits schedules and rules through the real HTTP stack and PostgreSQL,
// and checks that the agent configuration endpoint reflects the change.
func TestOwnerSchedulesHTTPFlowIntegration(t *testing.T) {
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
	owner := &ownerapi.Store{DB: pool}
	handler := NewHandler(Options{Auth: authService, NodeCredentials: nodes.NewService(pool, appKey), OwnerNodes: owner, OwnerSchedules: owner})
	session, csrf := loginIntegrationOwner(t, handler)
	call := func(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://center.test"+path, strings.NewReader(body))
		if method == http.MethodGet {
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
		} else {
			ownerHTTPWrite(req, session, csrf)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	var ids []string
	for i, name := range []string{"n1", "n2"} {
		rec := call(http.MethodPost, "/api/nodes", `{"name":"`+name+`","expectedDown":300,"expectedUp":100}`, map[string]string{"Idempotency-Key": "sched-flow-" + name})
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %d = %d %s", i, rec.Code, rec.Body.String())
		}
		var created struct {
			Data struct {
				Node struct {
					Interval int `json:"interval"`
					Offset   int `json:"offset"`
				} `json:"node"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.Data.Node.Interval != 60 || created.Data.Node.Offset != 0 {
			t.Fatalf("created node schedule = %d:%d, want 60:0: %v", created.Data.Node.Interval, created.Data.Node.Offset, err)
		}
		ids = append(ids, responseString(t, rec.Body.Bytes(), "data", "node", "id"))
	}

	if rec := call(http.MethodGet, "/api/schedules", "", nil); rec.Code != http.StatusOK || rec.Header().Get("ETag") == "" ||
		!strings.Contains(rec.Body.String(), `"stagger":true`) {
		t.Fatalf("collection = %d %s", rec.Code, rec.Body.String())
	}
	one := call(http.MethodGet, "/api/schedules/"+ids[0], "", nil)
	tag := one.Header().Get("ETag")
	if one.Code != http.StatusOK || tag == "" || responseString(t, one.Body.Bytes(), "data", "nodeId") != ids[0] ||
		!strings.Contains(one.Body.String(), `"interval":60`) || !strings.Contains(one.Body.String(), `"offset":0`) ||
		!strings.Contains(one.Body.String(), `"bytesPerDay":0`) || !strings.Contains(one.Body.String(), `"bytesPerMonth":0`) {
		t.Fatalf("one = %d %s", one.Code, one.Body.String())
	}
	put := call(http.MethodPut, "/api/schedules/"+ids[0], `{"interval":60,"offset":75,"enabled":true}`, map[string]string{"If-Match": tag})
	if put.Code != http.StatusOK || put.Header().Get("ETag") == tag || !strings.Contains(put.Body.String(), `"offset":59`) {
		t.Fatalf("put = %d %s", put.Code, put.Body.String())
	}
	if rec := call(http.MethodPut, "/api/schedules/"+ids[0], `{"interval":60,"offset":0,"enabled":true}`, map[string]string{"If-Match": tag}); rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale put = %d", rec.Code)
	}
	five := call(http.MethodPut, "/api/schedules/"+ids[0], `{"interval":5,"offset":4,"enabled":true}`, map[string]string{"If-Match": put.Header().Get("ETag")})
	if five.Code != http.StatusOK || !strings.Contains(five.Body.String(), `"interval":5`) || !strings.Contains(five.Body.String(), `"offset":4`) {
		t.Fatalf("minimum interval update = %d %s", five.Code, five.Body.String())
	}
	restored := call(http.MethodPut, "/api/schedules/"+ids[0], `{"interval":60,"offset":0,"enabled":true}`, map[string]string{"If-Match": five.Header().Get("ETag")})
	if restored.Code != http.StatusOK {
		t.Fatalf("restore schedule = %d %s", restored.Code, restored.Body.String())
	}
	if rec := call(http.MethodPut, "/api/schedules/"+ids[0], `{"interval":4,"offset":0,"enabled":true}`, map[string]string{"If-Match": restored.Header().Get("ETag")}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("interval below the minimum = %d", rec.Code)
	}

	collection := call(http.MethodGet, "/api/schedules", "", nil).Header().Get("ETag")
	rules := call(http.MethodPut, "/api/schedules/rules", `{"stagger":false,"maxConcurrent":2,"quiet":{"quietStart":"23:00","quietEnd":"05:00","timezone":"Asia/Ho_Chi_Minh"}}`, map[string]string{"If-Match": collection})
	if rules.Code != http.StatusOK || responseString(t, rules.Body.Bytes(), "data", "quiet", "quietStart") != "23:00" {
		t.Fatalf("rules = %d %s", rules.Code, rules.Body.String())
	}
	if rec := call(http.MethodPut, "/api/schedules/rules", `{"stagger":false,"maxConcurrent":2,"quiet":{"quietStart":"23:00","quietEnd":"23:00","timezone":"UTC"}}`, map[string]string{"If-Match": rules.Header().Get("ETag")}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("equal quiet ends = %d", rec.Code)
	}
	if rec := call(http.MethodPost, "/api/schedules/stagger", "", map[string]string{"If-Match": collection}); rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("stagger with an old tag = %d, want 412", rec.Code)
	}
	stagger := call(http.MethodPost, "/api/schedules/stagger", "", map[string]string{"If-Match": rules.Header().Get("ETag")})
	if stagger.Code != http.StatusOK || !strings.Contains(stagger.Body.String(), `"clashes":[]`) {
		t.Fatalf("stagger = %d %s", stagger.Code, stagger.Body.String())
	}
	var interval, offset int
	var revision int64
	if err := pool.QueryRow(ctx, `SELECT interval_minutes, offset_minutes, revision FROM schedules WHERE node_id=$1`, ids[0]).Scan(&interval, &offset, &revision); err != nil ||
		interval != 60 || offset != 0 || revision < 3 {
		t.Fatalf("stored schedule: %d %d rev %d %v", interval, offset, revision, err)
	}
	var quiet string
	if err := pool.QueryRow(ctx, `SELECT value #>> '{}' FROM settings WHERE key='timezone'`).Scan(&quiet); err != nil || quiet != "Asia/Ho_Chi_Minh" {
		t.Fatalf("stored timezone = %q %v", quiet, err)
	}
	t.Log("PASS: schedule list, per-node replace with clamp, rules, stagger and ETag preconditions over HTTP against PostgreSQL")
}
