//go:build integration

package stats

import (
	"fmt"
	"testing"
	"time"
)

func TestSeriesBucketsGapsPercentAndSummary(t *testing.T) {
	pool, ctx, store := openStatsIntegrationStore(t)
	from := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	to := from.Add(4 * time.Hour)
	a, b := "00000000-0000-4000-8000-0000000000a1", "00000000-0000-4000-8000-0000000000a2"
	for _, q := range []string{
		`INSERT INTO nodes (id,name,expected_down_mbps,expected_up_mbps) VALUES ('` + a + `','Alpha',1000,100)`,
		`INSERT INTO nodes (id,name,expected_down_mbps,expected_up_mbps) VALUES ('` + b + `','Bravo',200,50)`,
		`INSERT INTO ip_addresses(ip) VALUES ('203.0.113.5')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	seed := func(node string, at time.Time, status string, down any) {
		t.Helper()
		n++
		var serverJSON, errText any = `{"id":9,"name":"S","city":"C","sponsor":"P"}`, nil
		var up any = 10.0
		if status == "failed" {
			serverJSON, errText, up = nil, "boom", nil
		}
		if _, err := pool.Exec(ctx, `INSERT INTO results (id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,server,ip,threads,error)
			VALUES ($1,$2,$3,$4,$5,$6,10,9,$7::jsonb,'203.0.113.5',4,$8)`, fmt.Sprintf("00000000-0000-4000-8000-%012x", n), node, at, status, down, up, serverJSON, errText); err != nil {
			t.Fatal(err)
		}
	}
	// 4 hours in 4 buckets of one hour: Alpha has samples in buckets 0 and 2, a failure in bucket 1 and nothing in 3.
	seed(a, from.Add(10*time.Minute), "ok", 400.0)
	seed(a, from.Add(40*time.Minute), "degraded", 600.0)
	seed(a, from.Add(70*time.Minute), "failed", nil)
	seed(a, from.Add(130*time.Minute), "ok", 800.0)
	seed(b, from.Add(10*time.Minute), "ok", 100.0)
	// Outside the window and exactly at the end: excluded.
	seed(a, from.Add(-time.Minute), "ok", 999.0)
	seed(a, to, "ok", 999.0)

	items, err := store.Series(ctx, Query{From: from, To: to, Metric: Download}, false, 4)
	if err != nil || len(items) != 2 {
		t.Fatalf("series: %v %v", items, err)
	}
	alpha := items[0]
	if alpha.Name != "Alpha" || alpha.Unit != "Mbps" || len(alpha.Points) != 4 {
		t.Fatalf("alpha = %+v", alpha)
	}
	wantValues := []*float64{ptr(500), nil, ptr(800), nil}
	wantCounts := []int{2, 0, 1, 0}
	for i, p := range alpha.Points {
		if !p.TS.Equal(from.Add(time.Duration(i)*time.Hour)) || p.Count != wantCounts[i] || (p.Value == nil) != (wantValues[i] == nil) || (p.Value != nil && *p.Value != *wantValues[i]) {
			t.Errorf("point %d = %+v, want value %v count %d", i, p, wantValues[i], wantCounts[i])
		}
	}
	// The summary uses the three original samples (400, 600, 800), not the bucket means.
	if alpha.Summary.N != 3 || *alpha.Summary.Mean != 600 || *alpha.Summary.Min != 400 || *alpha.Summary.Max != 800 || *alpha.Summary.Median != 600 {
		t.Fatalf("alpha summary = %+v", alpha.Summary)
	}

	percent, err := store.Series(ctx, Query{From: from, To: to, Metric: Download}, true, 4)
	if err != nil || percent[0].Unit != "%" {
		t.Fatalf("percent series: %v %v", percent, err)
	}
	// Alpha expects 1000 Mbps and Bravo 200: the same 100 Mbps is 10% for one and 50% for the other.
	if *percent[0].Points[0].Value != 50 || *percent[0].Points[2].Value != 80 || *percent[1].Points[0].Value != 50 {
		t.Fatalf("percent values: alpha %+v bravo %+v", percent[0].Points, percent[1].Points)
	}
	if *percent[0].Summary.Mean != 60 {
		t.Fatalf("percent summary = %+v", percent[0].Summary)
	}

	one, err := store.Series(ctx, Query{From: from, To: to, Metric: Upload, NodeIDs: []string{b}, NodesSet: true}, false, 2)
	if err != nil || len(one) != 1 || len(one[0].Points) != 2 || *one[0].Points[0].Value != 10 || one[0].Points[1].Value != nil {
		t.Fatalf("selected node: %+v %v", one, err)
	}
	ping, err := store.Series(ctx, Query{From: from, To: to, Metric: Ping}, false, 1)
	if err != nil || len(ping[0].Points) != 1 || ping[0].Points[0].Count != 3 || *ping[0].Points[0].Value != 10 {
		t.Fatalf("one bucket for the whole window: %+v %v", ping, err)
	}
	if _, err := store.Series(ctx, Query{From: from, To: to, Metric: Ping}, true, 4); err == nil {
		t.Fatal("percent ping accepted")
	}
}

func ptr(v float64) *float64 { return &v }
