//go:build integration

package geo

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/shared/testenv"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	centerdb "github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/geo/budget"
	"github.com/jooservices/speedtest/internal/shared/config"
)

const backfillNodeID = "00000000-0000-4000-8000-000000000099"

func countGeoRows(t *testing.T, pool *pgxpool.Pool, ctx context.Context, query string, args ...any) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func integrationPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	values, err := godotenv.Read(filepath.Join("../../../", ".env"))
	if err != nil {
		t.Fatal("a configured worktree .env is required")
	}
	testenv.Apply(t, values)
	cfg, err := config.LoadCenter()
	if err != nil {
		t.Fatal("invalid test database configuration")
	}
	if err := centerdb.CheckTestDatabase(cfg.Database.Name); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	pool, err := centerdb.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var name, marker string
	if err := pool.QueryRow(ctx, "SELECT current_database(), shobj_description(oid,'pg_database') FROM pg_database WHERE datname=current_database()").Scan(&name, &marker); err != nil || name != cfg.Database.Name || marker != "workspaces-project:speedtest" {
		t.Fatal("test database identity/ownership verification failed")
	}
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	t.Log("PASS: database name and Workspaces project ownership marker verified")
	return pool, ctx
}

func TestWorktreeDatabaseGuard(t *testing.T) {
	_, _ = integrationPool(t)
}

