//go:build integration

package ipapi

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	centerdb "github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/geo"
	"github.com/jooservices/speedtest/internal/shared/config"
)

func ipapiIntegrationPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	values, err := godotenv.Read(filepath.Join("../../../", ".env"))
	if err != nil {
		t.Fatal("a configured worktree .env is required")
	}
	for key, value := range values {
		t.Setenv(key, value)
	}
	cfg, err := config.LoadCenter()
	if err != nil || centerdb.CheckTestDatabase(cfg.Database.Name) != nil {
		t.Fatal("invalid test database configuration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	pool, err := centerdb.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal("could not open the worktree test database")
	}
	t.Cleanup(pool.Close)
	var name, marker string
	if err := pool.QueryRow(ctx, "SELECT current_database(),shobj_description(oid,'pg_database') FROM pg_database WHERE datname=current_database()").Scan(&name, &marker); err != nil || name != "speedtest_wt88" || marker != "workspaces-project:speedtest" {
		t.Fatal("test database identity/ownership verification failed")
	}
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	return pool, ctx
}

type ipapiFixedResolver struct {
	location geo.Location
	err      error
}

func (r ipapiFixedResolver) Lookup(context.Context, string) (geo.Location, error) {
	return r.location, r.err
}

func TestIPViewsAndQualityMetadataIntegration(t *testing.T) {
	pool, ctx := ipapiIntegrationPool(t)
	const node = "11111111-1111-4111-8111-111111111111"
	const history = "22222222-2222-4222-8222-222222222222"
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES($1,'geo fixture',100,50)`, node); err != nil {
		t.Fatal("could not seed node fixture")
	}
	geoJSON := `{"city":"London","region":"England","country":"GB","latitude":51.5,"longitude":-0.1,"asn":64500,"organisation":"Example Network","quality_score":90,"field_sources":{"city":"ip-api","country":"geolite2"}}`
	for _, ip := range []string{"203.0.113.1", "203.0.113.2"} {
		if _, err := pool.Exec(ctx, `INSERT INTO ip_addresses(ip,geo,source,looked_up_at) VALUES($1::inet,$2::jsonb,'merged:geolite2+ip-api',now())`, ip, geoJSON); err != nil {
			t.Fatal("could not seed geolocation fixture")
		}
	}
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `INSERT INTO node_ip_addresses(node_id,ip,tests,first_seen,last_seen,speed_tests,down_sum,up_sum) VALUES
($1,'203.0.113.1',2,$2,$2,2,200,100),($1,'203.0.113.2',1,$3,$3,1,150,75)`, node, now.Add(-time.Hour), now); err != nil {
		t.Fatal("could not seed node address fixtures")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO node_ip_history(id,node_id,ts,from_ip,to_ip) VALUES($1,$2,$3,'203.0.113.1','203.0.113.2')`, history, node, now); err != nil {
		t.Fatal("could not seed IP history fixture")
	}
	store := &Store{DB: pool}
	entries, count, err := store.History(ctx, node, 1, 25)
	if err != nil || count != 1 || entries[0]["fromGeo"].(map[string]any)["qualityScore"] != 90 {
		t.Fatalf("History = count %d entries=%+v err=%v", count, entries, err)
	}
	addresses, total, err := store.Addresses(ctx, node, 1, 25)
	if err != nil || total != 2 || addresses[0]["geo"].(map[string]any)["fieldSources"].(map[string]string)["city"] != "ip-api" {
		t.Fatalf("Addresses = total %d entries=%+v err=%v", total, addresses, err)
	}
	locations, err := store.Map(ctx, node)
	if err != nil || len(locations) != 1 || locations[0]["geo"].(map[string]any)["qualityScore"] != 90 {
		t.Fatalf("Map = %+v, %v", locations, err)
	}
	store.Resolver = ipapiFixedResolver{location: geo.Location{City: "London", Country: "GB", Latitude: 51.5, Longitude: -0.1}}
	lookup, err := store.Lookup(ctx, "203.0.113.2")
	if err != nil || lookup["qualityScore"] == nil || lookup["fieldSources"] == nil {
		t.Fatalf("Lookup = %+v, %v", lookup, err)
	}
	if _, err := store.Lookup(ctx, "host.example"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid lookup error = %v", err)
	}
	store.Resolver = ipapiFixedResolver{err: geo.ErrNotFound}
	if result, err := store.Lookup(ctx, "198.51.100.3"); err != nil || result != nil {
		t.Fatalf("unknown address = %+v, %v", result, err)
	}
	if providers, err := (&Store{}).Providers(ctx); err != nil || len(providers) != 0 {
		t.Fatalf("unconfigured providers = %+v, %v", providers, err)
	}
}
