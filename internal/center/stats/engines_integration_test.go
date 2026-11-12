//go:build integration

package stats

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestEveryAggregationSeparatesEngines(t *testing.T) {
	pool, ctx, store := openStatsIntegrationStore(t)
	const node = "00000000-0000-4000-8000-000000000071"
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES ($1,'engines',1000,100)`, node); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ip_addresses(ip) VALUES ('203.0.113.71')`); err != nil {
		t.Fatal(err)
	}
	for i, engine := range []string{"ookla", "librespeed"} {
		if _, err := pool.Exec(ctx, `INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,threads,engine,ip,server) VALUES (gen_random_uuid(),$1,$2,'ok',$3,10,5,4,$4,'203.0.113.71','{"name":"Fixture"}')`, node, from.Add(time.Hour), 100*(i+1), engine); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO results(id,node_id,ts,status,threads,error,engine,failure_class) VALUES (gen_random_uuid(),$1,$2,'failed',4,'rate limited fixture','ookla','rate_limited')`, node, from.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "ookla", "librespeed"} {
		q := Query{Engine: name, From: from, To: from.Add(24 * time.Hour), PreviousFrom: from.Add(-24 * time.Hour), PreviousTo: from, Metric: Download, Timezone: "UTC", Bucket: "hour", Bins: 5}
		want := 100.0
		if name == "librespeed" {
			want = 200
		}
		wantCount, wantLimited := 1, 0
		if name != "librespeed" {
			wantCount, wantLimited = 2, 1
		}
		summary, err := store.Summary(ctx, q)
		if err != nil || len(summary) != 1 || summary[0].Current.Down.Mean == nil || *summary[0].Current.Down.Mean != want || summary[0].Current.Count != wantCount || summary[0].Current.RateLimited != wantLimited {
			t.Fatalf("summary %q: %+v %v", name, summary, err)
		}
		series, err := store.Series(ctx, q, false, 1)
		if err != nil || len(series) != 1 || series[0].Points[0].Value == nil || *series[0].Points[0].Value != want || series[0].Points[0].Count != 1 {
			t.Fatalf("series %q: %+v %v", name, series, err)
		}
		trend, err := store.Trend(ctx, q)
		if err != nil || len(trend) != 1 || trend[0].Summary.Mean == nil || *trend[0].Summary.Mean != want {
			t.Fatalf("trend %q: %+v %v", name, trend, err)
		}
		hours, err := store.HourOfDay(ctx, q)
		if err != nil || len(hours) != 1 || hours[0].Hours[1].Value == nil || *hours[0].Hours[1].Value != want || hours[0].Hours[1].Count != 1 {
			t.Fatalf("hours %q: %+v %v", name, hours, err)
		}
		heat, err := store.Heatmap(ctx, q, node)
		if err != nil {
			t.Fatalf("heat %q: %+v %v", name, heat, err)
		}

		samples := 0
		for _, cell := range heat.Cells {
			if cell.Count > 0 {
				samples += cell.Count
				if cell.Value == nil || *cell.Value != want {
					t.Fatalf("heat mixed engines %q: %+v", name, cell)
				}
			}
		}
		if samples != 1 {
			t.Fatalf("heat samples %q: %d", name, samples)
		}
		dist, err := store.Distribution(ctx, q)
		if err != nil || len(dist) != 1 || dist[0].Count != 1 {
			t.Fatalf("distribution %q: %+v %v", name, dist, err)
		}
	}
	t.Log("PASS: summary, series, trend, hour-of-day, heatmap and distribution accept independent engines; omitted filter selects Ookla")
}

func TestStatisticsISPFilterUsesResultThenGeoAndScopesAllAggregates(t *testing.T) {
	pool, ctx, store := openStatsIntegrationStore(t)
	const node = "00000000-0000-4000-8000-000000000081"
	from := time.Now().UTC().Truncate(time.Hour).Add(-6 * time.Hour)
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES ($1,'isp',1000,100)`, node); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ip_addresses(ip,geo) VALUES ('203.0.113.81','{"organisation":"Geo ISP"}'),('203.0.113.82','{}')`); err != nil {
		t.Fatal(err)
	}
	seedISPResult(t, pool, ctx, node, "00000000-0000-4000-8000-000000000811", from.Add(time.Hour), "203.0.113.81", "ookla", " Fast ISP ", 100)
	seedISPResult(t, pool, ctx, node, "00000000-0000-4000-8000-000000000812", from.Add(2*time.Hour), "203.0.113.81", "ookla", "", 200)
	seedISPResult(t, pool, ctx, node, "00000000-0000-4000-8000-000000000813", from.Add(3*time.Hour), "203.0.113.82", "ookla", "", 300)
	seedISPResult(t, pool, ctx, node, "00000000-0000-4000-8000-000000000814", from.Add(4*time.Hour), "203.0.113.81", "librespeed", "Fast ISP", 900)
	q := Query{NodeIDs: []string{node}, NodesSet: true, Engine: "ookla", ISP: "fast isp", From: from, To: from.Add(6 * time.Hour), PreviousFrom: from.Add(-6 * time.Hour), PreviousTo: from, Metric: Download, Bucket: "hour", Timezone: "UTC", Bins: 2}
	verifyFilteredAggregates(t, store, q, node, 100)
	q.Engine = "librespeed"
	verifyFilteredAggregates(t, store, q, node, 900)
	q.Engine = "ookla"
	q.ISP = "geo isp"
	verifyFilteredAggregates(t, store, q, node, 200)
	q.ISP = "unknown"
	noResults, err := store.Summary(ctx, q)
	if err != nil || len(noResults) != 1 || noResults[0].Current.Count != 0 || noResults[0].Current.Down.Mean != nil {
		t.Fatalf("unknown ISP filter = %+v err=%v", noResults, err)
	}
}

func TestHeatmapCombinesRepeatedDSTHourAndKeepsEmptyCells(t *testing.T) {
	pool, ctx, store := openStatsIntegrationStore(t)
	const node = "00000000-0000-4000-8000-000000000082"
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES ($1,'DST heatmap',100,20)`, node); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ip_addresses(ip) VALUES ('203.0.113.83')`); err != nil {
		t.Fatal(err)
	}
	for i, ts := range []time.Time{time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC), time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC)} {
		if _, err := pool.Exec(ctx, `INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,server,ip,threads)
