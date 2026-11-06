package geo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func extraConfig(url string) ProviderConfig {
	return ProviderConfig{
		Timeout: 300 * time.Millisecond, FailureThreshold: 2, BreakerReset: 20 * time.Millisecond,
		IPWhoEnabled: true, IPWhoURL: url, IPWhoRequestsPerMinute: 600,
		IPAPICoEnabled: true, IPAPICoURL: url, IPAPICoRequestsPerMinute: 600,
		IPGeoEnabled: true, IPGeoURL: url + "/ipgeo", IPGeoRequestsPerMinute: 600, IPGeoKey: "secret-key",
	}
}

func TestExtraProvidersMapResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/8.8.8.8":
			_, _ = fmt.Fprint(w, `{"ip":"8.8.8.8","success":true,"country_code":"US","region":"California","city":"Mountain View","latitude":37.4,"longitude":-122.0,"connection":{"asn":15169,"org":"","isp":"Google LLC"},"timezone":{"id":"America/Los_Angeles"}}`)
		case "/8.8.8.8/json/":
			_, _ = fmt.Fprint(w, `{"ip":"8.8.8.8","city":"Mountain View","region":"California","country_code":"US","latitude":37.4,"longitude":-122.0,"timezone":"America/Los_Angeles","asn":"AS15169","org":"GOOGLE"}`)
		case "/ipgeo":
			if r.URL.Query().Get("apiKey") != "secret-key" || r.URL.Query().Get("ip") != "8.8.8.8" {
				t.Errorf("unexpected ipgeolocation query %q", r.URL.RawQuery)
			}
			_, _ = fmt.Fprint(w, `{"ip":"8.8.8.8","country_code2":"US","state_prov":"California","city":"Mountain View","latitude":"37.4","longitude":"-122.0","organization":"","isp":"Google LLC","asn":"AS15169 Google","time_zone":{"name":"America/Los_Angeles"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	providers, err := NewFallbackProviders(extraConfig(server.URL))
	if err != nil || len(providers) != 3 {
		t.Fatalf("providers = %d, %v", len(providers), err)
	}
	wantSources := []string{"ipwho", "ipapi-co", "ipgeolocation"}
	for i, p := range providers {
		if p.Source() != wantSources[i] {
			t.Fatalf("source %d = %q", i, p.Source())
		}
		got, err := p.Lookup(context.Background(), "8.8.8.8")
		if err != nil || got.Country != "US" || got.City != "Mountain View" || got.ASN != 15169 || got.TimeZone != "America/Los_Angeles" || got.Latitude != 37.4 {
			t.Fatalf("%s Lookup() = %+v, %v", p.Source(), got, err)
		}
	}
}

func TestExtraProviderErrors(t *testing.T) {
	var body string
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	defer server.Close()
	providers, err := NewFallbackProviders(extraConfig(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		status int
		body   string
		want   error
		text   string
		only   string
	}{
		{"fail", 200, `{"success":false}`, ErrNotFound, "", "ipwho"},
		{"fail-error", 200, `{"error":true}`, ErrNotFound, "", "ipapi-co"},
		{"fail-noip", 200, `{"message":"x"}`, ErrNotFound, "", "ipgeolocation"},
		{"404", 404, ``, ErrNotFound, "", ""},
		{"500", 500, ``, nil, "HTTP status 500", ""},
		{"junk", 200, `not json`, nil, "invalid", ""},
		{"empty", 200, `{"ip":"8.8.8.8","success":true}`, ErrNotFound, "", ""},
	}
	for _, c := range cases {
		for _, p := range providers {
			body, status = c.body, c.status
			// A fresh provider each time keeps the circuit breaker closed.
			fresh, err := NewFallbackProviders(extraConfig(server.URL))
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range fresh {
				if f.Source() != p.Source() || (c.only != "" && c.only != f.Source()) {
					continue
				}
				_, err := f.Lookup(context.Background(), "8.8.8.8")
				if c.want != nil && !errors.Is(err, c.want) {
					t.Errorf("%s/%s: err = %v; want %v", c.name, p.Source(), err, c.want)
				}
				if c.want == nil && c.text != "" && (err == nil || !strings.Contains(err.Error(), c.text)) {
					t.Errorf("%s/%s: err = %v; want text %q", c.name, p.Source(), err, c.text)
				}
				if err != nil && strings.Contains(err.Error(), "secret-key") {
					t.Errorf("%s/%s: error leaks the key", c.name, p.Source())
				}
			}
		}
	}
}

func TestExtraProviderRejectsAnotherIPAndBadCoordinates(t *testing.T) {
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, body) }))
	defer server.Close()
	for _, b := range []string{
		`{"ip":"1.1.1.1","success":true,"country_code":"US","country_code2":"US"}`,
		`{"ip":"8.8.8.8","success":true,"error":false,"country_code":"US","country_code2":"US","latitude":999}`,
	} {
		body = b
		providers, err := NewFallbackProviders(extraConfig(server.URL))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range providers {
			if _, err := p.Lookup(context.Background(), "8.8.8.8"); err == nil || errors.Is(err, ErrNotFound) {
				t.Errorf("%s accepted %s: %v", p.Source(), b, err)
			}
		}
	}
}

func TestExtraProviderConstructionAndConfig(t *testing.T) {
	cfg := extraConfig("https://example.com")
	cfg.IPGeoKey = ""
	if _, err := NewFallbackProviders(cfg); err == nil {
		t.Fatal("expected an error when ipgeolocation.io has no key")
	}
	cfg = extraConfig("https://example.com")
	cfg.IPGeoURL = "http://api.example.com/ipgeo"
	if _, err := NewFallbackProviders(cfg); err == nil {
		t.Fatal("expected an error for a key provider without HTTPS")
	}
	cfg = extraConfig("not a url")
	if _, err := NewFallbackProviders(cfg); err == nil {
		t.Fatal("expected an invalid endpoint error")
	}
	if _, err := (&jsonProvider{}).Lookup(context.Background(), "nonsense"); err == nil {
		t.Fatal("expected an invalid IP error")
	}
	if _, err := ipgeoEndpoint("::bad", "8.8.8.8", "k"); err == nil {
		t.Fatal("expected an invalid endpoint error")
	}
	if _, err := pathEndpoint("::bad", "8.8.8.8", ""); err == nil {
		t.Fatal("expected an invalid endpoint error")
	}
	// The endpoint builders reject a base URL without a host.
	if _, err := ipgeoEndpoint("/relative", "8.8.8.8", "k"); err == nil {
		t.Fatal("expected a missing host error")
	}
}

func TestExtraProviderEnvironment(t *testing.T) {
	t.Setenv("SPEEDTEST_GEO_IPWHO_ENABLED", "true")
	t.Setenv("SPEEDTEST_GEO_IPAPICO_ENABLED", "true")
	t.Setenv("SPEEDTEST_GEO_IPAPICO_REQUESTS_PER_MINUTE", "7")
	t.Setenv("SPEEDTEST_GEO_IPGEOLOCATION_ENABLED", "true")
	t.Setenv("SPEEDTEST_GEO_IPGEOLOCATION_KEY", "k")
	t.Setenv("SPEEDTEST_GEO_CONSENSUS", "true")
	cfg, err := ProviderConfigFromEnv()
	if err != nil || !cfg.IPWhoEnabled || !cfg.IPAPICoEnabled || cfg.IPAPICoRequestsPerMinute != 7 || !cfg.IPGeoEnabled || cfg.IPGeoKey != "k" || !cfg.Consensus {
		t.Fatalf("ProviderConfigFromEnv() = %+v, %v", cfg, err)
	}
	t.Setenv("SPEEDTEST_GEO_IPGEOLOCATION_KEY", "")
	if _, err := ProviderConfigFromEnv(); err == nil {
		t.Fatal("expected an error when the key is missing")
	}
	t.Setenv("SPEEDTEST_GEO_IPGEOLOCATION_KEY", "k")
	t.Setenv("SPEEDTEST_GEO_IPWHO_REQUESTS_PER_MINUTE", "-1")
	if _, err := ProviderConfigFromEnv(); err == nil {
		t.Fatal("expected an error for a negative rate")
	}
	t.Setenv("SPEEDTEST_GEO_IPWHO_REQUESTS_PER_MINUTE", "x")
	if _, err := ProviderConfigFromEnv(); err == nil {
		t.Fatal("expected an error for a malformed rate")
	}
	t.Setenv("SPEEDTEST_GEO_IPWHO_REQUESTS_PER_MINUTE", "30")
	t.Setenv("SPEEDTEST_GEO_IPWHO_ENABLED", "maybe")
	if _, err := ProviderConfigFromEnv(); err == nil {
		t.Fatal("expected an error for a malformed boolean")
	}
	t.Setenv("SPEEDTEST_GEO_IPWHO_ENABLED", "true")
	t.Setenv("SPEEDTEST_GEO_CONSENSUS", "maybe")
	if _, err := ProviderConfigFromEnv(); err == nil {
		t.Fatal("expected an error for a malformed consensus flag")
	}
}

type fakeProvider struct {
	name string
	loc  Location
	err  error
}

func (f fakeProvider) Source() string                                   { return f.name }
func (f fakeProvider) Lookup(context.Context, string) (Location, error) { return f.loc, f.err }

type memoryCache struct {
	source string
	loc    Location
}

func (m *memoryCache) Get(context.Context, string) (Location, string, bool, error) {
	return Location{}, "", false, nil
}
func (m *memoryCache) Put(_ context.Context, _ string, l Location, s string) error {
	m.loc, m.source = l, s
	return nil
}

func TestConsensusPicksTheMajorityAndFillsGaps(t *testing.T) {
	cache := &memoryCache{}
	var logged strings.Builder
	logger := slog.New(slog.NewTextHandler(&logged, nil))
	r := NewResolver(cache, nil,
		fakeProvider{"a", Location{Country: "DE", City: "Berlin"}, nil},
		fakeProvider{"b", Location{Country: "vn", City: "Hanoi", ASN: 7552}, nil},
		fakeProvider{"c", Location{Country: "VN", Organisation: "Viettel", TimeZone: "Asia/Ho_Chi_Minh"}, nil},
		fakeProvider{"d", Location{}, ErrNotFound},
		fakeProvider{"e", Location{}, errors.New("boom")},
	).WithConsensus(true, logger)
	got, err := r.Lookup(context.Background(), "1.2.3.4")
	if err != nil || got.Country != "vn" || got.City != "Hanoi" || got.ASN != 7552 || got.Organisation != "Viettel" || got.TimeZone != "Asia/Ho_Chi_Minh" {
		t.Fatalf("Lookup() = %+v, %v", got, err)
	}
	if cache.source != "consensus:b+c" {
		t.Fatalf("source = %q", cache.source)
	}
	if !strings.Contains(logged.String(), "disagree") || strings.Contains(logged.String(), "1.2.3.4") {
		t.Fatalf("log = %q", logged.String())
	}
}

func TestConsensusSingleAnswerFailuresAndCancel(t *testing.T) {
	cache := &memoryCache{}
	r := NewResolver(cache, nil,
		fakeProvider{"a", Location{}, ErrNotFound},
		fakeProvider{"b", Location{Country: "US"}, nil},
	).WithConsensus(true, nil)
	if _, err := r.Lookup(context.Background(), "1.2.3.4"); err != nil || cache.source != "b" {
		t.Fatalf("single answer: %v source=%q", err, cache.source)
	}
	none := NewResolver(nil, nil, fakeProvider{"a", Location{}, ErrNotFound}, fakeProvider{"b", Location{}, ErrNotFound}).WithConsensus(true, nil)
	if _, err := none.Lookup(context.Background(), "1.2.3.4"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("none = %v", err)
	}
	failing := NewResolver(nil, nil, fakeProvider{"a", Location{}, errors.New("x")}, fakeProvider{"b", Location{}, errors.New("y")}).WithConsensus(true, nil)
	if _, err := failing.Lookup(context.Background(), "1.2.3.4"); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "x") {
		t.Fatalf("failing = %v", err)
	}
	cancelled := NewResolver(nil, nil, fakeProvider{"a", Location{}, context.Canceled}, fakeProvider{"b", Location{Country: "US"}, nil}).WithConsensus(true, nil)
	if _, err := cancelled.Lookup(context.Background(), "1.2.3.4"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled = %v", err)
	}
	// Context cancelled between providers.
	ctx, cancel := context.WithCancel(context.Background())
	between := NewResolver(nil, nil, cancelAfter{cancel}, fakeProvider{"b", Location{Country: "US"}, nil}).WithConsensus(true, nil)
	if _, err := between.Lookup(ctx, "1.2.3.4"); !errors.Is(err, context.Canceled) {
		t.Fatalf("between = %v", err)
	}
}

type cancelAfter struct{ cancel context.CancelFunc }

func (c cancelAfter) Source() string { return "cancel" }
func (c cancelAfter) Lookup(context.Context, string) (Location, error) {
	c.cancel()
	return Location{Country: "US"}, nil
}

func TestPickConsensusTiesAndFill(t *testing.T) {
	loc, source, dis := pickConsensus([]sourced{{"a", Location{Country: "DE"}}, {"b", Location{Country: "FR"}}})
	if loc.Country != "DE" || source != "a" || dis != "DE,FR" {
		t.Fatalf("tie = %+v %q %q", loc, source, dis)
	}
	_, _, dis = pickConsensus([]sourced{{"a", Location{Country: ""}}, {"b", Location{Country: "FR"}}, {"c", Location{Country: "FR"}}})
	if dis != "none,FR" {
		t.Fatalf("empty country label = %q", dis)
	}
	got := fill(Location{}, Location{City: "c", Region: "r", Country: "x", Latitude: 1, Longitude: 2, TimeZone: "z", AccuracyRadius: 5, ASN: 9, Organisation: "o"})
	if got.City != "c" || got.Region != "r" || got.Country != "x" || got.Latitude != 1 || got.TimeZone != "z" || got.AccuracyRadius != 5 || got.ASN != 9 || got.Organisation != "o" {
		t.Fatalf("fill() = %+v", got)
	}
	kept := fill(Location{City: "keep", Latitude: 3, ASN: 1}, Location{City: "no", Latitude: 4, Longitude: 4, ASN: 2})
	if kept.City != "keep" || kept.Latitude != 3 || kept.ASN != 1 {
		t.Fatalf("fill() overwrote values: %+v", kept)
	}
}
