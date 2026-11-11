//go:build integration

package ownerapi

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type fixture struct {
	store      *Store
	alpha, bra string
	now        time.Time
}

func seedFixture(t *testing.T) (context.Context, *pgxpool.Pool, fixture) {
	t.Helper()
	pool, ctx := openPool(t)
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	fresh := now
	fx := fixture{store: &Store{DB: pool, Now: func() time.Time { return now }}, now: now}
	fx.alpha = seedNode(t, ctx, pool, 1, "Alpha", &fresh)
	fx.bra = seedNode(t, ctx, pool, 2, "bravo", &fresh)
	seedResult(t, ctx, pool, 1, fx.alpha, now.Add(-10*time.Minute), "ok", "203.0.113.5")
	seedResult(t, ctx, pool, 2, fx.alpha, now.Add(-5*time.Minute), "failed", "203.0.113.7")
	seedResult(t, ctx, pool, 3, fx.bra, now.Add(-time.Minute), "degraded", "203.0.113.6")
	seedResult(t, ctx, pool, 4, fx.bra, now.Add(-2*time.Hour), "ok", "203.0.113.5")
	// Different speeds, one server in another city and one address without a stored location.
	if _, err := pool.Exec(ctx, `UPDATE results SET down_mbps = 900, ping_ms = 5, server = '{"id":2,"name":"FPT","city":"Da Nang","sponsor":"FPT"}' WHERE id = $1`, uid(1004)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE results SET down_mbps = 40 WHERE id = $1`, uid(1003)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ip_addresses (ip,geo,source) VALUES ('198.51.100.9','{}','none')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE results SET ip = '198.51.100.9' WHERE id = $1`, uid(1003)); err != nil {
		t.Fatal(err)
	}
	return ctx, pool, fx
}

func ids(items []map[string]any) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it["id"].(string)
	}
	return out
}