func TestPostgresCacheRoundTrip(t *testing.T) {
	pool, ctx := integrationPool(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	cache := NewPostgresCache(tx)
	ip := integrationFixtureIP(uint64(time.Now().UnixNano()))
	if net.ParseIP(ip) == nil {
		t.Fatalf("invalid integration fixture IP %q", ip)
	}
	if _, _, found, err := cache.Get(ctx, ip); err != nil || found {
		t.Fatalf("initial cache state: found=%v err=%v", found, err)
	}
	want := Location{City: "Test City", Region: "Test Region", Country: "ZZ", Latitude: 10.5, Longitude: 20.25, ASN: 64500, Organisation: "Test Network"}
	if err := cache.Put(ctx, ip, want, "ip-api"); err != nil {
		t.Fatal(err)
	}
	got, source, found, err := cache.Get(ctx, ip)
	if err != nil || !found || source != "ip-api" || got.City != want.City || got.Region != want.Region || got.Country != want.Country ||
		got.Latitude != want.Latitude || got.Longitude != want.Longitude || got.ASN != want.ASN || got.Organisation != want.Organisation ||
		got.QualityScore != 90 || got.FieldSources.City != "ip-api" || got.FieldSources.Country != "ip-api" {
		t.Fatalf("cache round trip = %+v, source=%q, found=%v, err=%v", got, source, found, err)
	}
	if err := cache.Put(ctx, ip, Location{Country: "US"}, "ipinfo-lite"); err != nil {
		t.Fatal(err)
	}
	got, source, found, err = cache.Get(ctx, ip)
	if err != nil || !found || source != "merged:ip-api+ipinfo-lite" || got.Country != "US" || got.City != want.City ||
		got.FieldSources.City != "ip-api" || got.FieldSources.Country != "ipinfo-lite" {
		t.Fatalf("higher-ranked provider did not merge provenance: %+v, source=%q, err=%v", got, source, err)
	}
	var lookedUpAt time.Time
	if err := tx.QueryRow(ctx, "SELECT looked_up_at FROM ip_addresses WHERE ip=$1::inet", ip).Scan(&lookedUpAt); err != nil || lookedUpAt.IsZero() {
		t.Fatalf("lookup timestamp missing: %v", err)
	}
	t.Log("PASS: ip_addresses stores JSON location, source and lookup time; a cached IP is not overwritten")
}

func TestPostgresCacheUnlookedMalformedAndCancelled(t *testing.T) {
	pool, ctx := integrationPool(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	cache := NewPostgresCache(tx)
	base := uint64(time.Now().UnixNano())
	unlookedIP := integrationFixtureIP(base)
	malformedIP := integrationFixtureIP(base + 1)
	if _, err := tx.Exec(ctx, "INSERT INTO ip_addresses(ip) VALUES ($1::inet), ($2::inet)", unlookedIP, malformedIP); err != nil {
		t.Fatal("could not create transactional cache fixtures")
	}
	if _, err := tx.Exec(ctx, "UPDATE ip_addresses SET geo=$2::jsonb, source='fixture', looked_up_at=now() WHERE ip=$1::inet", malformedIP, `{"latitude":"invalid"}`); err != nil {
		t.Fatal("could not create malformed cache fixture")
	}
	if _, _, found, err := cache.Get(ctx, unlookedIP); err != nil || found {
		t.Fatalf("unlooked IP should be a cache miss: found=%v err=%v", found, err)
	}
	if _, _, _, err := cache.Get(ctx, malformedIP); err == nil || err.Error() != "invalid cached geolocation" {
		t.Fatalf("malformed cached JSON error = %v", err)
	}
	if err := cache.Put(ctx, malformedIP, Location{}, ""); err == nil {
		t.Fatal("empty cache source was accepted")
	}
	if err := cache.Put(ctx, "not-an-ip", Location{Country: "US"}, "fixture"); err == nil {
		t.Fatal("invalid IP was accepted by PostgreSQL cache")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, _, err := cache.Get(cancelled, unlookedIP); err == nil {
		t.Fatal("cancelled cache lookup unexpectedly succeeded")
	}
	t.Log("PASS: unlooked rows miss, malformed JSON and invalid IP fail safely, cancellation propagates")
}

func TestPostgresCacheMergeRefreshesOnlyContributingObservationsIntegration(t *testing.T) {
	pool, ctx := integrationPool(t)
	cache := NewPostgresCache(pool)
	base := uint64(time.Now().UnixNano())
	completeIP, partialIP := integrationFixtureIP(base+2), integrationFixtureIP(base+3)
	old := time.Now().UTC().Add(-30 * 24 * time.Hour).Truncate(time.Microsecond)
	complete := `{"city":"London","country":"GB","latitude":51.5,"longitude":-0.1,"asn":64500,"organisation":"Example","field_sources":{"city":"geolite2","country":"geolite2","coordinates":"geolite2","asn":"geolite2","organisation":"geolite2"}}`
	partial := `{"country":"US","field_sources":{"country":"ipwho"}}`
	for ip, raw := range map[string]string{completeIP: complete, partialIP: partial} {
		if _, err := pool.Exec(ctx, `INSERT INTO ip_addresses(ip,geo,source,looked_up_at) VALUES($1::inet,$2::jsonb,$3,$4)`, ip, raw, map[string]string{completeIP: "geolite2", partialIP: "ipwho"}[ip], old); err != nil {
			t.Fatal("could not insert freshness fixture")
		}
	}
	if _, _, err := cache.Merge(ctx, completeIP, Location{City: "London", Country: "GB", Latitude: 51.5, Longitude: -0.1,
		ASN: 64500, Organisation: "Example"}, "ipwho"); err != nil {
		t.Fatal("no-op lower-ranked merge failed")
	}
	var unchanged time.Time
	if err := pool.QueryRow(ctx, `SELECT looked_up_at FROM ip_addresses WHERE ip=$1::inet`, completeIP).Scan(&unchanged); err != nil || !unchanged.Equal(old) {
		t.Fatalf("no-op merge renewed old complete location: timestamp=%v err=%v", unchanged, err)
	}
	got, _, found, err := cache.Get(ctx, completeIP)
	if err != nil || !found || got.AgeDays < 29 {
		t.Fatalf("no-op merge reset cached age: %+v found=%v err=%v", got, found, err)
	}
	if _, _, err := cache.Merge(ctx, partialIP, Location{ASN: 174, Organisation: "Cogent"}, "ipinfo-lite"); err != nil {
		t.Fatal("partial contribution merge failed")
	}
	var refreshed time.Time
	if err := pool.QueryRow(ctx, `SELECT looked_up_at FROM ip_addresses WHERE ip=$1::inet`, partialIP).Scan(&refreshed); err != nil || !refreshed.After(old.Add(29*24*time.Hour)) {
		t.Fatalf("new ASN observation did not refresh its location timestamp: timestamp=%v err=%v", refreshed, err)
	}
	var source string
	got, source, found, err = cache.Get(ctx, partialIP)
	if err != nil || !found || got.Country != "US" || got.ASN != 174 || got.FieldSources.Country != "ipwho" ||
		got.FieldSources.ASN != "ipinfo-lite" || source != "merged:ipinfo-lite+ipwho" {
		t.Fatalf("partial refresh lost existing values or provenance: %+v source=%q found=%v err=%v", got, source, found, err)
	}
}

type backfillBatch struct {
	calls     int
	err       error
	weak      bool
	addresses [][]string
}

func (b *backfillBatch) LookupBatch(_ context.Context, ips []string, _ budget.Purpose) (map[string]Location, error) {
	b.calls++
	b.addresses = append(b.addresses, append([]string(nil), ips...))
	if b.err != nil {
		return nil, b.err
	}
	locations := make(map[string]Location, len(ips))
	for _, ip := range ips {
		if b.weak {
			locations[ip] = Location{Country: "GB"}
			continue
		}
		locations[ip] = Location{City: "London", Region: "England", Country: "GB", Latitude: 51.5, Longitude: -0.1,
			TimeZone: "Europe/London", AccuracyRadius: 5, ASN: 64500, Organisation: "Example Network"}
	}
	return locations, nil
}

func TestBackfillResumesWithinTheUTCDayIntegration(t *testing.T) {
	pool, ctx := integrationPool(t)
	base := uint64(time.Now().UnixNano())
	first := integrationFixtureIP(base)
	second := integrationFixtureIP(base + 1)
	for _, ip := range []string{first, second} {
		if _, err := pool.Exec(ctx, `INSERT INTO ip_addresses(ip,geo,source,looked_up_at) VALUES($1::inet,'{"country":"GB","quality_score":20}'::jsonb,'ipwho',now())`, ip); err != nil {
			t.Fatal("could not insert weak cache fixture")
		}
	}
	batch := &backfillBatch{}
	job := &Backfill{DB: pool, Cache: NewPostgresCache(pool), Batch: batch, MaxPerRun: 1, BatchSize: 1,
		Now: func() time.Time { return time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC) }}
	for range 2 {
		processed, err := job.RunOnce(ctx)
		if err != nil || processed != 1 {
			t.Fatalf("RunOnce = (%d, %v), want (1, nil)", processed, err)
		}
	}
	if processed, err := job.RunOnce(ctx); err != nil || processed != 0 {
		t.Fatalf("completed cursor RunOnce = (%d, %v), want (0, nil)", processed, err)
	}
	if batch.calls != 2 {
		t.Fatalf("batch calls = %d, want 2", batch.calls)
	}
	var refreshed int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ip_addresses WHERE ip IN ($1::inet,$2::inet) AND geo->>'city'='London'`, first, second).Scan(&refreshed); err != nil || refreshed != 2 {
		t.Fatalf("refreshed rows = %d, err=%v", refreshed, err)
	}
}

func TestBackfillTraversesMissingAndWeakNodeAddressesAcrossDaysIntegration(t *testing.T) {
	pool, ctx := integrationPool(t)
	addresses := []string{"203.0.113.101", "203.0.113.102", "203.0.113.103", "203.0.113.104"}
	if _, err := pool.Exec(ctx, `INSERT INTO ip_addresses(ip,geo,source,looked_up_at) VALUES
($1::inet,'{"country":"GB","quality_score":20}'::jsonb,'ipwho',now()),
($2::inet,'{"country":"GB","quality_score":20}'::jsonb,'ipwho',now()),
($3::inet,'{}'::jsonb,'',NULL),
($4::inet,'{"country":"GB","quality_score":20}'::jsonb,'ipwho',now())`, addresses[0], addresses[1], addresses[2], addresses[3]); err != nil {
		t.Fatal("could not insert weak and missing location fixtures")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES($1,'backfill node',100,50)`, backfillNodeID); err != nil {
		t.Fatal("could not insert node fixture")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO node_ip_addresses(node_id,ip,tests,first_seen,last_seen,speed_tests,down_sum,up_sum)
VALUES($1,$2::inet,1,now(),now(),1,1,1),($1,$3::inet,1,now(),now(),1,1,1)`, backfillNodeID, addresses[2], addresses[3]); err != nil {
		t.Fatal("could not mark node address priority")
	}
	batch := &backfillBatch{err: ErrBudgetExhausted, weak: true}
	now := time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC)
	job := &Backfill{DB: pool, Cache: NewPostgresCache(pool), Batch: batch, MaxPerRun: 1, BatchSize: 1,
		Now: func() time.Time { return now }}
	if processed, err := job.RunOnce(ctx); err != nil || processed != 0 {
		t.Fatalf("initial budget stop = (%d,%v)", processed, err)
	}
	batch.err = nil
	wantOrder := []string{addresses[2], addresses[3], addresses[0], addresses[1]}
	for i, want := range wantOrder {
		now = now.AddDate(0, 0, 1)
		if processed, err := job.RunOnce(ctx); err != nil || processed != 1 {
			t.Fatalf("night %d RunOnce = (%d,%v), want (1,nil)", i+1, processed, err)
		}
		if len(batch.addresses) != i+2 || len(batch.addresses[i+1]) != 1 || batch.addresses[i+1][0] != want {
			t.Fatalf("night %d candidate = %v, want %s", i+1, batch.addresses[i+1], want)
		}
	}
	now = now.AddDate(0, 0, 1)
	if processed, err := job.RunOnce(ctx); err != nil || processed != 0 {
		t.Fatalf("end-of-keyspace pass = (%d,%v)", processed, err)
	}
	now = now.AddDate(0, 0, 1)
	if processed, err := job.RunOnce(ctx); err != nil || processed != 1 || batch.addresses[len(batch.addresses)-1][0] != addresses[2] {
		t.Fatalf("new pass did not revisit first node address: (%d,%v), calls=%v", processed, err, batch.addresses)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ip_addresses WHERE host(ip) IN($1,$2,$3,$4) AND looked_up_at IS NOT NULL`, addresses[0], addresses[1], addresses[2], addresses[3]).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("missing placeholders were not backfilled alongside weak records: %d", n)
	}
}

