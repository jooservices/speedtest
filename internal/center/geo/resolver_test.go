package geo

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/geo/budget"
)

type cachedResult struct {
	location Location
	source   string
}

type testCache struct {
	mu      sync.Mutex
	entries map[string]cachedResult
	gets    int
	puts    int
	getErr  error
	putErr  error
}

func (c *testCache) Get(_ context.Context, ip string) (Location, string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets++
	if c.getErr != nil {
		return Location{}, "", false, c.getErr
	}
	entry, found := c.entries[ip]
	return entry.location, entry.source, found, nil
}

func (c *testCache) Put(_ context.Context, ip string, location Location, source string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.puts++
	if c.putErr != nil {
		return c.putErr
	}
	if c.entries == nil {
		c.entries = make(map[string]cachedResult)
	}
	if _, exists := c.entries[ip]; !exists {
		c.entries[ip] = cachedResult{location: location, source: source}
	}
	return nil
}

type testGeo struct {
	location Location
	err      error
	calls    atomic.Int32
	wait     time.Duration
}

func (g *testGeo) Lookup(ctx context.Context, _ string) (Location, error) {
	g.calls.Add(1)
	if g.wait > 0 {
		timer := time.NewTimer(g.wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return Location{}, ctx.Err()
		case <-timer.C:
		}
	}
	return g.location, g.err
}

type testFallback struct {
	name string
	geo  *testGeo
}

type mergeTestCache struct {
	*testCache
	mergeErr error
}

func (c *mergeTestCache) Merge(_ context.Context, _ string, location Location, source string) (Location, string, error) {
	if c.mergeErr != nil {
		return Location{}, "", c.mergeErr
	}
	return location, source, nil
}

func (f testFallback) Source() string { return f.name }
func (f testFallback) Lookup(ctx context.Context, ip string) (Location, error) {
	return f.geo.Lookup(ctx, ip)
}

type cancelFallback struct {
	name   string
	cancel context.CancelFunc
	calls  atomic.Int32
}

func (f *cancelFallback) Source() string { return f.name }
func (f *cancelFallback) Lookup(context.Context, string) (Location, error) {
	f.calls.Add(1)
	f.cancel()
	return Location{}, ErrNotFound
}

func TestResolverCacheHitSkipsAllLookups(t *testing.T) {
	want := Location{City: "London", Region: "England", Country: "GB", Latitude: 51.5, Longitude: -0.1,
		TimeZone: "Europe/London", AccuracyRadius: 20, ASN: 64500, Organisation: "Example"}
	cache := &testCache{entries: map[string]cachedResult{"81.2.69.160": {location: want, source: "ip-api"}}}
	offline := &testGeo{err: ErrNotFound}
	fallback := testFallback{name: "ip-api", geo: &testGeo{err: errors.New("must not call provider")}}
	resolver := NewResolver(cache, offline, fallback)

	got, err := resolver.Lookup(context.Background(), "81.2.69.160")
	if err != nil || got.City != want.City || got.Country != want.Country || got.ASN != want.ASN || got.QualityScore == 0 {
		t.Fatalf("Lookup() = %+v, %v; want %+v", got, err, want)
	}
	if offline.calls.Load() != 0 || fallback.geo.calls.Load() != 0 {
		t.Fatal("cache hit called an upstream geolocation source")
	}
}

func TestResolverOfflineThenFallbackAndPersistsSource(t *testing.T) {
	t.Run("offline result", func(t *testing.T) {
		cache := &testCache{}
		want := Location{City: "London", Region: "England", Country: "GB", Latitude: 51.5, Longitude: -0.1,
			TimeZone: "Europe/London", AccuracyRadius: 20, ASN: 64500, Organisation: "Example"}
		offline := &testGeo{location: want}
		provider := testFallback{name: "ip-api", geo: &testGeo{err: errors.New("unexpected provider call")}}
		got, err := NewResolver(cache, offline, provider).Lookup(context.Background(), "81.2.69.160")
		if err != nil || got.City != want.City || got.Country != want.Country || got.QualityScore == 0 || cache.entries["81.2.69.160"].source != GeoLiteSource {
			t.Fatalf("got %+v, %v; cache=%+v", got, err, cache.entries)
		}
		if provider.geo.calls.Load() != 0 || cache.puts != 1 {
			t.Fatal("offline hit did not stop and persist the chain")
		}
	})

	t.Run("fallback chain", func(t *testing.T) {
		cache := &testCache{}
		offline := &testGeo{err: ErrNotFound}
		first := testFallback{name: "ip-api", geo: &testGeo{err: ErrNotFound}}
		want := Location{Country: "US", ASN: 15169, Organisation: "Google LLC"}
		second := testFallback{name: "ipinfo-lite", geo: &testGeo{location: want}}
		got, err := NewResolver(cache, offline, first, second).WithHighValue(true).Lookup(context.Background(), "8.8.8.8")
		if err != nil || got.Country != want.Country || got.ASN != want.ASN || got.Organisation != want.Organisation || cache.entries["8.8.8.8"].source != "ipinfo-lite" {
			t.Fatalf("got %+v, %v; cache=%+v", got, err, cache.entries)
		}
		if first.geo.calls.Load() != 1 || second.geo.calls.Load() != 1 {
			t.Fatal("fallbacks were not tried in configured order")
		}
	})
}