func TestListResultsFiltersSortsAndPaginates(t *testing.T) {
	ctx, pool, fx := seedFixture(t)
	s := fx.store
	all := ResultFilter{From: fx.now.Add(-24 * time.Hour), To: fx.now.Add(time.Second)}
	seedResultISPs(t, ctx, pool)

	items, total, err := s.ListResults(ctx, all, 1, 2)
	if err != nil || total != 4 || len(items) != 2 || items[0]["id"] != uid(1003) || items[1]["id"] != uid(1002) {
		t.Fatalf("default order newest first: %v total %d err %v", ids(items), total, err)
	}
	if rest, _, _ := s.ListResults(ctx, all, 2, 2); len(rest) != 2 || rest[1]["id"] != uid(1004) {
		t.Fatalf("page 2 = %v", ids(rest))
	}
	if beyond, total, err := s.ListResults(ctx, all, 9, 2); err != nil || len(beyond) != 0 || total != 4 {
		t.Fatalf("beyond the last page = %d total %d err %v", len(beyond), total, err)
	}

	cases := []struct {
		name string
		f    func(ResultFilter) ResultFilter
		want []string
	}{
		{"one node", func(f ResultFilter) ResultFilter { f.NodesSet, f.Nodes = true, []string{fx.alpha}; return f }, []string{uid(1002), uid(1001)}},
		{"no node selected", func(f ResultFilter) ResultFilter { f.NodesSet = true; return f }, nil},
		{"one ISP", func(f ResultFilter) ResultFilter { f.ISPsSet, f.ISPs = true, []string{uid(5001)}; return f }, []string{uid(1002), uid(1001), uid(1004)}},
		{"another ISP", func(f ResultFilter) ResultFilter { f.ISPsSet, f.ISPs = true, []string{uid(5002)}; return f }, []string{uid(1003)}},
		{"status failed", func(f ResultFilter) ResultFilter { f.Status = "failed"; return f }, []string{uid(1002)}},
		{"status all", func(f ResultFilter) ResultFilter { f.Status = "all"; return f }, []string{uid(1003), uid(1002), uid(1001), uid(1004)}},
		{"time range excludes older", func(f ResultFilter) ResultFilter { f.From = fx.now.Add(-30 * time.Minute); return f }, []string{uid(1003), uid(1002), uid(1001)}},
		{"min download skips nulls", func(f ResultFilter) ResultFilter { v := 100.0; f.MinDown = &v; return f }, []string{uid(1001), uid(1004)}},
		{"max download", func(f ResultFilter) ResultFilter { v := 100.0; f.MaxDown = &v; return f }, []string{uid(1003)}},
		{"search server city", func(f ResultFilter) ResultFilter { f.Query = "da nang"; return f }, []string{uid(1004)}},
		{"search ip", func(f ResultFilter) ResultFilter { f.Query = "203.0.113.7"; return f }, []string{uid(1002)}},
		{"search location city", func(f ResultFilter) ResultFilter { f.Query = "HANOI"; f.Sort, f.Dir = "ts", "asc"; return f }, []string{uid(1004), uid(1001), uid(1002)}},
		{"search error", func(f ResultFilter) ResultFilter { f.Query = "link down"; return f }, []string{uid(1002)}},
		{"search node name", func(f ResultFilter) ResultFilter { f.Query = "BRAV"; return f }, []string{uid(1003), uid(1004)}},
		{"wildcards are literal", func(f ResultFilter) ResultFilter { f.Query = "%"; return f }, nil},
		{"injection text is literal", func(f ResultFilter) ResultFilter { f.Query = "x' OR '1'='1"; return f }, nil},
		{"sort download asc nulls last", func(f ResultFilter) ResultFilter { f.Sort, f.Dir = "down", "asc"; return f }, []string{uid(1003), uid(1001), uid(1004), uid(1002)}},
		{"sort download desc nulls last", func(f ResultFilter) ResultFilter { f.Sort, f.Dir = "down", "desc"; return f }, []string{uid(1004), uid(1001), uid(1003), uid(1002)}},
		{"sort by node then id", func(f ResultFilter) ResultFilter { f.Sort, f.Dir = "node", "asc"; return f }, []string{uid(1001), uid(1002), uid(1003), uid(1004)}},
		{"sort by server city", func(f ResultFilter) ResultFilter { f.Sort, f.Dir = "server", "asc"; return f }, []string{uid(1001), uid(1002), uid(1003), uid(1004)}},
		{"sort by status", func(f ResultFilter) ResultFilter { f.Sort, f.Dir = "status", "asc"; return f }, []string{uid(1003), uid(1002), uid(1001), uid(1004)}},
		{"sort by ip", func(f ResultFilter) ResultFilter { f.Sort, f.Dir = "ip", "asc"; return f }, []string{uid(1003), uid(1001), uid(1004), uid(1002)}},
	}
	// "ok" rows 1001 and 1004 tie on status, so only the order of the unambiguous cases is asserted strictly.
	for _, tc := range cases {
		got, total, err := s.ListResults(ctx, tc.f(all), 1, 100)
		if err != nil || total != len(tc.want) || len(got) != len(tc.want) {
			t.Errorf("%s: got %v total %d err %v, want %v", tc.name, ids(got), total, err, tc.want)
			continue
		}
		for i, id := range ids(got) {
			if id != tc.want[i] {
				t.Errorf("%s: got %v, want %v", tc.name, ids(got), tc.want)
				break
			}
		}
	}
	if _, _, err := s.ListResults(ctx, ResultFilter{Sort: "bogus"}, 1, 10); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad sort = %v", err)
	}
}

func seedResultISPs(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, query := range []string{
		`INSERT INTO isps(id,asn,organisation,name) VALUES ('` + uid(5001) + `',64501,'Results ISP A','Results ISP A'),('` + uid(5002) + `',64502,'Results ISP B','Results ISP B')`,
		`UPDATE results SET isp_id='` + uid(5001) + `' WHERE id IN ('` + uid(1001) + `','` + uid(1002) + `','` + uid(1004) + `')`,
		`UPDATE results SET isp_id='` + uid(5002) + `' WHERE id='` + uid(1003) + `'`,
	} {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatal("seed result ISP filter:", err)
		}
	}
}

