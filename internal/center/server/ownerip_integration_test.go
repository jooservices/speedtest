//go:build integration

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/auth"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/geo"
	"github.com/jooservices/speedtest/internal/center/ipapi"
)

type stubChain struct{ calls []string }

func (s *stubChain) Lookup(_ context.Context, ip string) (geo.Location, error) {
	s.calls = append(s.calls, ip)
	if ip == "203.0.113.5" {
		return geo.Location{City: "Hanoi", Country: "VN", Latitude: 21.03, Longitude: 105.85, ASN: 7552, Organisation: "Viettel"}, nil
	}
	return geo.Location{}, geo.ErrNotFound
}

func TestOwnerIPHTTPFlowIntegration(t *testing.T) {
	pool, ctx, _ := openServerIntegrationPool(t)
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
	chain := &stubChain{}
	handler := NewHandler(Options{Auth: authService, OwnerIP: &ipapi.Store{DB: pool, Resolver: chain}})
	session, _ := loginIntegrationOwner(t, handler)
	for _, q := range []string{
		`INSERT INTO nodes (id,name,expected_down_mbps,expected_up_mbps) VALUES ('` + nodeID + `','Alpha',100,50)`,
		`INSERT INTO ip_addresses (ip,geo,source,looked_up_at) VALUES ('203.0.113.5','{"city":"Hanoi","country":"VN"}','GeoLite2',now()),('203.0.113.6','{}','',NULL)`,
		`INSERT INTO node_ip_history (id,node_id,ts,from_ip,to_ip) VALUES (gen_random_uuid(),'` + nodeID + `','2026-11-03T01:00:00Z',NULL,'203.0.113.5'),(gen_random_uuid(),'` + nodeID + `','2026-11-03T02:00:00Z','203.0.113.5','203.0.113.6')`,
		`INSERT INTO node_ip_addresses (node_id,ip,tests,first_seen,last_seen,speed_tests,down_sum,up_sum) VALUES ('` + nodeID + `','203.0.113.5',2,'2026-11-03T01:00:00Z','2026-11-03T01:30:00Z',2,200,50),('` + nodeID + `','203.0.113.6',1,'2026-11-03T02:00:00Z','2026-11-03T02:00:00Z',1,90,20)`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://center.test"+path, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	var history struct {
		Data []map[string]any `json:"data"`
		Meta struct {
			Pagination map[string]any `json:"pagination"`
		} `json:"meta"`
	}
	rec := get("/api/ip/history?node=" + nodeID)
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &history) != nil || len(history.Data) != 1 || history.Data[0]["to"] != "203.0.113.6" ||
		history.Data[0]["toGeo"] != nil || history.Data[0]["fromGeo"].(map[string]any)["city"] != "Hanoi" || history.Meta.Pagination["total"] != 1.0 {
		t.Fatalf("history = %d %s", rec.Code, rec.Body.String())
	}
	var addresses struct {
		Data []map[string]any `json:"data"`
	}
	rec = get("/api/ip/addresses")
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &addresses) != nil || len(addresses.Data) != 2 || addresses.Data[0]["ip"] != "203.0.113.6" || addresses.Data[1]["avgDown"] != 100.0 {
		t.Fatalf("addresses = %d %s", rec.Code, rec.Body.String())
	}
	if rec := get("/api/ip/map"); rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &addresses) != nil || len(addresses.Data) != 1 || addresses.Data[0]["ip"] != "203.0.113.6" {
		t.Fatalf("map = %d %s", rec.Code, rec.Body.String())
	}
	var lookup struct {
		Data map[string]any `json:"data"`
	}
	rec = get("/api/ip/lookup?ip=203.0.113.5")
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &lookup) != nil || lookup.Data["city"] != "Hanoi" || lookup.Data["source"] != "GeoLite2" || lookup.Data["asn"] != 7552.0 {
		t.Fatalf("lookup = %d %s", rec.Code, rec.Body.String())
	}
	if rec := get("/api/ip/lookup?ip=198.51.100.77"); rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &lookup) != nil || lookup.Data != nil {
		t.Fatalf("unknown address = %d %s", rec.Code, rec.Body.String())
	}
	callsBefore := len(chain.calls)
	for _, bad := range []string{"example.com", "203.0.113.5:80", "203.0.113.0%2F24", "fe80::1%25eth0"} {
		if rec := get("/api/ip/lookup?ip=" + bad); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("ip=%s = %d, want 422", bad, rec.Code)
		}
	}
	if len(chain.calls) != callsBefore {
		t.Fatalf("invalid input must never reach the geolocation chain: %v", chain.calls[callsBefore:])
	}
}