func TestResolverLowValueFallbackSkipsUnusableProvidersUntilSuccess(t *testing.T) {
	for _, firstErr := range []error{ErrBudgetExhausted, ErrNotFound, errors.New("provider unavailable")} {
		first := testFallback{name: "first", geo: &testGeo{err: firstErr}}
		second := testFallback{name: "second", geo: &testGeo{location: Location{Country: "US"}}}
		third := testFallback{name: "third", geo: &testGeo{err: errors.New("must stop after a result")}}
		got, err := NewResolver(nil, nil, first, second, third).Lookup(context.Background(), "8.8.8.8")
		if err != nil || got.Country != "US" || first.geo.calls.Load() != 1 || second.geo.calls.Load() != 1 || third.geo.calls.Load() != 0 {
			t.Fatalf("first=%v lookup = %+v, %v; calls=(%d,%d,%d)", firstErr, got, err,
				first.geo.calls.Load(), second.geo.calls.Load(), third.geo.calls.Load())
		}
	}
}

type agedMergeCache struct {
	location Location
	source   string
	found    bool
}

func (c *agedMergeCache) Get(context.Context, string) (Location, string, bool, error) {
	return c.location, c.source, c.found, nil
}

func (c *agedMergeCache) Put(_ context.Context, _ string, location Location, source string) error {
	c.location, c.source, c.found = location, source, true
	return nil
}

func (c *agedMergeCache) Merge(_ context.Context, _ string, incoming Location, source string) (Location, string, error) {
	merged, mergedSource := MergeLocations(c.location, c.source, incoming, source)
	if !sameLocationData(c.location, merged) || c.source != mergedSource {
		merged.AgeDays = 0
	} else {
		merged.AgeDays = c.location.AgeDays
	}
	merged.QualityScore = QualityScore(merged, mergedSource, merged.AgeDays)
	c.location, c.source, c.found = merged, mergedSource, true
	return merged, mergedSource, nil
}

func TestAgedNoOpOfflineMergeKeepsAgeAndStillRefreshes(t *testing.T) {
	old := Location{City: "London", Country: "GB", Latitude: 51.5, Longitude: -0.1, ASN: 64500,
		Organisation: "Example Network", AgeDays: 180}
	old.FieldSources = sourcesFor(old, "geolite2")
	for _, path := range []string{"interactive", "backfill"} {
		t.Run(path, func(t *testing.T) {
			cache := &agedMergeCache{location: old, source: "geolite2", found: true}
			offline := &testGeo{location: old}
			provider := testFallback{name: "freeipapi", geo: &testGeo{err: ErrNotFound}}
			resolver := NewResolver(cache, offline, provider)
			if path == "backfill" {
				job := &Backfill{Cache: cache, Resolver: resolver}
				if _, err := job.refresh(context.Background(), []backfillCandidate{{address: "8.8.8.8"}}); err != nil {
					t.Fatal(err)
				}
			} else if _, err := resolver.Lookup(context.Background(), "8.8.8.8"); err != nil {
				t.Fatal(err)
			}
			if provider.geo.calls.Load() != 1 || cache.location.AgeDays != 180 || cache.location.QualityScore != QualityScore(old, "ipwho", 180) {
				t.Fatalf("no-op offline refresh changed freshness or skipped provider: calls=%d location=%+v", provider.geo.calls.Load(), cache.location)
			}
		})
	}
}

func TestConsensusRetainsPartialOfflineAnswerWhenProvidersHaveNoUsableAnswer(t *testing.T) {
	partial := Location{Country: "GB", ASN: 64500, Organisation: "Example Network"}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "miss", err: ErrNotFound},
		{name: "failure", err: errors.New("provider unavailable")},
		{name: "exhausted", err: ErrBudgetExhausted},
	} {
		for _, withCache := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/without-cache", true: "/with-cache"}[withCache], func(t *testing.T) {
				var cache Cache
				if withCache {
					cache = &testCache{}
				}
				first := testFallback{name: "ipwho", geo: &testGeo{err: tc.err}}
				second := testFallback{name: "freeipapi", geo: &testGeo{err: tc.err}}
				got, err := NewResolver(cache, &testGeo{location: partial}, first, second).
					WithHighValue(true).WithConsensus(true, nil).Lookup(context.Background(), "8.8.8.8")
				if got.Country != partial.Country || got.ASN != partial.ASN || got.Organisation != partial.Organisation || err != nil {
					t.Fatalf("partial offline result = (%+v,%v)", got, err)
				}
				if withCache {
					stored := cache.(*testCache)
					if stored.entries["8.8.8.8"].location.Country != partial.Country || stored.puts == 0 {
						t.Fatalf("offline answer was not persisted: %+v", stored.entries)
					}
				}
			})
		}
	}
}

