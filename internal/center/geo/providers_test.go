package geo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/geo/budget"
)

func providerTestConfig() ProviderConfig {
	return ProviderConfig{
		Timeout: 300 * time.Millisecond, FailureThreshold: 2, BreakerReset: 20 * time.Millisecond,
		IPAPIEnabled: true, IPAPIURL: "http://ip-api.com/json", IPAPIRequestsPerMinute: 45,
		IPInfoEnabled: true, IPInfoURL: "https://api.ipinfo.io/lite", IPInfoToken: "test-token",
	}
}

func TestIPAPIProviderLookupAndRateHeaders(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/json/81.2.69.160" || r.URL.Query().Get("fields") == "" {
			t.Errorf("unexpected endpoint: %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		w.Header().Set("X-Rl", "0")
		w.Header().Set("X-Ttl", "1")
		_, _ = fmt.Fprint(w, `{"status":"success","query":"81.2.69.160","city":"London","regionName":"England","countryCode":"GB","lat":51.5142,"lon":-0.0931,"timezone":"Europe/London","org":"Example Org","as":"AS64500 Example ASN"}`)
	}))
	defer server.Close()
	cfg := providerTestConfig()
	cfg.IPAPIURL = server.URL + "/json"
	provider, err := NewIPAPIProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := provider.Lookup(context.Background(), "81.2.69.160")
	want := Location{City: "London", Region: "England", Country: "GB", Latitude: 51.5142, Longitude: -0.0931, TimeZone: "Europe/London", ASN: 64500, Organisation: "Example ASN"}
	if err != nil || got != want {
		t.Fatalf("Lookup() = %+v, %v; want %+v", got, err, want)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := provider.Lookup(ctx, "81.2.69.160"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("rate-limited lookup error = %v; want deadline", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("X-Rl/X-Ttl allowed another request: %d requests", requests.Load())
	}
}

func TestIPAPIBatchLookup(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		w.Header().Set("X-Rl", "0")
		w.Header().Set("X-Ttl", "1")
		if req.Method != http.MethodPost || req.URL.Path != "/batch" || req.URL.Query().Get("fields") == "" {
			t.Errorf("unexpected batch endpoint: %s %s?%s", req.Method, req.URL.Path, req.URL.RawQuery)
		}
		body, err := io.ReadAll(req.Body)
		if err != nil || string(body) != `["81.2.69.160","8.8.8.8"]` {
			t.Errorf("batch body = %s, err=%v", body, err)
		}
		_, _ = fmt.Fprint(w, `[{"status":"success","query":"81.2.69.160","city":"London","countryCode":"GB","lat":51.5,"lon":-0.1,"as":"AS64500 Example ASN"},{"status":"fail","query":"8.8.8.8"}]`)
	}))
	defer server.Close()
	cfg := providerTestConfig()
	cfg.IPAPIURL = server.URL + "/json"
	provider, err := NewIPAPIProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	locations, err := provider.LookupBatch(context.Background(), []string{"81.2.69.160", "8.8.8.8"}, budget.Background)
	want := Location{City: "London", Country: "GB", Latitude: 51.5, Longitude: -0.1, ASN: 64500, Organisation: "Example ASN"}
	if err != nil || len(locations) != 1 || locations["81.2.69.160"] != want || requests.Load() != 1 {
		t.Fatalf("LookupBatch() = %+v, %v; requests=%d", locations, err, requests.Load())
	}
	provider.batchLimiter.mu.Lock()
	provider.batchLimiter.next = time.Now().Add(-time.Second)
	provider.batchLimiter.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := provider.LookupBatch(ctx, []string{"8.8.8.8"}, budget.Background); !errors.Is(err, context.DeadlineExceeded) || requests.Load() != 1 {
		t.Fatalf("batch cooldown was not honored: err=%v requests=%d", err, requests.Load())
	}
	if _, err := provider.LookupBatch(context.Background(), make([]string, 101), budget.Background); err == nil {
		t.Fatal("accepted more than 100 addresses")
	}
}

