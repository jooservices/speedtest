//go:build integration

package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/alerts"
	"github.com/jooservices/speedtest/internal/center/auth"
	"github.com/jooservices/speedtest/internal/center/dashboard"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/ownerapi"
	"github.com/jooservices/speedtest/internal/center/stats"
)

func dashboardTestHandler(t *testing.T) (http.Handler, string, func(string, ...any)) {
	t.Helper()
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
	owner := &ownerapi.Store{DB: pool}
	statsStore := &stats.Store{DB: pool}
	handler := NewHandler(Options{Auth: authService, OwnerDashboard: &dashboard.Store{DB: pool, Nodes: owner, Stats: statsStore, Results: owner,
		Alerts: &alerts.Store{DB: pool, AppKey: make([]byte, 32)}}})
	session, _ := loginIntegrationOwner(t, handler)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	return handler, session, exec
}

func dashboardGet(handler http.Handler, session, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://center.test"+path, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

const (
	dashAlpha = "00000000-0000-4000-8000-0000000000a1"
	dashBravo = "00000000-0000-4000-8000-0000000000a2"
	dashEmpty = "00000000-0000-4000-8000-0000000000a3"
)

// TestDashboardGoldenFixture checks the fleet figures, node cards and lists against hand-computed values that follow the
// approved preview's formulas.
func TestDashboardGoldenFixture(t *testing.T) {
	handler, session, exec := dashboardTestHandler(t)
	now := time.Now().UTC()
	exec(`INSERT INTO nodes (id,name,label,expected_down_mbps,expected_up_mbps,color_slot,host,agent) VALUES
		('` + dashAlpha + `','Alpha','Home',1000,100,1,'h','a/1'),('` + dashBravo + `','Bravo','Office',200,50,2,'h','a/1'),('` + dashEmpty + `','Empty','',100,25,3,'h','a/1')`)
	exec(`INSERT INTO schedules (node_id) SELECT id FROM nodes`)
	exec(`INSERT INTO engine_schedules (node_id, engine) SELECT id, 'ookla' FROM nodes`)
	exec(`INSERT INTO ip_addresses(ip) VALUES ('203.0.113.5'),('203.0.113.6'),('203.0.113.7')`)
	n := 0
	seed := func(node string, at time.Time, status string, down, up, ping any) {
		t.Helper()
		n++
		var serverJSON, errText any = `{"id":9,"name":"S","city":"C","sponsor":"P"}`, nil
		if status == "failed" {
			serverJSON, errText = nil, "boom"
		}
		exec(`INSERT INTO results (id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,server,ip,threads,error)
			VALUES ($1,$2,$3,$4,$5,$6,$7,9,$8::jsonb,'203.0.113.5',4,$9)`, fmt.Sprintf("00000000-0000-4000-8000-%012x", n), node, at, status, down, up, ping, serverJSON, errText)
	}
	// Current 24 hours.
	seed(dashAlpha, now.Add(-10*time.Hour), "ok", 400.0, 50.0, 10.0)
	seed(dashAlpha, now.Add(-9*time.Hour), "degraded", 600.0, 50.0, 100.0)
	seed(dashAlpha, now.Add(-8*time.Hour), "failed", nil, nil, nil)
	seed(dashBravo, now.Add(-5*time.Hour), "ok", 160.0, 40.0, 30.0)
	// Previous 24 hours.
	seed(dashAlpha, now.Add(-30*time.Hour), "ok", 300.0, 30.0, 20.0)
	// Eight old results so that the list of latest results must be cut to nine.
	for i := 0; i < 8; i++ {
		seed(dashBravo, now.Add(-time.Duration(60+i)*24*time.Hour), "ok", 100.0, 10.0, 10.0)
	}
	exec(`INSERT INTO node_ip_history (id,node_id,ts,from_ip,to_ip) VALUES
		(gen_random_uuid(),$1,$2,NULL,'203.0.113.5'),(gen_random_uuid(),$1,$3,'203.0.113.5','203.0.113.6'),
		(gen_random_uuid(),$4,$5,'203.0.113.5','203.0.113.7'),(gen_random_uuid(),$1,$6,'203.0.113.6','203.0.113.5')`,
		dashAlpha, now.Add(-11*time.Hour), now.Add(-7*time.Hour), dashBravo, now.Add(-4*time.Hour), now.Add(-29*time.Hour))
	for i := 0; i < 8; i++ {
		exec(`INSERT INTO alert_events (id,node_id,severity,ts,title,acked) VALUES (gen_random_uuid(),$1,'warn',$2,'Slow',$3)`,
			dashAlpha, now.Add(-time.Duration(i+1)*time.Hour), i >= 7)
	}

	var env struct {
		Data struct {
			Current, Previous map[string]any
			Nodes             []map[string]any
			LatestResults     []map[string]any
			OpenAlerts        []map[string]any
			OpenAlertCount    int
		} `json:"data"`
	}
	rec := dashboardGet(handler, session, "/api/dashboard?range=24h")
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &env) != nil {
		t.Fatalf("dashboard = %d %s", rec.Code, rec.Body.String())
	}
	d := env.Data
	// down: mean of Alpha 50% and Bravo 80%; up: Alpha 50% and Bravo 80%; ping: median of the ok-only pings 10 and 30; success 3/4.
	if d.Current["down"] != 65.0 || d.Current["up"] != 65.0 || d.Current["ping"] != 20.0 || d.Current["success"] != 75.0 || d.Current["ips"] != 2.0 || d.Current["count"] != 4.0 {
		t.Fatalf("current fleet = %v", d.Current)
	}
	if d.Previous["down"] != 30.0 || d.Previous["up"] != 30.0 || d.Previous["ping"] != 20.0 || d.Previous["success"] != 100.0 || d.Previous["ips"] != 1.0 || d.Previous["count"] != 1.0 {
		t.Fatalf("previous fleet = %v", d.Previous)
	}
	if len(d.Nodes) != 3 || d.Nodes[0]["node"].(map[string]any)["name"] != "Alpha" || d.Nodes[2]["node"].(map[string]any)["name"] != "Empty" {
		t.Fatalf("node cards must follow the node order: %v", d.Nodes)
	}
	alpha := d.Nodes[0]["summary"].(map[string]any)
	if alpha["count"] != 3.0 || alpha["failed"] != 1.0 || alpha["downPct"] != 50.0 {
		t.Fatalf("alpha summary = %v", alpha)
	}
	if empty := d.Nodes[2]["summary"].(map[string]any); empty["count"] != 0.0 || empty["successRate"] != nil || empty["downPct"] != nil {
		t.Fatalf("a node without results has null aggregates: %v", empty)
	}
	for i, card := range d.Nodes {
		spark := card["sparkline"].([]any)
		if len(spark) != dashboard.SparkPoints {
			t.Fatalf("node %d sparkline has %d points, want %d", i, len(spark), dashboard.SparkPoints)
		}
	}
	nonNull := 0
	for _, p := range d.Nodes[0]["sparkline"].([]any) {
		if p.(map[string]any)["value"] != nil {
			nonNull++
		}
	}
	if nonNull != 2 {
		t.Fatalf("alpha sparkline has %d samples in the last 24 h, want its 2 non-failed ones", nonNull)
	}
	if len(d.LatestResults) != 9 || d.LatestResults[0]["nodeId"] != dashBravo || d.LatestResults[0]["down"] != 160.0 {
		t.Fatalf("latest results = %d, first %v", len(d.LatestResults), d.LatestResults[0])
	}
	if len(d.OpenAlerts) != 6 || d.OpenAlertCount != 7 {
		t.Fatalf("open alerts = %d, count %d (7 unacknowledged, the newest 6 listed)", len(d.OpenAlerts), d.OpenAlertCount)
	}
	if rec := dashboardGet(handler, "", "/api/dashboard"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("without a session = %d", rec.Code)
	}
}