func TestResolverErrorAndInvalidIPPaths(t *testing.T) {
	t.Run("cancelled context", func(t *testing.T) {
		cache := &testCache{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := NewResolver(cache, nil).Lookup(ctx, "8.8.8.8"); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context cancellation, got %v", err)
		}
		if cache.gets != 0 {
			t.Fatal("cancelled lookup reached the cache")
		}
	})

	t.Run("cache read fails closed", func(t *testing.T) {
		cache := &testCache{getErr: errors.New("database down")}
		offline := &testGeo{location: Location{Country: "GB"}}
		if _, err := NewResolver(cache, offline).Lookup(context.Background(), "81.2.69.160"); err == nil || err.Error() != "geolocation cache read failed" {
			t.Fatalf("expected sanitized cache error, got %v", err)
		}
		if offline.calls.Load() != 0 {
			t.Fatal("lookup bypassed failed cache")
		}
	})

	t.Run("cache write failure", func(t *testing.T) {
		cache := &testCache{putErr: errors.New("database down")}
		offline := &testGeo{location: Location{Country: "GB"}}
		if _, err := NewResolver(cache, offline).Lookup(context.Background(), "81.2.69.160"); err == nil || err.Error() != "geolocation cache write failed" {
			t.Fatalf("expected sanitized cache error, got %v", err)
		}
	})

	t.Run("invalid IP", func(t *testing.T) {
		cache := &testCache{}
		offline := &testGeo{err: ErrNotFound}
		if _, err := NewResolver(cache, offline).Lookup(context.Background(), "not-an-ip"); !errors.Is(err, ErrInvalidIP) {
			t.Fatalf("expected ErrInvalidIP, got %v", err)
		}
		if cache.gets != 0 || offline.calls.Load() != 0 {
			t.Fatal("invalid IP reached cache or GeoLite2")
		}
	})

	t.Run("provider errors continue and are returned", func(t *testing.T) {
		offline := &testGeo{err: ErrNotFound}
		first := testFallback{name: "ip-api", geo: &testGeo{err: errors.New("temporary provider error")}}
		second := testFallback{name: "ipinfo-lite", geo: &testGeo{err: ErrNotFound}}
		_, err := NewResolver(nil, offline, first, second).WithHighValue(true).Lookup(context.Background(), "8.8.4.4")
		if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "temporary provider error") {
			t.Fatalf("expected not-found with provider cause, got %v", err)
		}
		if second.geo.calls.Load() != 1 {
			t.Fatal("provider error stopped the fallback chain")
		}
	})

	t.Run("offline errors stop fallback", func(t *testing.T) {
		offline := &testGeo{err: errors.New("offline reader failed")}
		provider := testFallback{name: "ip-api", geo: &testGeo{location: Location{Country: "US"}}}
		_, err := NewResolver(nil, offline, provider).Lookup(context.Background(), "8.8.8.8")
		if err == nil || err.Error() != "offline reader failed" || provider.geo.calls.Load() != 0 {
			t.Fatalf("offline error did not stop fallback: %v", err)
		}
	})

	t.Run("no answer", func(t *testing.T) {
		if _, err := NewResolver(nil, nil).Lookup(context.Background(), "8.8.8.8"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
		var zero Resolver
		if _, err := zero.Lookup(context.Background(), "8.8.8.8"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("zero-value resolver = %v", err)
		}
	})
}