func TestIPInfoLiteProviderUsesBearerAndMapsFreeFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/lite/8.8.8.8" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("unexpected IPinfo request: %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = fmt.Fprint(w, `{"ip":"8.8.8.8","country_code":"US","asn":"AS15169","as_name":"Google LLC"}`)
	}))
	defer server.Close()
	cfg := providerTestConfig()
	cfg.IPInfoURL = server.URL + "/lite"
	provider, err := NewIPInfoLiteProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := provider.Lookup(context.Background(), "8.8.8.8")
	want := Location{Country: "US", ASN: 15169, Organisation: "Google LLC"}
	if err != nil || got != want {
		t.Fatalf("Lookup() = %+v, %v; want %+v", got, err, want)
	}
}

func TestProviderTimeoutIncludesRateLimitWait(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = fmt.Fprint(w, `{"ip":"8.8.8.8","country_code":"US"}`)
	}))
	defer server.Close()
	cfg := providerTestConfig()
	cfg.IPInfoURL = server.URL + "/lite"
	cfg.IPInfoRequestsPerMinute = 1
	cfg.Timeout = 25 * time.Millisecond
	provider, err := NewIPInfoLiteProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Lookup(context.Background(), "8.8.8.8"); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := provider.Lookup(context.Background(), "8.8.8.8"); err == nil || !strings.Contains(err.Error(), "rate limit wait timed out") {
		t.Fatalf("rate wait timeout error = %v", err)
	}
	if time.Since(started) > 250*time.Millisecond || requests.Load() != 1 {
		t.Fatalf("rate wait was not bounded: elapsed=%s requests=%d", time.Since(started), requests.Load())
	}
}

func TestProviderCircuitBreakerOpensAndRecovers(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) < 3 {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = fmt.Fprint(w, `{"ip":"8.8.8.8","country_code":"US"}`)
	}))
	defer server.Close()
	cfg := providerTestConfig()
	cfg.IPInfoURL = server.URL + "/lite"
	cfg.IPInfoRequestsPerMinute = 0
	cfg.FailureThreshold = 2
	cfg.BreakerReset = 25 * time.Millisecond
	provider, err := NewIPInfoLiteProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := provider.Lookup(context.Background(), "8.8.8.8"); err == nil {
			t.Fatal("expected provider error")
		}
	}
	if _, err := provider.Lookup(context.Background(), "8.8.8.8"); err == nil || !strings.Contains(err.Error(), "circuit breaker is open") {
		t.Fatalf("expected open circuit, got %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	got, err := provider.Lookup(context.Background(), "8.8.8.8")
	if err != nil || got.Country != "US" || requests.Load() != 3 {
		t.Fatalf("half-open recovery = %+v, %v; requests=%d", got, err, requests.Load())
	}
}

func TestProviderTimeoutAndInvalidResponses(t *testing.T) {
	t.Run("429 without headers backs off", func(t *testing.T) {
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer server.Close()
		cfg := providerTestConfig()
		cfg.IPInfoURL = server.URL + "/lite"
		cfg.IPInfoRequestsPerMinute = 0
		provider, err := NewIPInfoLiteProvider(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := provider.Lookup(context.Background(), "8.8.8.8"); err == nil {
			t.Fatal("expected HTTP 429 error")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		if _, err := provider.Lookup(ctx, "8.8.8.8"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("429 backoff error = %v; want deadline", err)
		}
		if requests.Load() != 1 {
			t.Fatalf("429 retry was sent before backoff elapsed: %d requests", requests.Load())
		}
	})

	t.Run("timeout counts as failure", func(t *testing.T) {
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if requests.Add(1) == 1 {
				<-r.Context().Done()
				return
			}
			_, _ = fmt.Fprint(w, `{"ip":"8.8.8.8","country_code":"US"}`)
		}))
		defer server.Close()
		cfg := providerTestConfig()
		cfg.IPInfoURL = server.URL + "/lite"
		cfg.IPInfoRequestsPerMinute = 0
		cfg.Timeout = 15 * time.Millisecond
		cfg.FailureThreshold = 1
		cfg.BreakerReset = 20 * time.Millisecond
		provider, err := NewIPInfoLiteProvider(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := provider.Lookup(context.Background(), "8.8.8.8"); err == nil {
			t.Fatal("expected timeout")
		}
		if _, err := provider.Lookup(context.Background(), "8.8.8.8"); err == nil || !strings.Contains(err.Error(), "circuit breaker is open") {
			t.Fatalf("timeout did not open breaker: %v", err)
		}
	})

	t.Run("mismatched response IP", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `{"ip":"1.1.1.1","country_code":"US"}`)
		}))
		defer server.Close()
		cfg := providerTestConfig()
		cfg.IPInfoURL = server.URL + "/lite"
		cfg.IPInfoRequestsPerMinute = 0
		provider, err := NewIPInfoLiteProvider(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := provider.Lookup(context.Background(), "8.8.8.8"); err == nil || !strings.Contains(err.Error(), "unexpected IPinfo response") {
			t.Fatalf("expected mismatched response error, got %v", err)
		}
	})

	t.Run("IPinfo malformed and empty responses", func(t *testing.T) {
		for _, tc := range []struct {
			name, body string
			status     int
			wantMiss   bool
		}{
			{name: "HTTP status", body: `{}`, status: http.StatusBadGateway},
			{name: "malformed JSON", body: `{`, status: http.StatusOK},
			{name: "empty location", body: `{"ip":"8.8.8.8"}`, status: http.StatusOK, wantMiss: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(tc.status)
					_, _ = fmt.Fprint(w, tc.body)
				}))
				defer server.Close()
				cfg := providerTestConfig()
				cfg.IPInfoURL = server.URL + "/lite"
				cfg.IPInfoRequestsPerMinute = 0
				provider, err := NewIPInfoLiteProvider(cfg)
				if err != nil {
					t.Fatal(err)
				}
				_, err = provider.Lookup(context.Background(), "8.8.8.8")
				if tc.wantMiss {
					if !errors.Is(err, ErrNotFound) {
						t.Fatalf("expected ErrNotFound, got %v", err)
					}
				} else if err == nil {
					t.Fatal("invalid IPinfo response was accepted")
				}
			})
		}
	})

	t.Run("direct invalid IP rejected", func(t *testing.T) {
		cfg := providerTestConfig()
		provider, err := NewIPInfoLiteProvider(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := provider.Lookup(context.Background(), "invalid"); !errors.Is(err, ErrInvalidIP) {
			t.Fatalf("expected ErrInvalidIP, got %v", err)
		}
	})
}

