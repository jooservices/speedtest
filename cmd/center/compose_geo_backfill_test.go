package main

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jooservices/speedtest/internal/center/geo"
	"github.com/jooservices/speedtest/internal/center/ipwatch"
	"github.com/jooservices/speedtest/internal/center/telemetry"
)

type backfillWiringProvider struct {
	name     string
	location geo.Location
	calls    int
}

type backfillWiringCache struct{ locations map[string]geo.Location }

func (c *backfillWiringCache) Get(_ context.Context, ip string) (geo.Location, string, bool, error) {
	location, ok := c.locations[ip]
	return location, "fixture", ok, nil
}

func (c *backfillWiringCache) Put(_ context.Context, ip string, location geo.Location, _ string) error {
	if c.locations == nil {
		c.locations = make(map[string]geo.Location)
	}
	c.locations[ip] = location
	return nil
}

func (c *backfillWiringCache) Merge(ctx context.Context, ip string, location geo.Location, source string) (geo.Location, string, error) {
	if err := c.Put(ctx, ip, location, source); err != nil {
		return geo.Location{}, "", err
	}
	return location, source, nil
}

func (p *backfillWiringProvider) Source() string { return p.name }

func (p *backfillWiringProvider) Lookup(context.Context, string) (geo.Location, error) {
	p.calls++
	return p.location, nil
}

type backfillWiringCandidate struct {
	address string
	node    bool
}

type backfillWiringDB struct{ candidates []backfillWiringCandidate }

func (d *backfillWiringDB) Query(_ context.Context, _ string, args ...any) (pgx.Rows, error) {
	cursor, _ := args[1].(string)
	cursorNode, _ := args[0].(bool)
	limit := args[2].(int)
	rows := &backfillWiringRows{}
	for _, candidate := range d.candidates {
		if cursor != "" && (candidate.node && !cursorNode || candidate.node == cursorNode && candidate.address <= cursor) {
			continue
		}
		rows.values = append(rows.values, candidate)
	}
	if len(rows.values) > limit {
		rows.values = rows.values[:limit]
	}
	return rows, nil
}

func (*backfillWiringDB) QueryRow(context.Context, string, ...any) pgx.Row {
	return backfillWiringRow{err: pgx.ErrNoRows}
}