VALUES (gen_random_uuid(),$1,$2,'ok',$3,20,10,1,'{"name":"Fixture"}'::jsonb,'203.0.113.83',4)`, node, ts, 100*(i+1)); err != nil {
			t.Fatal(err)
		}
	}
	query := Query{Engine: "ookla", From: time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC),
		To: time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC), Metric: Download, Timezone: "America/New_York"}
	heat, err := store.Heatmap(ctx, query, node)
	if err != nil || len(heat.Cells) != 168 {
		t.Fatalf("DST heatmap cells=%d err=%v", len(heat.Cells), err)
	}
	fold := heat.Cells[6*24+1]
	if fold.Count != 2 || fold.Value == nil || *fold.Value != 150 {
		t.Fatalf("repeated fall-back hour = %+v", fold)
	}
	gap := heat.Cells[6*24+2]
	if gap.Count != 0 || gap.Value != nil {
		t.Fatalf("empty local hour = %+v", gap)
	}
}

func TestStatisticsIPEventsFollowSelectedEngineAcrossNullsAndWindowEdges(t *testing.T) {
	pool, ctx, store := openStatsIntegrationStore(t)
	const firstNode = "00000000-0000-4000-8000-000000000083"
	const secondNode = "00000000-0000-4000-8000-000000000084"
	for _, node := range []string{firstNode, secondNode} {
		if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES ($1,$2,100,20)`, node, node); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ip_addresses(ip) SELECT ('203.0.113.' || octet)::inet FROM generate_series(1,7) AS octet`); err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(8 * time.Hour)
	rows := []struct {
		id, node string
		offset   time.Duration
		engine   string
		ip       any
	}{
		{"00000000-0000-4000-8000-000000000831", firstNode, -time.Hour, "ookla", "203.0.113.1"},
		{"00000000-0000-4000-8000-000000000832", firstNode, 0, "ookla", "203.0.113.1"},
		{"00000000-0000-4000-8000-000000000833", firstNode, time.Hour, "librespeed", "203.0.113.2"},
		{"00000000-0000-4000-8000-000000000834", firstNode, 2 * time.Hour, "ookla", "203.0.113.1"},
		{"00000000-0000-4000-8000-000000000835", firstNode, 3 * time.Hour, "ookla", "203.0.113.1"},
		{"00000000-0000-4000-8000-000000000836", firstNode, 4 * time.Hour, "librespeed", "203.0.113.2"},
		{"00000000-0000-4000-8000-000000000837", firstNode, 5 * time.Hour, "ookla", "203.0.113.2"},
		{"00000000-0000-4000-8000-000000000838", firstNode, 6 * time.Hour, "ookla", nil},
		{"00000000-0000-4000-8000-000000000839", firstNode, 7 * time.Hour, "ookla", "203.0.113.2"},
		{"00000000-0000-4000-8000-00000000083a", firstNode, 8 * time.Hour, "ookla", "203.0.113.3"},
		{"00000000-0000-4000-8000-000000000841", secondNode, -time.Second, "ookla", "203.0.113.4"},
		{"00000000-0000-4000-8000-000000000842", secondNode, 0, "ookla", "203.0.113.5"},
		{"00000000-0000-4000-8000-000000000843", secondNode, time.Hour, "ookla", nil},
		{"00000000-0000-4000-8000-000000000844", secondNode, 2 * time.Hour, "ookla", "203.0.113.6"},
		{"00000000-0000-4000-8000-000000000845", secondNode, 8 * time.Hour, "ookla", "203.0.113.7"},
	}
	for _, row := range rows {
		seedIPTransitionResult(t, pool, ctx, row.id, row.node, from.Add(row.offset), row.engine, row.ip)
	}
	query := Query{NodeIDs: []string{firstNode, secondNode}, NodesSet: true, Engine: "ookla", From: from, To: to}
	got, err := store.Events(ctx, query)
	if err != nil || len(got.Items) != 3 || got.Truncated {
		t.Fatalf("engine-scoped IP events = %+v err=%v", got, err)
	}
	want := []Event{
		{ID: "00000000-0000-4000-8000-000000000837", NodeID: firstNode, TS: from.Add(5 * time.Hour), Type: "ip_change", Label: "IP 203.0.113.1 → 203.0.113.2"},
		{ID: "00000000-0000-4000-8000-000000000844", NodeID: secondNode, TS: from.Add(2 * time.Hour), Type: "ip_change", Label: "IP 203.0.113.5 → 203.0.113.6"},
		{ID: "00000000-0000-4000-8000-000000000842", NodeID: secondNode, TS: from, Type: "ip_change", Label: "IP 203.0.113.4 → 203.0.113.5"},
	}
	for i := range want {
		actual, expected := got.Items[i], want[i]
		if actual.ID != expected.ID || actual.NodeID != expected.NodeID || !actual.TS.Equal(expected.TS) || actual.Type != expected.Type || actual.Label != expected.Label {
			t.Fatalf("IP event[%d] = %+v, want %+v", i, got.Items[i], want[i])
		}
	}
	query.Engine = "librespeed"
	otherEngine, err := store.Events(ctx, query)
	if err != nil || len(otherEngine.Items) != 0 {
		t.Fatalf("LibreSpeed IP events = %+v err=%v; IP changes from Ookla rows must not leak", otherEngine, err)
	}
}

func seedIPTransitionResult(t *testing.T, pool *pgxpool.Pool, ctx context.Context, id, node string, ts time.Time, engine string, ip any) {
	t.Helper()
	status, measured, serverID, server, failure := "ok", any(100), any(1), any(`{"name":"Fixture"}`), any(nil)
	if ip == nil {
		status, measured, serverID, server, failure = "failed", nil, nil, nil, "fixture failure"
	}
	_, err := pool.Exec(ctx, `INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,server,ip,threads,engine,engine_isp,error)