func TestProviderConfigurationAndLimits(t *testing.T) {
	for _, key := range []string{
		"SPEEDTEST_GEO_PROVIDER_TIMEOUT", "SPEEDTEST_GEO_PROVIDER_FAILURE_THRESHOLD", "SPEEDTEST_GEO_PROVIDER_BREAKER_RESET",
		"SPEEDTEST_GEO_IPAPI_ENABLED", "SPEEDTEST_GEO_IPAPI_URL", "SPEEDTEST_GEO_IPAPI_REQUESTS_PER_MINUTE",
		"SPEEDTEST_GEO_IPINFO_ENABLED", "SPEEDTEST_GEO_IPINFO_URL", "SPEEDTEST_GEO_IPINFO_REQUESTS_PER_MINUTE", "SPEEDTEST_GEO_IPINFO_TOKEN",
	} {
		t.Setenv(key, "")
	}
	for _, key := range []string{"SPEEDTEST_GEO_IPWHO_ENABLED", "SPEEDTEST_GEO_FREEIPAPI_ENABLED", "SPEEDTEST_GEO_GEOJS_ENABLED", "SPEEDTEST_GEO_RIPESTAT_ENABLED"} {
		t.Setenv(key, "false")
	}
	cfg, err := ProviderConfigFromEnv()
	if err != nil || cfg.Timeout != 5*time.Second || cfg.IPAPIRequestsPerMinute != 45 || cfg.IPAPIEnabled || cfg.IPInfoEnabled || cfg.IPInfoURL != "https://api.ipinfo.io/lite" {
		t.Fatalf("unexpected defaults: %+v, %v", cfg, err)
	}
	t.Setenv("SPEEDTEST_GEO_IPINFO_ENABLED", "true")
	if _, err := ProviderConfigFromEnv(); err == nil || strings.Contains(err.Error(), "test-token") {
		t.Fatal("enabled IPinfo without token was accepted or error exposed a value")
	}
	t.Setenv("SPEEDTEST_GEO_IPINFO_TOKEN", "test-token")
	t.Setenv("SPEEDTEST_GEO_IPAPI_ENABLED", "true")
	cfg, err = ProviderConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	providers, err := NewFallbackProviders(cfg)
	if err != nil || len(providers) != 2 || providers[0].Source() != "ip-api" || providers[1].Source() != "ipinfo-lite" {
		t.Fatalf("provider order = %v, %v", providers, err)
	}
	badEndpoint := cfg
	badEndpoint.IPAPIURL = "ftp://invalid.example/json"
	if _, err := NewFallbackProviders(badEndpoint); err == nil {
		t.Fatal("invalid enabled provider endpoint was accepted")
	}
	cfg.IPAPIRequestsPerMinute = 46
	if _, err := NewFallbackProviders(cfg); err == nil {
		t.Fatal("ip-api rate above its published limit was accepted")
	}
	cfg.IPAPIEnabled = false
	cfg.IPInfoEnabled = false
	providers, err = NewFallbackProviders(cfg)
	if err != nil || len(providers) != 0 {
		t.Fatalf("disabled providers were not omitted: %v, %v", providers, err)
	}
	t.Setenv("SPEEDTEST_GEO_IPAPI_URL", "https://geo.example.test/json")
	cfg, err = ProviderConfigFromEnv()
	if err != nil || cfg.IPAPIURL != "https://geo.example.test/json" {
		t.Fatalf("provider URL override was not loaded: %q, %v", cfg.IPAPIURL, err)
	}
}