func TestGetResultReturnsEveryFieldAndLocation(t *testing.T) {
	ctx, _, fx := seedFixture(t)
	r, err := fx.store.GetResult(ctx, uid(1001))
	if err != nil {
		t.Fatal(err)
	}
	geo, _ := r["geo"].(map[string]any)
	if r["status"] != "ok" || r["down"] != 250.5 || r["up"] != 90.25 || r["serverId"] != int64(9) || r["bytesDown"] != int64(1000) ||
		r["manual"] != false || r["raw"].(map[string]any)["k"] != "v" || r["ip"] != "203.0.113.5" {
		t.Fatalf("result = %v", r)
	}
	if geo["city"] != "Hanoi" || geo["country"] != "VN" || geo["asn"] != uint(7552) || geo["source"] != "GeoLite2" || geo["version"] != 4 {
		t.Fatalf("geo = %v", geo)
	}
	if noGeo, err := fx.store.GetResult(ctx, uid(1003)); err != nil || noGeo["geo"] != nil || noGeo["ip"] != "198.51.100.9" {
		t.Fatalf("address without a stored location: %v %v", noGeo, err)
	}
	if _, err := fx.store.GetResult(ctx, uid(9999)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id = %v", err)
	}
}

func TestExportResultsFollowsTheSameFilterAndStopsOnError(t *testing.T) {
	ctx, _, fx := seedFixture(t)
	f := ResultFilter{From: fx.now.Add(-24 * time.Hour), To: fx.now.Add(time.Second), Status: "ok", Sort: "ts", Dir: "asc"}
	var got []string
	err := fx.store.ExportResults(ctx, f, func(r ResultRow) error {
		got = append(got, r.Result["id"].(string)+"/"+r.NodeName)
		return nil
	})
	if err != nil || len(got) != 2 || got[0] != uid(1004)+"/bravo" || got[1] != uid(1001)+"/Alpha" {
		t.Fatalf("export = %v err %v", got, err)
	}
	stop := errors.New("stop")
	calls := 0
	if err := fx.store.ExportResults(ctx, f, func(ResultRow) error { calls++; return stop }); !errors.Is(err, stop) || calls != 1 {
		t.Fatalf("emit error = %v after %d calls", err, calls)
	}
}

func TestExportOfOneHundredThousandRowsStreams(t *testing.T) {
	ctx, pool, fx := seedFixture(t)
	if _, err := pool.Exec(ctx, `INSERT INTO results (id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,server,ip,threads,raw)
		SELECT gen_random_uuid(), $1, $2::timestamptz - (g || ' seconds')::interval, 'ok', 100 + g % 50, 50, 10, 9,
		'{"id":9,"name":"S","city":"C","sponsor":"P","padding":"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}', '203.0.113.5', 4, '{"k":"v"}'
		FROM generate_series(1, 100000) g`, fx.alpha, fx.now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	f := ResultFilter{From: fx.now.Add(-48 * time.Hour), To: fx.now.Add(time.Second)}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	rows := 0
	var peak uint64
	start := time.Now()
	err := fx.store.ExportResults(ctx, f, func(ResultRow) error {
		rows++
		if rows%10000 == 0 {
			runtime.ReadMemStats(&after)
			if after.HeapAlloc > peak {
				peak = after.HeapAlloc
			}
		}
		return nil
	})
	if err != nil || rows != 100004 {
		t.Fatalf("exported %d rows, err %v", rows, err)
	}
	growth := int64(peak) - int64(before.HeapAlloc)
	t.Logf("exported %d rows in %s; heap grew %d KiB", rows, time.Since(start).Round(time.Millisecond), growth/1024)
	if growth > 64<<20 {
		t.Fatalf("heap grew %d MiB while streaming; rows must not be buffered", growth>>20)
	}
}