func TestResolverUsesPurposeAwareFallbackAndPreservesStorageErrors(t *testing.T) {
	location := Location{Country: "GB", City: "London", Latitude: 51.5, Longitude: -0.1,
		ASN: 64500, Organisation: "Example Network", AccuracyRadius: 5}
	t.Run("passes background purpose", func(t *testing.T) {
		provider := &purposeFallback{name: "ipwho", location: location}
		got, err := NewResolver(nil, nil, provider).WithPurpose(budget.Background).Lookup(context.Background(), "8.8.8.8")
		if err != nil || got.Country != location.Country || provider.lastPurpose != budget.Background || provider.calls != 1 {
			t.Fatalf("purpose-aware lookup = (%+v,%v), purpose=%v calls=%d", got, err, provider.lastPurpose, provider.calls)
		}
	})
	t.Run("offline merge storage failure", func(t *testing.T) {
		cache := &mergeTestCache{testCache: &testCache{}, mergeErr: errors.New("database unavailable")}
		_, err := NewResolver(cache, &testGeo{location: location}).Lookup(context.Background(), "8.8.8.8")
		if err == nil || err.Error() != "geolocation cache write failed" {
			t.Fatalf("offline merge storage error = %v", err)
		}
	})
	t.Run("consensus seed storage failure", func(t *testing.T) {
		cache := &testCache{putErr: errors.New("database unavailable"), entries: map[string]cachedResult{
			"8.8.8.8": {location: Location{Country: "GB"}, source: "ipwho"},
		}}
		first := testFallback{name: "ipwho", geo: &testGeo{location: location}}
		second := testFallback{name: "freeipapi", geo: &testGeo{location: location}}
		_, err := NewResolver(cache, nil, first, second).WithHighValue(true).WithConsensus(true, nil).Lookup(context.Background(), "8.8.8.8")
		if err == nil || err.Error() != "geolocation cache write failed" || first.geo.calls.Load() != 0 || second.geo.calls.Load() != 0 {
			t.Fatalf("consensus seed storage error = %v provider calls=(%d,%d)", err, first.geo.calls.Load(), second.geo.calls.Load())
		}
	})
}

func TestResolverWaiterCancellationDoesNotCancelSharedLookup(t *testing.T) {
	providerGeo := &testGeo{location: Location{Country: "GB"}, wait: 50 * time.Millisecond}
	resolver := NewResolver(nil, nil, testFallback{name: "ipwho", geo: providerGeo})
	first := make(chan error, 1)
	go func() {
		_, err := resolver.Lookup(context.Background(), "8.8.8.8")
		first <- err
	}()
	deadline := time.Now().Add(time.Second)
	for providerGeo.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithCancel(context.Background())
	waiter := make(chan error, 1)
	go func() {
		_, err := resolver.Lookup(ctx, "8.8.8.8")
		waiter <- err
	}()
	time.Sleep(5 * time.Millisecond)
	cancel()
	if err := <-waiter; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter error = %v", err)
	}
	if err := <-first; err != nil || providerGeo.calls.Load() != 1 {
		t.Fatalf("shared lookup = %v, provider calls=%d", err, providerGeo.calls.Load())
	}
}

func TestResolverStopsProviderChainWhenContextIsCanceledBetweenProviders(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	first := &cancelFallback{name: "ipwho", cancel: cancel}
	second := &testGeo{location: Location{Country: "GB"}}
	resolver := NewResolver(nil, nil, first, testFallback{name: "freeipapi", geo: second}).WithHighValue(true)
	if _, err := resolver.Lookup(ctx, "8.8.8.8"); !errors.Is(err, context.Canceled) {
		t.Fatalf("provider chain cancellation = %v", err)
	}
	if first.calls.Load() != 1 || second.calls.Load() != 0 {
		t.Fatalf("provider calls after cancellation = (%d,%d)", first.calls.Load(), second.calls.Load())
	}
}

func TestCheapProviderAgreementIgnoresCountrylessAnswers(t *testing.T) {
	answers := []sourced{
		{source: "ripestat", location: Location{ASN: 64500}},
		{source: "ipwho", location: Location{Country: "gb"}},
		{source: "freeipapi", location: Location{Country: "GB"}},
	}
	if !cheapProvidersAgree(answers) {
		t.Fatal("countryless ASN answer prevented agreeing country providers from bypassing ipgeolocation")
	}
	if cheapProvidersAgree([]sourced{{location: Location{ASN: 64500}}, {location: Location{}}}) {
		t.Fatal("countryless answers were counted as a country agreement")
	}
}

func TestResolverCoalescesConcurrentLookups(t *testing.T) {
	cache := &testCache{}
	offline := &testGeo{err: ErrNotFound}
	providerGeo := &testGeo{location: Location{Country: "US"}, wait: 25 * time.Millisecond}
	resolver := NewResolver(cache, offline, testFallback{name: "ip-api", geo: providerGeo})
	const requests = 24
	var wg sync.WaitGroup
	errs := make(chan error, requests)
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			location, err := resolver.Lookup(context.Background(), "8.8.8.8")
			if err == nil && location.Country != "US" {
				err = errors.New("unexpected location")
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if providerGeo.calls.Load() != 1 || cache.puts != 1 {
		t.Fatalf("same-IP requests made %d upstream calls and %d cache writes", providerGeo.calls.Load(), cache.puts)
	}
}