func TestProviderConfigurationRejectsInvalidValues(t *testing.T) {
	keys := []string{
		"SPEEDTEST_GEO_PROVIDER_TIMEOUT", "SPEEDTEST_GEO_PROVIDER_FAILURE_THRESHOLD", "SPEEDTEST_GEO_PROVIDER_BREAKER_RESET",
		"SPEEDTEST_GEO_IPAPI_ENABLED", "SPEEDTEST_GEO_IPAPI_URL", "SPEEDTEST_GEO_IPAPI_REQUESTS_PER_MINUTE",
		"SPEEDTEST_GEO_IPINFO_ENABLED", "SPEEDTEST_GEO_IPINFO_URL", "SPEEDTEST_GEO_IPINFO_REQUESTS_PER_MINUTE", "SPEEDTEST_GEO_IPINFO_TOKEN",
	}
	for _, key := range keys {
		t.Setenv(key, "")
	}
	for _, tc := range []struct{ key, value string }{
		{"SPEEDTEST_GEO_PROVIDER_TIMEOUT", "invalid"},
		{"SPEEDTEST_GEO_PROVIDER_TIMEOUT", "0s"},
		{"SPEEDTEST_GEO_PROVIDER_FAILURE_THRESHOLD", "invalid"},
		{"SPEEDTEST_GEO_PROVIDER_FAILURE_THRESHOLD", "0"},
		{"SPEEDTEST_GEO_PROVIDER_BREAKER_RESET", "invalid"},
		{"SPEEDTEST_GEO_PROVIDER_BREAKER_RESET", "0s"},
		{"SPEEDTEST_GEO_IPAPI_ENABLED", "sometimes"},
		{"SPEEDTEST_GEO_IPAPI_REQUESTS_PER_MINUTE", "invalid"},
		{"SPEEDTEST_GEO_IPAPI_REQUESTS_PER_MINUTE", "0"},
		{"SPEEDTEST_GEO_IPAPI_REQUESTS_PER_MINUTE", "-1"},
		{"SPEEDTEST_GEO_IPAPI_REQUESTS_PER_MINUTE", "46"},
		{"SPEEDTEST_GEO_IPINFO_ENABLED", "sometimes"},
		{"SPEEDTEST_GEO_IPINFO_REQUESTS_PER_MINUTE", "invalid"},
		{"SPEEDTEST_GEO_IPINFO_REQUESTS_PER_MINUTE", "-1"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			for _, key := range keys {
				t.Setenv(key, "")
			}
			if strings.HasPrefix(tc.key, "SPEEDTEST_GEO_IPAPI_REQUESTS_PER_MINUTE") {
				t.Setenv("SPEEDTEST_GEO_IPAPI_ENABLED", "true")
			}
			t.Setenv(tc.key, tc.value)
			if _, err := ProviderConfigFromEnv(); err == nil {
				t.Fatalf("invalid %s value was accepted", tc.key)
			}
		})
	}
}