func TestDashboardOfAnEmptyFleet(t *testing.T) {
	handler, session, _ := dashboardTestHandler(t)
	rec := dashboardGet(handler, session, "/api/dashboard")
	var env struct {
		Data map[string]any `json:"data"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &env) != nil {
		t.Fatalf("dashboard = %d %s", rec.Code, rec.Body.String())
	}
	for _, key := range []string{"nodes", "latestResults", "openAlerts"} {
		if list, ok := env.Data[key].([]any); !ok || len(list) != 0 {
			t.Errorf("%s = %v, want an empty array", key, env.Data[key])
		}
	}
	current := env.Data["current"].(map[string]any)
	if current["down"] != nil || current["success"] != nil || current["count"] != 0.0 || env.Data["openAlertCount"] != 0.0 {
		t.Fatalf("empty fleet aggregates = %v", current)
	}
}

func TestSeriesOverHTTPAndResponseSize(t *testing.T) {
	handler, session, exec := dashboardTestHandler(t)
	for i := 1; i <= 5; i++ {
		exec(`INSERT INTO nodes (id,name,label,expected_down_mbps,expected_up_mbps,color_slot,host,agent) VALUES ($1,$2,'',500,100,$3,'h','a/1')`,
			fmt.Sprintf("00000000-0000-4000-8000-0000000000b%d", i), fmt.Sprintf("Node%d", i), i)
	}
	exec(`INSERT INTO ip_addresses(ip) VALUES ('203.0.113.5')`)
	// One week of results every 30 minutes for each of the five nodes.
	exec(`INSERT INTO results (id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,server,ip,threads)
		SELECT gen_random_uuid(), n.id, now() - (g * interval '30 minutes') - interval '1 minute', 'ok', 100 + (g % 50), 20, 10 + (g % 7), 9,
		'{"id":9,"name":"S","city":"C","sponsor":"P"}'::jsonb, '203.0.113.5', 4 FROM nodes n CROSS JOIN generate_series(0, 335) g`)

	for _, tc := range []struct{ query, unit string }{{"range=7d", "Mbps"}, {"range=7d&mode=percent", "%"}, {"range=24h&metric=ping&maxPoints=48", "ms"}} {
		rec := dashboardGet(handler, session, "/api/series?"+tc.query)
		var env struct {
			Data []struct {
				Name   string `json:"name"`
				Unit   string `json:"unit"`
				Points []struct {
					Value *float64 `json:"value"`
					Count int      `json:"count"`
				} `json:"points"`
				Summary struct {
					N int `json:"n"`
				} `json:"summary"`
			} `json:"data"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &env) != nil || len(env.Data) != 5 {
			t.Fatalf("%s = %d %.200s", tc.query, rec.Code, rec.Body.String())
		}
		limit := dashboard.SparkPoints
		if tc.query == "range=7d" || tc.query == "range=7d&mode=percent" {
			limit = 300
		}
		for _, s := range env.Data {
			if len(s.Points) > limit || len(s.Points) == 0 || s.Unit != tc.unit || s.Summary.N == 0 {
				t.Fatalf("%s: %s has %d points (limit %d) unit %q n %d", tc.query, s.Name, len(s.Points), limit, s.Unit, s.Summary.N)
			}
		}
		if rec.Body.Len() > 120<<10 {
			t.Fatalf("%s response is %d bytes; five series of at most 300 points must stay small", tc.query, rec.Body.Len())
		}
		t.Logf("%s: 5 series, %d bytes", tc.query, rec.Body.Len())
	}
	if rec := dashboardGet(handler, session, "/api/series?mode=percent&metric=ping"); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("percent ping = %d", rec.Code)
	}
}
