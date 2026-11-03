//go:build integration

package ipapi

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/shared/config"
)

func openPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	values, err := godotenv.Read(filepath.Join("..", "..", "..", ".env"))
	if err != nil {
		t.Fatal("a configured worktree .env is required")
	}
	for k, v := range values {
		t.Setenv(k, v)
	}
	cfg, err := config.LoadCenter()
	if err != nil {
		t.Fatal("invalid test configuration")
	}
	if err := db.CheckTestDatabase(cfg.Database.Name); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	return pool, ctx
}

const (
	nodeA = "00000000-0000-4000-8000-0000000000a1"
	nodeB = "00000000-0000-4000-8000-0000000000a2"
	nodeC = "00000000-0000-4000-8000-0000000000a3"
)

func fixture(t *testing.T) (*Store, context.Context) {
	t.Helper()
	pool, ctx := openPool(t)
	base := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	for _, q := range []string{
		`INSERT INTO nodes (id,name,expected_down_mbps,expected_up_mbps) VALUES ('` + nodeA + `','Alpha',100,50),('` + nodeB + `','Bravo',100,50),('` + nodeC + `','Charlie',100,50)`,
		`INSERT INTO ip_addresses (ip,geo,source,looked_up_at) VALUES
			('203.0.113.5','{"city":"Hanoi","country":"VN","latitude":21.03,"longitude":105.85,"time_zone":"Asia/Ho_Chi_Minh","asn":7552,"organisation":"Viettel"}','GeoLite2',now()),
			('203.0.113.6','{"city":"Da Nang","country":"VN","latitude":16.05,"longitude":108.2,"asn":18403,"organisation":"FPT"}','ip-api',now()),
			('2001:db8::1','{}','',NULL),
			('198.51.100.9','{"city":"Singapore","country":"SG","latitude":1.35,"longitude":103.82}','GeoLite2',now())`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	// Alpha: .5 -> .6 -> .5; Bravo: only 2001:db8::1 (no change); Charlie never reported.
	for i, h := range []struct {
		node, from, to string
		at             time.Duration
	}{
		{nodeA, "", "203.0.113.5", 1}, {nodeA, "203.0.113.5", "203.0.113.6", 2}, {nodeA, "203.0.113.6", "203.0.113.5", 3},
		{nodeB, "", "2001:db8::1", 2}, {nodeA, "203.0.113.5", "198.51.100.9", 4},
	} {
		var from any
		if h.from != "" {
			from = h.from
		}
		if _, err := pool.Exec(ctx, `INSERT INTO node_ip_history (id,node_id,ts,from_ip,to_ip) VALUES ($1,$2,$3,$4,$5)`,
			"00000000-0000-4000-8000-0000000001"+string(rune('0'+i))+"0", h.node, base.Add(h.at*time.Hour), from, h.to); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range []struct {
		node, ip     string
		tests, speed int
		first, last  time.Duration
		down, up     float64
	}{
		{nodeA, "203.0.113.5", 4, 3, 1, 3, 600, 150}, {nodeA, "203.0.113.6", 1, 1, 2, 2, 90, 20}, {nodeA, "198.51.100.9", 2, 0, 4, 4, 0, 0},
		{nodeB, "2001:db8::1", 5, 5, 2, 2, 500, 100},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO node_ip_addresses (node_id,ip,tests,first_seen,last_seen,speed_tests,down_sum,up_sum) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			a.node, a.ip, a.tests, base.Add(a.first*time.Hour), base.Add(a.last*time.Hour), a.speed, a.down, a.up); err != nil {
			t.Fatal(err)
		}
	}
	return &Store{DB: pool}, ctx
}

func TestHistoryListsChangesNewestFirstWithLocations(t *testing.T) {
	s, ctx := fixture(t)
	items, total, err := s.History(ctx, "", 1, 25)
	if err != nil || total != 3 || len(items) != 3 {
		t.Fatalf("history: %d items total %d err %v (the first address of a node is not a change)", len(items), total, err)
	}
	newest := items[0]
	if newest["to"] != "198.51.100.9" || newest["from"] != "203.0.113.5" || newest["nodeId"] != nodeA {
		t.Fatalf("newest = %v", newest)
	}
	if to := newest["toGeo"].(map[string]any); to["city"] != "Singapore" || to["source"] != "GeoLite2" || to["countryCode"] != "SG" {
		t.Fatalf("toGeo = %v", newest["toGeo"])
	}
	if items[1]["from"] != "203.0.113.6" || items[2]["fromGeo"].(map[string]any)["city"] != "Hanoi" {
		t.Fatalf("order/from locations = %v %v", items[1], items[2])
	}
	if filtered, total, _ := s.History(ctx, nodeB, 1, 25); len(filtered) != 0 || total != 0 {
		t.Fatalf("Bravo never changed address: %v", filtered)
	}
	page, total, _ := s.History(ctx, nodeA, 2, 2)
	if total != 3 || len(page) != 1 || page[0]["to"] != "203.0.113.6" {
		t.Fatalf("page 2 of size 2 = %v total %d", page, total)
	}
}

func TestAddressesListAveragesAndUnknownLocations(t *testing.T) {
	s, ctx := fixture(t)
	items, total, err := s.Addresses(ctx, "", 1, 25)
	if err != nil || total != 4 || len(items) != 4 {
		t.Fatalf("addresses: %d total %d err %v", len(items), total, err)
	}
	if items[0]["ip"] != "198.51.100.9" || items[0]["avgDown"] != nil || items[0]["avgUp"] != nil || items[0]["tests"] != int64(2) {
		t.Fatalf("failed-only address has no averages: %v", items[0])
	}
	var hanoi, v6 map[string]any
	for _, it := range items {
		switch it["ip"] {
		case "203.0.113.5":
			hanoi = it
		case "2001:db8::1":
			v6 = it
		}
	}
	if hanoi["avgDown"] != 200.0 || hanoi["avgUp"] != 50.0 || hanoi["tests"] != int64(4) || hanoi["first"] != "2026-11-03T01:00:00Z" || hanoi["last"] != "2026-11-03T03:00:00Z" {
		t.Fatalf("averages exclude failed tests: %v", hanoi)
	}
	if v6["geo"] != nil || v6["avgDown"] != 100.0 {
		t.Fatalf("an address without a lookup has geo null: %v", v6)
	}
	if only, total, _ := s.Addresses(ctx, nodeB, 1, 25); total != 1 || len(only) != 1 || only[0]["ip"] != "2001:db8::1" {
		t.Fatalf("node filter = %v", only)
	}
	if none, total, _ := s.Addresses(ctx, nodeC, 1, 25); total != 0 || len(none) != 0 || none == nil {
		t.Fatalf("a node without addresses: %v", none)
	}
}

func TestMapShowsEachNodesLatestAddressOnly(t *testing.T) {
	s, ctx := fixture(t)
	items, err := s.Map(ctx, "")
	if err != nil || len(items) != 2 {
		t.Fatalf("map = %v %v (Charlie never reported an address and is omitted)", items, err)
	}
	if items[0]["nodeId"] != nodeA || items[0]["ip"] != "198.51.100.9" || items[0]["geo"].(map[string]any)["city"] != "Singapore" {
		t.Fatalf("alpha = %v", items[0])
	}
	if items[1]["nodeId"] != nodeB || items[1]["geo"] != nil {
		t.Fatalf("an unknown location is geo null, never invented coordinates: %v", items[1])
	}
	if one, _ := s.Map(ctx, nodeB); len(one) != 1 {
		t.Fatalf("node filter = %v", one)
	}
}
