//go:build integration

package stats

import (
	"testing"
	"time"
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