func (*backfillWiringDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

type backfillWiringRow struct{ err error }

func (r backfillWiringRow) Scan(...any) error { return r.err }

type backfillWiringRows struct {
	pgx.Rows
	values []backfillWiringCandidate
	index  int
}

func (r *backfillWiringRows) Next() bool { return r.index < len(r.values) }
func (r *backfillWiringRows) Scan(dest ...any) error {
	*dest[0].(*string) = r.values[r.index].address
	*dest[1].(*bool) = r.values[r.index].node
	r.index++
	return nil
}
func (*backfillWiringRows) Close()                                       {}
func (*backfillWiringRows) Err() error                                   { return nil }
func (*backfillWiringRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (*backfillWiringRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (*backfillWiringRows) Values() ([]any, error)                       { return nil, nil }
func (*backfillWiringRows) RawValues() [][]byte                          { return nil }
func (*backfillWiringRows) Conn() *pgx.Conn                              { return nil }

func setBackfillWiringConfig(t *testing.T) {
	t.Helper()
	for key, value := range map[string]string{
		"SPEEDTEST_GEO_BACKFILL_MAX_PER_NIGHT":    "100",
		"SPEEDTEST_GEO_BACKFILL_BATCH_SIZE":       "100",
		"SPEEDTEST_GEO_BACKFILL_PAUSE":            "0s",
		"SPEEDTEST_GEO_BACKFILL_HOUR_UTC":         "2",
		"SPEEDTEST_GEO_IPAPI_ENABLED":             "false",
		"SPEEDTEST_GEO_IPAPI_URL":                 "http://ip-api.com/json",
		"SPEEDTEST_GEO_IPAPI_REQUESTS_PER_MINUTE": "45",
	} {
		t.Setenv(key, value)
	}
}

func TestBuildGeoBackfillUsesNodeConsensusAndSingleLookupProvider(t *testing.T) {
	setBackfillWiringConfig(t)
	location := geo.Location{City: "London", Country: "GB", Latitude: 51.5, Longitude: -0.1,
		ASN: 64500, Organisation: "Example Network", AccuracyRadius: 5}
	first := &backfillWiringProvider{name: "ipwho", location: location}
	second := &backfillWiringProvider{name: "freeipapi", location: location}
	batchOnly := &backfillWiringProvider{name: "ip-api", location: location}
	watcher := &ipwatch.Watcher{Offline: nil, Fallbacks: []geo.Fallback{first, second, batchOnly}, Consensus: true}
	operational := telemetry.New("test")
	backfill, err := buildGeoBackfillWithCache(nil, &backfillWiringCache{}, watcher, operational, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if backfill.Resolver == nil || backfill.NodeResolver == nil || backfill.Batch != nil {
		t.Fatalf("backfill resolver wiring = %+v", backfill)
	}
	backfill.DB = &backfillWiringDB{candidates: []backfillWiringCandidate{{address: "203.0.113.2", node: true}, {address: "203.0.113.1"}}}
	if count, err := backfill.RunOnce(context.Background()); err != nil || count != 2 {
		t.Fatalf("wired backfill RunOnce = (%d,%v)", count, err)
	}
	if first.calls != 2 || second.calls != 1 || batchOnly.calls != 0 {
		t.Fatalf("wired provider calls = (%d,%d,%d), want both node providers, first lookup provider, and no single IPAPI calls", first.calls, second.calls, batchOnly.calls)
	}
	backfill.ObserveRun(true)
}

func TestBuildGeoBackfillUsesPostgresCache(t *testing.T) {
	setBackfillWiringConfig(t)
	backfill, err := buildGeoBackfill(nil, &ipwatch.Watcher{}, telemetry.New("test"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || backfill.Cache == nil || backfill.Resolver == nil || backfill.NodeResolver == nil {
		t.Fatalf("Postgres backfill wiring = (%+v,%v)", backfill, err)
	}
}

func TestBuildGeoBackfillRejectsInvalidConfiguration(t *testing.T) {
	setBackfillWiringConfig(t)
	t.Setenv("SPEEDTEST_GEO_BACKFILL_MAX_PER_NIGHT", "invalid")
	if _, err := buildGeoBackfillWithCache(nil, &backfillWiringCache{}, &ipwatch.Watcher{}, telemetry.New("test"), slog.Default()); err == nil {
		t.Fatal("invalid backfill configuration accepted")
	}
}

func TestBuildGeoBackfillRejectsInvalidBatchProvider(t *testing.T) {
	setBackfillWiringConfig(t)
	providers, err := geo.ProviderConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	providers.IPAPIEnabled = true
	providers.IPAPIURL = ":invalid"
	watcher := &ipwatch.Watcher{Providers: providers}
	if _, err = buildGeoBackfillWithCache(nil, &backfillWiringCache{}, watcher, telemetry.New("test"), slog.Default()); err == nil {
		t.Fatal("invalid batch provider accepted")
	}
}

func TestBuildGeoBackfillWiresBatchProviderWhenEnabled(t *testing.T) {
	setBackfillWiringConfig(t)
	providers, err := geo.ProviderConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	providers.IPAPIEnabled = true
	watcher := &ipwatch.Watcher{Providers: providers}
	backfill, err := buildGeoBackfillWithCache(nil, &backfillWiringCache{}, watcher, telemetry.New("test"), slog.Default())
	if err != nil || backfill.Batch == nil {
		t.Fatalf("batch provider wiring = (%+v,%v)", backfill, err)
	}
}
