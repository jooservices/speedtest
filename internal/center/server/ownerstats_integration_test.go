//go:build integration

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jooservices/speedtest/internal/center/auth"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/stats"
)

const statsEmptyNodeID = "00000000-0000-4000-8000-0000000000a3"
const statsConstantNodeID = "00000000-0000-4000-8000-0000000000a4"
const statsEventNodeID = "00000000-0000-4000-8000-0000000000a5"

type statsEnvelope[T any] struct {
	Data T `json:"data"`
}

func TestOwnerStatisticsHTTPFlowIntegration(t *testing.T) {
	pool, ctx, _ := openServerIntegrationPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)

	passwordHash, err := auth.HashPassword("integration test owner password")
	if err != nil {
		t.Fatal("could not prepare integration owner")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,email,password_hash) VALUES ('00000000-0000-4000-8000-000000000901','owner@example.test',$1)`, passwordHash); err != nil {
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
	now := time.Now().UTC().Truncate(time.Second)
	from := now.Add(-24 * time.Hour)
	previousFrom := now.Add(-48 * time.Hour)
	const nodeA = nodeID
	const nodeB = "00000000-0000-4000-8000-0000000000a2"
	for _, q := range []string{
		`INSERT INTO nodes (id,name,expected_down_mbps,expected_up_mbps) VALUES ('` + nodeA + `','Alpha',1000,100)`,
		`INSERT INTO nodes (id,name,expected_down_mbps,expected_up_mbps) VALUES ('` + nodeB + `','Bravo',200,50)`,
		`INSERT INTO nodes (id,name,expected_down_mbps,expected_up_mbps) VALUES ('` + statsEmptyNodeID + `','Empty',100,25)`,
		`INSERT INTO nodes (id,name,expected_down_mbps,expected_up_mbps) VALUES ('` + statsConstantNodeID + `','Constant',100,25)`,
		`INSERT INTO ip_addresses(ip) VALUES ('203.0.113.5'),('203.0.113.6')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	bucketStart := now.Add(-6 * time.Hour).Truncate(time.Hour)
	seedResult := func(id int, node string, at time.Time, status string, down, up, ping, jitter, loss any, ip string) {
		t.Helper()
		var serverID any = 9
		var errorText, serverJSON any = nil, `{"id":9,"name":"Fixture","city":"C","sponsor":"P"}`
		if status == "failed" {
			serverID, errorText, serverJSON = nil, "fixture failure", nil
		}
		resultID := fmt.Sprintf("00000000-0000-4000-8000-%012x", id)
		_, err := pool.Exec(ctx, `INSERT INTO results (id,node_id,ts,status,down_mbps,up_mbps,ping_ms,jitter_ms,loss_percent,server_id,server,ip,threads,error)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$13::jsonb,$11,4,$12)`, resultID, node, at, status, down, up, ping, jitter, loss, serverID, ip, errorText, serverJSON)
		if err != nil {
			t.Fatal(err)
		}
	}
	seedResult(0xb1, nodeA, previousFrom.Add(4*time.Hour), "ok", 10.0, 2.0, 30.0, 1.0, 0.0, "203.0.113.5")
	seedResult(0xb2, nodeA, previousFrom.Add(5*time.Hour), "ok", 20.0, 4.0, 50.0, 3.0, 1.0, "203.0.113.5")
	seedResult(0xb3, nodeA, previousFrom.Add(6*time.Hour), "degraded", 30.0, 6.0, 70.0, 5.0, 2.0, "203.0.113.5")
	seedResult(0xb4, nodeA, previousFrom.Add(7*time.Hour), "failed", nil, nil, nil, nil, nil, "203.0.113.6")
	seedResult(0xb5, nodeA, bucketStart.Add(10*time.Minute), "ok", 100.0, 10.0, 50.0, 2.0, 0.5, "203.0.113.5")
	seedResult(0xb6, nodeA, bucketStart.Add(35*time.Minute), "ok", 200.0, 20.0, 10.0, 4.0, 1.0, "203.0.113.5")
	seedResult(0xb7, nodeA, bucketStart.Add(70*time.Minute), "ok", 300.0, 30.0, 30.0, nil, nil, "203.0.113.5")
	seedResult(0xb8, nodeA, bucketStart.Add(100*time.Minute), "degraded", 400.0, 40.0, 40.0, 8.0, 2.0, "203.0.113.5")
	seedResult(0xb9, nodeA, bucketStart.Add(130*time.Minute), "failed", nil, nil, nil, nil, nil, "203.0.113.6")
	seedResult(0xc0, nodeA, bucketStart.Add(160*time.Minute), "failed", nil, nil, nil, nil, nil, "203.0.113.6")
	if _, err := pool.Exec(ctx, `UPDATE results SET failure_class='rate_limited' WHERE id=$1`, fmt.Sprintf("00000000-0000-4000-8000-%012x", 0xc0)); err != nil {
		t.Fatal(err)
	}
	seedResult(0xba, nodeB, bucketStart.Add(15*time.Minute), "ok", 50.0, 5.0, 20.0, 1.0, 0.1, "203.0.113.5")
	seedResult(0xbf, statsConstantNodeID, now.Add(-12*time.Hour), "ok", 0.0, 0.0, 0.0, 0.0, 0.0, "203.0.113.5")

	handler := NewHandler(Options{Auth: authService, NodeCredentials: nil, OwnerStats: &stats.Store{DB: pool}})
	session, _ := loginIntegrationOwner(t, handler)
	get := func(path string, withSession bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://center.test"+path, nil)
		if withSession {
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	currentRange := "from=" + from.Format(time.RFC3339Nano) + "&to=" + now.Format(time.RFC3339Nano)
	if rec := get("/api/stats/summary?"+currentRange, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("summary without session = %d", rec.Code)
	}
	var summary statsEnvelope[[]stats.SummaryComparison]
	rec := get("/api/stats/summary?"+currentRange, true)
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &summary) != nil {
		t.Fatalf("summary = %d %s", rec.Code, rec.Body.String())
	}
	if len(summary.Data) != 4 {
		t.Fatalf("summary nodes = %d, want all 4", len(summary.Data))
	}
	var alpha stats.SummaryComparison
	for _, item := range summary.Data {
		if item.NodeID == nodeA {
			alpha = item
		}
	}
	if alpha.Current.Count != 6 || alpha.Current.OK != 4 || alpha.Current.Failed != 2 || alpha.Current.RateLimited != 1 || alpha.Current.SuccessRate == nil || *alpha.Current.SuccessRate != 80 || alpha.Current.IPCount != 2 {
		t.Fatalf("current counts = %+v", alpha.Current)
	}
	if alpha.Current.Down.N != 4 || alpha.Current.Down.Mean == nil || *alpha.Current.Down.Mean != 250 || alpha.Current.Down.Median == nil || *alpha.Current.Down.Median != 250 ||
		alpha.Current.Down.P5 == nil || *alpha.Current.Down.P5 != 115 || alpha.Current.Down.P95 == nil || *alpha.Current.Down.P95 != 385 ||
		alpha.Current.Down.Min == nil || *alpha.Current.Down.Min != 100 || alpha.Current.Down.Max == nil || *alpha.Current.Down.Max != 400 ||
		alpha.Current.DownPct == nil || *alpha.Current.DownPct != 25 || alpha.Current.Stability == nil ||
		math.Abs(*alpha.Current.Stability-(100-220*math.Sqrt(12500)/250)) > 1e-12 {
		t.Fatalf("current download summary = %+v", alpha.Current)
	}
	if alpha.Previous.Count != 4 || alpha.Previous.OK != 3 || alpha.Previous.Down.N != 3 || alpha.Previous.Down.Mean == nil || *alpha.Previous.Down.Mean != 20 {
		t.Fatalf("previous period = %+v", alpha.Previous)
	}

	selected := nodeA + "," + nodeB
	trendPath := "/api/stats/trend?" + currentRange + "&nodes=" + selected + "&metric=down&timezone=Asia%2FHo_Chi_Minh&bucket=hour&combined=true"
	var trend statsEnvelope[[]stats.Series]
	rec = get(trendPath, true)
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &trend) != nil || len(trend.Data) != 3 {
		t.Fatalf("trend = %d %s", rec.Code, rec.Body.String())
	}
	localHour := bucketStart.In(mustLocation(t, "Asia/Ho_Chi_Minh")).Hour()
	combinedFound := false
	for _, series := range trend.Data {
		var point *stats.Point
		for i := range series.Points {
			if series.Points[i].TS.In(mustLocation(t, "Asia/Ho_Chi_Minh")).Hour() == localHour && series.Points[i].Count > 0 {
				point = &series.Points[i]
			}
		}
		if series.NodeID != nil && *series.NodeID == nodeA {
			if point == nil || *point.Value != 150 || point.Count != 2 || series.Summary.N != 3 {
				t.Fatalf("node trend = %+v", series)
			}
		}
		if series.NodeID != nil && *series.NodeID == nodeB && (point == nil || *point.Value != 50 || point.Count != 1) {
			t.Fatalf("second node trend = %+v", series)
		}
		if series.NodeID == nil {
			combinedFound = true
			if point == nil || *point.Value != 200 || point.Count != 3 {
				t.Fatalf("combined trend = %+v", series)
			}
			partialFound := false
			for _, candidate := range series.Points {
				if candidate.TS.Equal(bucketStart.Add(time.Hour)) {
					partialFound = candidate.Value != nil && *candidate.Value == 300 && candidate.Count == 1
				}
			}
			if !partialFound {
				t.Fatalf("combined trend did not preserve a bucket present for one selected node: %+v", series.Points)
			}
		}
	}
	if !combinedFound {
		t.Fatal("combined trend series is missing")
	}

	var hours statsEnvelope[[]stats.HourSeries]
	rec = get("/api/stats/hour-of-day?"+currentRange+"&nodes="+nodeA+"&metric=down&timezone=Asia%2FHo_Chi_Minh", true)
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &hours) != nil || len(hours.Data) != 1 || len(hours.Data[0].Hours) != 24 {
		t.Fatalf("hour of day = %d %s", rec.Code, rec.Body.String())
	}
	if value := hours.Data[0].Hours[localHour]; value.Count != 2 || value.Value == nil || *value.Value != 150 {
		t.Fatalf("local hour %d = %+v", localHour, value)
	}

	var heat statsEnvelope[stats.Heatmap]
	rec = get("/api/stats/heatmap?node="+nodeA+"&metric=down&timezone=Asia%2FHo_Chi_Minh", true)
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &heat) != nil || len(heat.Data.Cells) != 168 {
		t.Fatalf("heatmap = %d %s", rec.Code, rec.Body.String())
	}
	localSample := bucketStart.In(mustLocation(t, "Asia/Ho_Chi_Minh"))
	weekday := (int(localSample.Weekday()) + 6) % 7
	cell := heat.Data.Cells[weekday*24+localSample.Hour()]
	if cell.Count != 2 || cell.Value == nil || *cell.Value != 150 || heat.Data.Timezone != "Asia/Ho_Chi_Minh" {
		t.Fatalf("heatmap cell = %+v", cell)
	}

	var distributions statsEnvelope[[]stats.Distribution]
	rec = get("/api/stats/distribution?"+currentRange+"&nodes="+nodeA+"&metric=down&bins=3", true)
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &distributions) != nil || len(distributions.Data) != 1 {
		t.Fatalf("distribution = %d %s", rec.Code, rec.Body.String())
	}
	distribution := distributions.Data[0]
	if distribution.Count != 3 || len(distribution.Bins) != 3 || distribution.Bins[0].Count != 1 || distribution.Bins[1].Count != 1 || distribution.Bins[2].Count != 1 ||
		distribution.Bins[0].From != 100 || distribution.Bins[2].To != 300 {
		t.Fatalf("distribution excludes degraded/failed samples: %+v", distribution)
	}
	if rec := get("/api/stats/distribution?"+currentRange+"&nodes="+statsEmptyNodeID, true); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"bins":[]`) || !strings.Contains(rec.Body.String(), `"count":0`) {
		t.Fatalf("empty distribution = %d %s", rec.Code, rec.Body.String())
	}
	rec = get("/api/stats/distribution?"+currentRange+"&nodes="+statsConstantNodeID+"&bins=4", true)
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &distributions) != nil || len(distributions.Data) != 1 || distributions.Data[0].Count != 1 || len(distributions.Data[0].Bins) != 4 {
		t.Fatalf("constant distribution = %d %s", rec.Code, rec.Body.String())
	}
	if distributions.Data[0].Bins[0].From >= 0 || distributions.Data[0].Bins[3].To <= 0 || distributions.Data[0].Bins[0].From != -distributions.Data[0].Bins[3].To {
		t.Fatalf("constant distribution range is not centered with nonzero width: %+v", distributions.Data[0])
	}
	if rec := get("/api/stats/heatmap?node="+statsEmptyNodeID, true); rec.Code != http.StatusOK {
		t.Fatalf("empty-node heatmap = %d %s", rec.Code, rec.Body.String())
	}
	if rec := get("/api/stats/heatmap?node=00000000-0000-4000-8000-0000000000ff", true); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown-node heatmap = %d", rec.Code)
	}
	seedResult(0xbb, nodeA, time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC), "ok", 60.0, 6.0, 20.0, 1.0, 0.1, "203.0.113.5")
	seedResult(0xbc, nodeA, time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC), "ok", 80.0, 8.0, 30.0, 1.0, 0.1, "203.0.113.5")
	seedResult(0xbd, nodeA, time.Date(2026, 3, 8, 6, 30, 0, 0, time.UTC), "ok", 70.0, 7.0, 40.0, 1.0, 0.1, "203.0.113.5")
	seedResult(0xbe, nodeA, time.Date(2026, 3, 8, 7, 30, 0, 0, time.UTC), "ok", 90.0, 9.0, 50.0, 1.0, 0.1, "203.0.113.5")

	assertDSTTrend := func(from, to time.Time, wantHours []int, wantRepeatedCount int) {
		t.Helper()
		path := fmt.Sprintf("/api/stats/trend?from=%s&to=%s&nodes=%s&metric=down&timezone=America%%2FNew_York&bucket=hour",
			from.Format(time.RFC3339), to.Format(time.RFC3339), nodeA)
		var result statsEnvelope[[]stats.Series]
		rec := get(path, true)
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &result) != nil || len(result.Data) != 1 || len(result.Data[0].Points) != len(wantHours) {
			t.Fatalf("DST trend = %d %s", rec.Code, rec.Body.String())
		}
		loc := mustLocation(t, "America/New_York")
		for i, hour := range wantHours {
			if result.Data[0].Points[i].TS.In(loc).Hour() != hour {
				t.Fatalf("DST local hour[%d] = %s, want %d", i, result.Data[0].Points[i].TS.In(loc), hour)
			}
		}
		if wantRepeatedCount > 0 && result.Data[0].Points[1].Count != wantRepeatedCount {
			t.Fatalf("repeated DST hour count = %d, want %d", result.Data[0].Points[1].Count, wantRepeatedCount)
		}
	}
	assertDSTTrend(time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC), []int{0, 1, 2}, 2)
	assertDSTTrend(time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC), time.Date(2026, 3, 8, 9, 0, 0, 0, time.UTC), []int{0, 1, 3, 4}, 0)

	t.Log("PASS: rate_limited is counted separately and excluded from success rate; authenticated stats flows pass over HTTP/PostgreSQL")
}

func TestOwnerStatisticsEventsAttributeEngineISPAndIPIntegration(t *testing.T) {
	get, now := setupStatisticsEvents(t)
	from := now.Add(-24 * time.Hour)
	path := "/api/stats/events?from=" + from.Format(time.RFC3339) + "&to=" + now.Format(time.RFC3339) + "&nodes=" + statsEventNodeID + "&engine=ookla"
	var result statsEnvelope[stats.EventsResponse]
	rec := get(path)
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &result) != nil || len(result.Data.Items) != 5 || result.Data.Truncated {
		t.Fatalf("events = %d %s", rec.Code, rec.Body.String())
	}
	if !hasStatsEvent(result.Data.Items, "ip_change", "IP 203.0.113.10 → 203.0.113.11") || !hasStatsEvent(result.Data.Items, "isp_change", "Carrier A → Carrier B") {
		t.Fatalf("IP/ISP marks are missing: %+v", result.Data.Items)
	}
	result = statsEnvelope[stats.EventsResponse]{}
	rec = get(path + "&isp=Carrier%20B")
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &result) != nil || len(result.Data.Items) != 2 || !hasStatsEvent(result.Data.Items, "alert", "Linked alert") {
		t.Fatalf("ISP-filtered events = %d %s", rec.Code, rec.Body.String())
	}
	result = statsEnvelope[stats.EventsResponse]{}
	rec = get(strings.Replace(path, "engine=ookla", "engine=librespeed", 1))
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &result) != nil || len(result.Data.Items) != 1 || !hasStatsEvent(result.Data.Items, "alert", "engine unknown") {
		t.Fatalf("unlinked alert with selected engine = %d %s", rec.Code, rec.Body.String())
	}
}

func setupStatisticsEvents(t *testing.T) (func(string) *httptest.ResponseRecorder, time.Time) {
	pool, ctx, _ := openServerIntegrationPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	now := time.Now().UTC().Truncate(time.Second)
	seedStatisticsEventRows(t, pool, ctx, now)
	handler := newStatisticsIntegrationHandler(t, pool, ctx)
	session, _ := loginIntegrationOwner(t, handler)
	return func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://center.test"+path, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}, now
}

func seedStatisticsEventRows(t *testing.T, pool *pgxpool.Pool, ctx context.Context, now time.Time) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES ($1,'Events',100,20)`, statsEventNodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ip_addresses(ip,geo) VALUES ('203.0.113.10','{"organisation":"Geo ISP"}'),('203.0.113.11','{"organisation":"Geo ISP"}')`); err != nil {
		t.Fatal(err)
	}
	insertStatsEventResult(t, pool, ctx, "00000000-0000-4000-8000-000000000101", now.Add(-25*time.Hour), "203.0.113.10", "Carrier A")
	insertStatsEventResult(t, pool, ctx, "00000000-0000-4000-8000-000000000102", now.Add(-8*time.Hour), "203.0.113.11", "Carrier A")
	insertStatsEventResult(t, pool, ctx, "00000000-0000-4000-8000-000000000103", now.Add(-7*time.Hour), "203.0.113.11", "Carrier B")
	insertStatsEventResult(t, pool, ctx, "00000000-0000-4000-8000-000000000104", now.Add(-6*time.Hour), "203.0.113.11", "")
	_, err := pool.Exec(ctx, `INSERT INTO node_ip_history(id,node_id,ts,from_ip,to_ip)