func TestRateLimiterPacesRequests(t *testing.T) {
	limiter := newRateLimiter(6000)
	if err := limiter.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := limiter.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) < 7*time.Millisecond {
		t.Fatal("configured request spacing was not applied")
	}
}

func TestProviderResponseValidation(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		wantMiss   bool
	}{
		{name: "HTTP status", body: `{}`, status: http.StatusBadGateway},
		{name: "malformed JSON", body: `{`, status: http.StatusOK},
		{name: "provider not found", body: `{"status":"fail","query":"8.8.8.8"}`, status: http.StatusOK, wantMiss: true},
		{name: "unexpected status", body: `{"status":"maybe","query":"8.8.8.8"}`, status: http.StatusOK},
		{name: "mismatched address", body: `{"status":"success","query":"1.1.1.1","countryCode":"US"}`, status: http.StatusOK},
		{name: "invalid coordinates", body: `{"status":"success","query":"8.8.8.8","lat":91,"countryCode":"US"}`, status: http.StatusOK},
		{name: "empty location", body: `{"status":"success","query":"8.8.8.8"}`, status: http.StatusOK, wantMiss: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			cfg := providerTestConfig()
			cfg.IPAPIURL = server.URL + "/json"
			provider, err := NewIPAPIProvider(cfg)
			if err != nil {
				t.Fatal(err)
			}
			_, err = provider.Lookup(context.Background(), "8.8.8.8")
			if tc.wantMiss {
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("expected ErrNotFound, got %v", err)
				}
			} else if err == nil {
				t.Fatal("invalid provider response was accepted")
			}
		})
	}
}