VALUES ($1,$2,$3,$4,$5,$5,$5,$6,$7::jsonb,$8,4,$9,'Carrier A',$10)`, id, node, ts, status, measured, serverID, server, ip, engine, failure)
	if err != nil {
		t.Fatal(err)
	}
}

func seedISPResult(t *testing.T, pool *pgxpool.Pool, ctx context.Context, node, id string, ts time.Time, ip, engine, isp string, down float64) {
	t.Helper()
	_, err := pool.Exec(ctx, `INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,server,ip,threads,engine,engine_isp)
VALUES ($1,$2,$3,'ok',$4,20,5,1,'{"name":"Fixture"}'::jsonb,$5,4,$6,NULLIF($7,''))`, id, node, ts, down, ip, engine, isp)
	if err != nil {
		t.Fatal(err)
	}
}

func verifyFilteredAggregates(t *testing.T, store *Store, q Query, node string, want float64) {
	t.Helper()
	summary, err := store.Summary(t.Context(), q)
	if err != nil || len(summary) != 1 || summary[0].Current.Count != 1 || summary[0].Current.Down.Mean == nil || *summary[0].Current.Down.Mean != want {
		t.Fatalf("ISP summary = %+v err=%v", summary, err)
	}
	trend, err := store.Trend(t.Context(), q)
	if err != nil || len(trend) != 1 || trend[0].Summary.N != 1 || trend[0].Summary.P50 == nil || *trend[0].Summary.P50 != want {
		t.Fatalf("ISP trend = %+v err=%v", trend, err)
	}
	hours, err := store.HourOfDay(t.Context(), q)
	if err != nil || len(hours) != 1 || !hasOneHourSample(hours[0], want) {
		t.Fatalf("ISP hours = %+v err=%v", hours, err)
	}
	heat, err := store.Heatmap(t.Context(), q, node)
	if err != nil || len(heat.Cells) != 168 || !hasOneHeatSample(heat, want) {
		t.Fatalf("ISP heatmap = %+v err=%v", heat, err)
	}
	dist, err := store.Distribution(t.Context(), q)
	if err != nil || len(dist) != 1 || dist[0].Count != 1 {
		t.Fatalf("ISP distribution = %+v err=%v", dist, err)
	}
}

func hasOneHourSample(series HourSeries, want float64) bool {
	total := 0
	for _, hour := range series.Hours {
		total += hour.Count
		if hour.Count > 0 && (hour.Value == nil || *hour.Value != want) {
			return false
		}
	}
	return total == 1
}

func hasOneHeatSample(heat Heatmap, want float64) bool {
	total := 0
	for _, cell := range heat.Cells {
		total += cell.Count
		if cell.Count > 0 && (cell.Value == nil || *cell.Value != want) {
			return false
		}
	}
	return total == 1
}
