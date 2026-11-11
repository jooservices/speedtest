//go:build integration

package stats

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestStatisticsQueriesFilterMultipleISPsIntegration(t *testing.T) {
	pool, ctx, store := openStatsIntegrationStore(t)
	from := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	seedStatsISPs(t, ctx, pool, from)
	for _, fixture := range []struct {
		id   string
		want float64
	}{{statsISPA, 100}, {statsISPB, 900}} {
		q := Query{NodeIDs: []string{statsISPNode}, NodesSet: true, ISPIDs: []string{fixture.id}, ISPsSet: true,
			From: from, To: from.Add(6 * time.Hour), PreviousFrom: from.Add(-6 * time.Hour), PreviousTo: from,
			Metric: Download, Timezone: "UTC", Bucket: "hour", Bins: 2}
		assertStatsISPFilter(t, store, ctx, q, fixture.want)
	}
	t.Log("PASS: summary, series, trend, hour-of-day, heatmap and distribution isolate two ISP rows")
}

const (
	statsISPNode = "00000000-0000-4000-8000-0000000000e1"
	statsISPA    = "00000000-0000-4000-8000-0000000000e2"
	statsISPB    = "00000000-0000-4000-8000-0000000000e3"
)

func seedStatsISPs(t *testing.T, ctx context.Context, pool *pgxpool.Pool, from time.Time) {
	t.Helper()
	for _, query := range []string{
		`INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES ('` + statsISPNode + `','ISP-filter-node',1000,100)`,
		`INSERT INTO isps(id,asn,organisation,name) VALUES ('` + statsISPA + `',64510,'Provider A','Provider A'),('` + statsISPB + `',64511,'Provider B','Provider B')`,
		`INSERT INTO ip_addresses(ip,isp_id) VALUES ('203.0.113.201','` + statsISPA + `'),('203.0.113.202','` + statsISPB + `')`,
	} {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatal("seed ISP filter data:", err)
		}
	}
	for i, fixture := range []struct {
		id, ispID, ip string
		down          float64
	}{{"00000000-0000-4000-8000-0000000000e4", statsISPA, "203.0.113.201", 100}, {"00000000-0000-4000-8000-0000000000e5", statsISPB, "203.0.113.202", 900}} {
		if _, err := pool.Exec(ctx, `INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,jitter_ms,loss_percent,server_id,server,ip,threads,isp_id)
			VALUES ($1,$2,$3,'ok',$4,10,20,2,0.1,1,'{"id":1,"name":"fixture"}'::jsonb,$5,4,$6)`, fixture.id, statsISPNode, from.Add(time.Hour+time.Duration(i)*time.Minute), fixture.down, fixture.ip, fixture.ispID); err != nil {
			t.Fatal("seed ISP result:", err)
		}
	}
}

func assertStatsISPFilter(t *testing.T, store *Store, ctx context.Context, q Query, want float64) {
	t.Helper()
	assertFilteredSummary(t, store, ctx, q, want)
	assertFilteredSeries(t, store, ctx, q, want)
	assertFilteredTrend(t, store, ctx, q, want)
	assertFilteredHours(t, store, ctx, q, want)
	assertFilteredHeatmap(t, store, ctx, q, want)
	assertFilteredDistribution(t, store, ctx, q)
}

func assertFilteredSummary(t *testing.T, store *Store, ctx context.Context, q Query, want float64) {
	t.Helper()
	rows, err := store.Summary(ctx, q)
	if err != nil || len(rows) != 1 || rows[0].Current.Count != 1 || rows[0].Current.Down.Mean == nil || *rows[0].Current.Down.Mean != want {
		t.Fatalf("summary filtered to %v: %+v %v", q.ISPIDs, rows, err)
	}
}

func assertFilteredSeries(t *testing.T, store *Store, ctx context.Context, q Query, want float64) {
	t.Helper()
	rows, err := store.Series(ctx, q, false, 2)
	if err != nil || len(rows) != 1 || rows[0].Summary.N != 1 || *rows[0].Summary.Mean != want {
		t.Fatalf("series filtered to %v: %+v %v", q.ISPIDs, rows, err)
	}
}

func assertFilteredTrend(t *testing.T, store *Store, ctx context.Context, q Query, want float64) {
	t.Helper()
	rows, err := store.Trend(ctx, q)
	if err != nil || len(rows) != 1 || rows[0].Summary.N != 1 || *rows[0].Summary.Mean != want {
		t.Fatalf("trend filtered to %v: %+v %v", q.ISPIDs, rows, err)
	}
}

func assertFilteredHours(t *testing.T, store *Store, ctx context.Context, q Query, want float64) {
	t.Helper()
	rows, err := store.HourOfDay(ctx, q)
	if err != nil || len(rows) != 1 || rows[0].Hours[1].Count != 1 || rows[0].Hours[1].Value == nil || *rows[0].Hours[1].Value != want {
		t.Fatalf("hour-of-day filtered to %v: %+v %v", q.ISPIDs, rows, err)
	}
}

func assertFilteredHeatmap(t *testing.T, store *Store, ctx context.Context, q Query, want float64) {
	t.Helper()
	heat, err := store.Heatmap(ctx, q, statsISPNode)
	if err != nil {
		t.Fatal("heatmap filtered query:", err)
	}
	for _, cell := range heat.Cells {
		if cell.Count == 1 && cell.Value != nil && *cell.Value == want {
			return
		}
	}
	t.Fatalf("heatmap lacks ISP value %.0f: %+v", want, heat.Cells)
}

func assertFilteredDistribution(t *testing.T, store *Store, ctx context.Context, q Query) {
	t.Helper()
	rows, err := store.Distribution(ctx, q)
	if err != nil || len(rows) != 1 || rows[0].Count != 1 {
		t.Fatalf("distribution filtered to %v: %+v %v", q.ISPIDs, rows, err)
	}
}
