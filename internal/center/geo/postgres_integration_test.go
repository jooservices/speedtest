//go:build integration

package geo

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	centerdb "github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/shared/config"
)

func integrationPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	values, err := godotenv.Read(filepath.Join("../../../", ".env"))
	if err != nil {
		t.Fatal("a configured worktree .env is required")
	}
	for key, value := range values {
		t.Setenv(key, value)
	}
	cfg, err := config.LoadCenter()
	if err != nil {
		t.Fatal("invalid test database configuration")
	}
	if cfg.Database.Name != "speedtest_wt34" && cfg.Database.Name != "speedtest_test" {
		t.Fatal("refusing database outside the ticket worktree/test allowlist")
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
	if err != nil || !found || source != "ip-api" || got != want {
		t.Fatalf("cache round trip = %+v, source=%q, found=%v, err=%v", got, source, found, err)
	}
	if err := cache.Put(ctx, ip, Location{Country: "US"}, "ipinfo-lite"); err != nil {
		t.Fatal(err)
	}
	got, source, found, err = cache.Get(ctx, ip)
	if err != nil || !found || source != "ip-api" || got != want {
		t.Fatalf("cached address was looked up or overwritten again: %+v, source=%q, err=%v", got, source, err)
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

func integrationFixtureIP(seed uint64) string {
	return fmt.Sprintf("2001:db8::%04x:%04x:%04x:%04x", uint16(seed>>48), uint16(seed>>32), uint16(seed>>16), uint16(seed))
}