func TestProviderConstructorsAndHelpers(t *testing.T) {
	cfg := providerTestConfig()
	cfg.IPAPIURL = "http://user@example.test/json"
	if _, err := NewIPAPIProvider(cfg); err == nil {
		t.Fatal("endpoint credentials were accepted")
	}
	cfg = providerTestConfig()
	cfg.IPAPIRequestsPerMinute = 0
	if _, err := NewIPAPIProvider(cfg); err == nil {
		t.Fatal("unlimited ip-api requests were accepted")
	}
	cfg = providerTestConfig()
	cfg.IPInfoToken = ""
	if _, err := NewIPInfoLiteProvider(cfg); err == nil {
		t.Fatal("IPinfo provider without a token was accepted")
	}
	cfg = providerTestConfig()
	cfg.IPInfoURL = "https://user@example.test/lite"
	if _, err := NewIPInfoLiteProvider(cfg); err == nil {
		t.Fatal("IPinfo endpoint user information was accepted")
	}
	cfg = providerTestConfig()
	cfg.IPInfoURL = "ftp://example.test/lite"
	if _, err := NewIPInfoLiteProvider(cfg); err == nil {
		t.Fatal("unsupported provider scheme was accepted")
	}
	cfg.IPInfoURL = "http://ipinfo.example/lite"
	if _, err := NewIPInfoLiteProvider(cfg); err == nil {
		t.Fatal("remote IPinfo endpoint without TLS was accepted")
	}
	provider, err := NewIPAPIProvider(providerTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Lookup(context.Background(), "invalid"); !errors.Is(err, ErrInvalidIP) {
		t.Fatalf("ip-api accepted invalid IP: %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.Lookup(cancelled, "8.8.8.8"); !errors.Is(err, context.Canceled) {
		t.Fatalf("provider ignored cancelled context: %v", err)
	}
	invalidCore := providerTestConfig()
	invalidCore.Timeout = 0
	if _, err := NewIPInfoLiteProvider(invalidCore); err == nil {
		t.Fatal("invalid provider timeout was accepted")
	}
	if _, err := endpointForIP("://invalid", "8.8.8.8", false); err == nil {
		t.Fatal("invalid endpoint was accepted")
	}
	if endpoint, err := endpointForIP("https://geo.example.test/json", "2001:db8::1", false); err != nil || !strings.Contains(endpoint, "2001:db8::1") {
		t.Fatalf("IPv6 endpoint construction = %q, %v", endpoint, err)
	}
	if matchesResponseIP("", "8.8.8.8") || matchesResponseIP("1.1.1.1", "8.8.8.8") {
		t.Fatal("missing or mismatched response IP was accepted")
	}
	if _, err := validatedIP("invalid"); !errors.Is(err, ErrInvalidIP) {
		t.Fatalf("invalid IP validation = %v", err)
	}
	asn, organisation := parseAS("", "Fallback Org")
	if asn != 0 || organisation != "Fallback Org" {
		t.Fatalf("empty AS mapping = %d, %q", asn, organisation)
	}
	asn, organisation = parseAS("Example Network", "Fallback Org")
	if asn != 0 || organisation != "Example Network" {
		t.Fatalf("unprefixed AS mapping = %d, %q", asn, organisation)
	}
	if err := validateLocation(Location{Latitude: 91}); err == nil {
		t.Fatal("invalid coordinates were accepted")
	}
	limiter := newRateLimiter(0)
	limiter.ObserveResponse(http.Header{"X-Rl": []string{"0"}}, http.StatusOK)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := limiter.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("missing X-Ttl fallback did not block: %v", err)
	}
}

func TestCircuitBreakerTransitionsAndAbandon(t *testing.T) {
	breaker := newCircuitBreaker(2, time.Second)
	breaker.Complete(false, true)
	breaker.Complete(false, false)
	breaker.Complete(false, true)
	breaker.Complete(false, true)
	if allowed, _ := breaker.Allow(); allowed {
		t.Fatal("breaker remained closed after repeated failures")
	}
	breaker.mu.Lock()
	breaker.openUntil = time.Now().Add(-time.Millisecond)
	breaker.mu.Unlock()
	allowed, probe := breaker.Allow()
	if !allowed || !probe {
		t.Fatal("breaker did not permit its half-open probe")
	}
	breaker.Complete(false, false) // Stale in-flight success must not close the probe.
	breaker.Abandon(false)
	breaker.Abandon(probe)
	allowed, probe = breaker.Allow()
	if !allowed || !probe {
		t.Fatal("abandoned probe did not release the half-open breaker")
	}
	breaker.Complete(probe, true)
	if allowed, _ := breaker.Allow(); allowed {
		t.Fatal("failed half-open probe did not reopen the breaker")
	}
	breaker.mu.Lock()
	breaker.openUntil = time.Now().Add(-time.Millisecond)
	breaker.mu.Unlock()
	allowed, probe = breaker.Allow()
	if !allowed || !probe {
		t.Fatal("breaker did not allow recovery probe")
	}
	breaker.Complete(probe, false)
	if allowed, probe := breaker.Allow(); !allowed || probe {
		t.Fatal("successful recovery probe did not close breaker")
	}
}

func TestProviderBodyLimitAndRedirectIsolation(t *testing.T) {
	t.Run("oversized response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, strings.Repeat("x", maxProviderResponse+1))
		}))
		defer server.Close()
		cfg := providerTestConfig()
		cfg.IPInfoURL = server.URL + "/lite"
		cfg.IPInfoRequestsPerMinute = 0
		provider, err := NewIPInfoLiteProvider(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := provider.Lookup(context.Background(), "8.8.8.8"); err == nil || !strings.Contains(err.Error(), "cannot read provider response") {
			t.Fatalf("oversized body error = %v", err)
		}
	})

	t.Run("cross-host redirect is rejected", func(t *testing.T) {
		var targetRequests atomic.Int32
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			targetRequests.Add(1)
			_, _ = fmt.Fprint(w, `{"ip":"8.8.8.8","country_code":"US"}`)
		}))
		defer target.Close()
		redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/leak", http.StatusFound)
		}))
		defer redirect.Close()
		cfg := providerTestConfig()
		cfg.IPInfoURL = redirect.URL + "/lite"
		cfg.IPInfoRequestsPerMinute = 0
		provider, err := NewIPInfoLiteProvider(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := provider.Lookup(context.Background(), "8.8.8.8"); err == nil {
			t.Fatal("cross-host redirect was followed")
		}
		if targetRequests.Load() != 0 {
			t.Fatal("provider token was forwarded to another host")
		}
	})
}
