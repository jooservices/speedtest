//go:build integration

package server

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/auth"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/nodes"
	"github.com/jooservices/speedtest/internal/center/ownerapi"
)

// TestOwnerResultsHTTPFlowIntegration reads results through the real HTTP stack and PostgreSQL.
func TestOwnerResultsHTTPFlowIntegration(t *testing.T) {
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
	handler := NewHandler(Options{Auth: authService, NodeCredentials: nodes.NewService(pool, appKey), OwnerNodes: owner, OwnerResults: owner})
	session, _ := loginIntegrationOwner(t, handler)

	node := "00000000-0000-4000-8000-000000000a01"
	now := time.Now().UTC()
	for _, q := range []string{
		`INSERT INTO nodes (id,name,label,expected_down_mbps,expected_up_mbps,color_slot,host,agent) VALUES ('` + node + `','VM1','Home',300,100,1,'h','a/1')`,
		`INSERT INTO schedules (node_id,interval_minutes,offset_minutes) VALUES ('` + node + `',30,0)`,
		`INSERT INTO ip_addresses (ip,geo,source) VALUES ('203.0.113.5','{"city":"Hanoi","country":"VN","latitude":21.03,"longitude":105.85,"asn":7552,"organisation":"Viettel"}','GeoLite2')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	for i, spec := range []struct {
		id, status string
		down       float64
		age        time.Duration
	}{{"00000000-0000-4000-8000-0000000000b1", "ok", 300, time.Minute}, {"00000000-0000-4000-8000-0000000000b2", "ok", 120, time.Hour}, {"00000000-0000-4000-8000-0000000000b3", "ok", 50, 10 * 24 * time.Hour}} {
		if _, err := pool.Exec(ctx, `INSERT INTO results (id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,server,ip,threads,bytes_down,bytes_up) VALUES ($1,$2,$3,$4,$5,40,11,9,'{"id":9,"name":"Viettel","city":"Hanoi","sponsor":"Viettel"}','203.0.113.5',4,700,300)`,
			spec.id, node, now.Add(-spec.age), spec.status, spec.down); err != nil {
			t.Fatal(i, err)
		}
	}
	get := func(path string, withSession bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://center.test"+path, nil)
		if withSession {
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	if rec := get("/api/results", false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("results without a session = %d", rec.Code)
	}
	var list struct {
		Data []map[string]any `json:"data"`
		Meta struct {
			Pagination map[string]any `json:"pagination"`
		} `json:"meta"`
	}
	rec := get("/api/results?size=1&sort=down&dir=asc", true)
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	if list.Meta.Pagination["total"] != float64(2) || list.Meta.Pagination["last_page"] != float64(2) || list.Data[0]["down"] != float64(120) {
		t.Fatalf("the default 7 day range holds two results, lowest download first: %s", rec.Body.String())
	}
	if wide := get("/api/results?range=30d&q=hanoi&minDown=100", true); !strings.Contains(wide.Body.String(), `"total":2`) {
		t.Fatalf("30d with search and min download = %s", wide.Body.String())
	}
	detail := get("/api/results/00000000-0000-4000-8000-0000000000b1", true)
	if detail.Code != http.StatusOK || responseString(t, detail.Body.Bytes(), "data", "geo", "city") != "Hanoi" || responseString(t, detail.Body.Bytes(), "data", "geo", "source") != "GeoLite2" {
		t.Fatalf("detail = %d %s", detail.Code, detail.Body.String())
	}
	if rec := get("/api/results/00000000-0000-4000-8000-0000000000ff", true); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown result = %d", rec.Code)
	}
	export := get("/api/results/export.csv?range=30d&sort=ts&dir=asc", true)
	records, err := csv.NewReader(export.Body).ReadAll()
	if export.Code != http.StatusOK || err != nil || len(records) != 4 || records[1][1] != "VM1 Home" || records[1][3] != "50" ||
		records[1][8] != "700" || records[1][9] != "300" || records[1][10] != "1000" || records[3][14] != "203.0.113.5" || records[1][13] != "Viettel - Hanoi" {
		t.Fatalf("export = %d %v %v", export.Code, records, err)
	}
	if rec := get("/api/results?minDown=500&maxDown=100", true); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("inverted download range = %d", rec.Code)
	}
	full := get("/api/export/results.json", true)
	var all []map[string]any
	if full.Code != http.StatusOK || json.Unmarshal(full.Body.Bytes(), &all) != nil || len(all) != 3 || all[0]["id"] != "00000000-0000-4000-8000-0000000000b3" ||
		all[2]["id"] != "00000000-0000-4000-8000-0000000000b1" {
		t.Fatalf("full JSON export covers every result regardless of age, oldest first: %d %s", full.Code, full.Body.String())
	}
	if all[0]["geo"].(map[string]any)["city"] != "Hanoi" {
		t.Fatalf("exported result lacks its location: %v", all[0])
	}
	fullCSV := get("/api/export/results.csv", true)
	if rows, err := csv.NewReader(fullCSV.Body).ReadAll(); fullCSV.Code != http.StatusOK || err != nil || len(rows) != 4 {
		t.Fatalf("full CSV export = %d %v %v", fullCSV.Code, rows, err)
	}
	t.Log("PASS: result list, detail with geolocation and CSV export over HTTP against PostgreSQL")
}

// countingWriter discards the body so the test measures the server, not a recorder that keeps everything.
type countingWriter struct {
	header http.Header
	bytes  int64
	status int
}

func (c *countingWriter) Header() http.Header  { return c.header }
func (c *countingWriter) WriteHeader(code int) { c.status = code }
func (c *countingWriter) Write(p []byte) (int, error) {
	c.bytes += int64(len(p))
	return len(p), nil
}

// TestFullExportStreamsLargeResultSets exports 30,000 rows through the handler and checks memory stays flat.
func TestFullExportStreamsLargeResultSets(t *testing.T) {
	pool, ctx, appKey := openServerIntegrationPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	hash, _ := auth.HashPassword("integration test owner password")
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
	handler := NewHandler(Options{Auth: authService, NodeCredentials: nodes.NewService(pool, appKey), OwnerResults: owner})
	session, _ := loginIntegrationOwner(t, handler)
	for _, q := range []string{
		`INSERT INTO nodes (id,name,label,expected_down_mbps,expected_up_mbps,color_slot,host,agent) VALUES ('00000000-0000-4000-8000-000000000a01','VM1','Home',300,100,1,'h','a/1')`,
		`INSERT INTO ip_addresses (ip) VALUES ('203.0.113.5')`,
		`INSERT INTO results (id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,server,ip,threads,raw)
			SELECT gen_random_uuid(),'00000000-0000-4000-8000-000000000a01',now() - (g || ' seconds')::interval,'ok',100,50,10,9,'{"id":9,"name":"S","city":"C","sponsor":"P"}','203.0.113.5',4,'{"k":"vvvvvvvvvvvvvvvvvvvvvvvv"}' FROM generate_series(1,30000) g`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"/api/export/results.csv", "/api/export/results.json"} {
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		w := &countingWriter{header: http.Header{}}
		req := httptest.NewRequest(http.MethodGet, "http://center.test"+path, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
		start := time.Now()
		handler.ServeHTTP(w, req)
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		growth := int64(after.HeapAlloc) - int64(before.HeapAlloc)
		t.Logf("%s: %d rows' worth of bytes = %d MiB in %s, heap delta %d KiB", path, 30000, w.bytes>>20, time.Since(start).Round(time.Millisecond), growth/1024)
		if w.status != http.StatusOK || w.bytes < 30000*40 || growth > 48<<20 {
			t.Fatalf("%s: status %d bytes %d heap growth %d MiB", path, w.status, w.bytes, growth>>20)
		}
	}
}