SELECT id,node_id,ts,NULL::inet,ip FROM results WHERE id='00000000-0000-4000-8000-000000000101'
UNION ALL SELECT id,node_id,ts,'203.0.113.10'::inet,'203.0.113.11'::inet FROM results WHERE id='00000000-0000-4000-8000-000000000102'
UNION ALL SELECT id,node_id,ts,NULL::inet,'203.0.113.11'::inet FROM results WHERE id='00000000-0000-4000-8000-000000000103'`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO alert_events(id,node_id,severity,ts,title,detail,trigger_result_id) VALUES
('00000000-0000-4000-8000-000000000201',$1,'info',$2,'Linked alert','Carrier B changed','00000000-0000-4000-8000-000000000103'),
('00000000-0000-4000-8000-000000000202',$1,'warn',$3,'Unlinked alert','No triggering result',NULL)`, statsEventNodeID, now.Add(-7*time.Hour), now.Add(-5*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
}

func integrationPasswordHash(t *testing.T) string {
	t.Helper()
	hash, err := auth.HashPassword("integration test owner password")
	if err != nil {
		t.Fatal("could not prepare integration owner")
	}
	return hash
}

func insertStatsEventResult(t *testing.T, pool *pgxpool.Pool, ctx context.Context, id string, ts time.Time, ip, isp string) {
	t.Helper()
	_, err := pool.Exec(ctx, `INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,server,ip,threads,engine,engine_isp)
VALUES ($1,$2,$3,'ok',100,20,10,1,'{"name":"Fixture"}'::jsonb,$4,4,'ookla',NULLIF($5,''))`, id, statsEventNodeID, ts, ip, isp)
	if err != nil {
		t.Fatal(err)
	}
}

func newStatisticsIntegrationHandler(t *testing.T, pool *pgxpool.Pool, ctx context.Context) http.Handler {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,email,password_hash) VALUES ('00000000-0000-4000-8000-000000000901','owner@example.test',$1)`, integrationPasswordHash(t)); err != nil {
		t.Fatal(err)
	}
	store, err := auth.NewPostgresStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	service, err := auth.NewService(store, auth.Config{SessionTTL: time.Hour, LoginMaxAttempts: 5, LoginWindow: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return NewHandler(Options{Auth: service, OwnerStats: &stats.Store{DB: pool}})
}

func hasStatsEvent(events []stats.Event, kind, fragment string) bool {
	for _, item := range events {
		if item.Type == kind && strings.Contains(item.Label, fragment) {
			return true
		}
	}
	return false
}

func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	location, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return location
}