func TestGeoQuotaMigrationRollbackPreservesAddressHistoryIntegration(t *testing.T) {
	pool, ctx := integrationPool(t)
	address := "203.0.113.199"
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES($1,'rollback fixture',100,50)`, backfillNodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ip_addresses(ip,geo,source,looked_up_at) VALUES($1::inet,'{"city":"London","country":"GB"}','geolite2',now())`, address); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO node_ip_addresses(node_id,ip,tests,first_seen,last_seen,speed_tests,down_sum,up_sum) VALUES($1,$2::inet,1,now(),now(),1,100,50)`, backfillNodeID, address); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO geo_provider_usage(provider,day,requests) VALUES('rollback-fixture',current_date,3)`); err != nil {
		t.Fatal(err)
	}
	if err := centerdb.Migrate(ctx, pool, "down"); err != nil {
		t.Fatal(err)
	}
	if n := countGeoRows(t, pool, ctx, `SELECT count(*) FROM ip_addresses WHERE ip=$1::inet`, address); n != 1 {
		t.Fatalf("rollback removed realistic cached address: %d", n)
	}
	if n := countGeoRows(t, pool, ctx, `SELECT count(*) FROM node_ip_addresses WHERE node_id=$1 AND ip=$2::inet`, backfillNodeID, address); n != 1 {
		t.Fatalf("rollback removed node address history: %d", n)
	}
	if err := centerdb.Migrate(ctx, pool, "up"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO geo_backfill_state(singleton,run_day,cursor) VALUES(true,current_date,$1::inet)`, address); err != nil {
		t.Fatal("migrated cursor schema rejected a realistic node address")
	}
	if n := countGeoRows(t, pool, ctx, `SELECT count(*) FROM geo_backfill_state WHERE cursor_node_priority`); n != 1 {
		t.Fatalf("new cursor priority default missing after rollback/up: %d", n)
	}
}

func integrationFixtureIP(seed uint64) string {
	return fmt.Sprintf("2001:db8::%04x:%04x:%04x:%04x", uint16(seed>>48), uint16(seed>>32), uint16(seed>>16), uint16(seed))
}
