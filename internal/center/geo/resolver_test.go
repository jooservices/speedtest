package geo

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

func (f testFallback) Source() string { return f.name }
func (f testFallback) Lookup(ctx context.Context, ip string) (Location, error) {
	return f.geo.Lookup(ctx, ip)
}

func TestResolverCacheHitSkipsAllLookups(t *testing.T) {
	want := Location{City: "London", Country: "GB", ASN: 64500}
	cache := &testCache{entries: map[string]cachedResult{"81.2.69.160": {location: want, source: "ip-api"}}}
	offline := &testGeo{err: ErrNotFound}
	fallback := testFallback{name: "ip-api", geo: &testGeo{err: errors.New("must not call provider")}}
	resolver := NewResolver(cache, offline, fallback)

	got, err := resolver.Lookup(context.Background(), "81.2.69.160")
	if err != nil || got != want {
		t.Fatalf("Lookup() = %+v, %v; want %+v", got, err, want)
	}
	if offline.calls.Load() != 0 || fallback.geo.calls.Load() != 0 {
		t.Fatal("cache hit called an upstream geolocation source")
	}
}

func TestResolverOfflineThenFallbackAndPersistsSource(t *testing.T) {
	t.Run("offline result", func(t *testing.T) {
		cache := &testCache{}
		want := Location{City: "London", Country: "GB"}
		offline := &testGeo{location: want}
		provider := testFallback{name: "ip-api", geo: &testGeo{err: errors.New("unexpected provider call")}}
		got, err := NewResolver(cache, offline, provider).Lookup(context.Background(), "81.2.69.160")
		if err != nil || got != want || cache.entries["81.2.69.160"].source != GeoLiteSource {
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
		got, err := NewResolver(cache, offline, first, second).Lookup(context.Background(), "8.8.8.8")
		if err != nil || got != want || cache.entries["8.8.8.8"].source != "ipinfo-lite" {
			t.Fatalf("got %+v, %v; cache=%+v", got, err, cache.entries)
		}
		if first.geo.calls.Load() != 1 || second.geo.calls.Load() != 1 {
			t.Fatal("fallbacks were not tried in configured order")
		}
	})
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
		_, err := NewResolver(nil, offline, first, second).Lookup(context.Background(), "8.8.4.4")
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
