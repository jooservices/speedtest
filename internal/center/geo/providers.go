package geo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jooservices/speedtest/internal/center/geo/budget"
)

const maxProviderResponse = 1 << 20

// ProviderConfig controls the free fallback services. IPInfoToken is a secret;
// never log this configuration value.
type ProviderConfig struct {
	Timeout                 time.Duration
	FailureThreshold        int
	BreakerReset            time.Duration
	IPAPIEnabled            bool
	IPAPIURL                string
	IPAPIRequestsPerMinute  int
	IPAPIRequestsPerDay     int
	IPInfoEnabled           bool
	IPInfoURL               string
	IPInfoToken             string
	IPInfoRequestsPerMinute int
	IPInfoRequestsPerDay    int

	// Optional extra providers, all off by default (JOOST-75).
	IPWhoEnabled               bool
	IPWhoURL                   string
	IPWhoRequestsPerMinute     int
	IPWhoRequestsPerDay        int
	IPAPICoEnabled             bool
	IPAPICoURL                 string
	IPAPICoRequestsPerMinute   int
	IPAPICoRequestsPerDay      int
	IPGeoEnabled               bool
	IPGeoURL                   string
	IPGeoKey                   string // secret; never log
	IPGeoRequestsPerMinute     int
	IPGeoRequestsPerDay        int
	FreeIPAPIEnabled           bool
	FreeIPAPIURL               string
	FreeIPAPIRequestsPerMinute int
	FreeIPAPIRequestsPerDay    int
	GeoJSEnabled               bool
	GeoJSURL                   string
	GeoJSRequestsPerMinute     int
	GeoJSRequestsPerDay        int
	RIPEStatEnabled            bool
	RIPEStatURL                string
	RIPEStatRequestsPerMinute  int
	RIPEStatRequestsPerDay     int
	// Consensus asks every enabled provider and keeps the answer most of them agree on.
	Consensus bool
	Budget    *budget.Broker
	Observe   func(provider, outcome string)
}

// ProviderConfigFromEnv reads fallback settings. The free ip-api endpoint is
// opt-in; IPinfo Lite also requires an access token and is opt-in.
func ProviderConfigFromEnv() (ProviderConfig, error) {
	cfg := defaultProviderConfig()
	if err := primaryProviderEnv(&cfg); err != nil {
		return ProviderConfig{}, err
	}
	if err := extraProviderEnv(&cfg); err != nil {
		return ProviderConfig{}, err
	}
	if err := validateProviderConfig(cfg); err != nil {
		return ProviderConfig{}, err
	}
	return cfg, nil
}

func defaultProviderConfig() ProviderConfig {
	return ProviderConfig{
		Timeout:                    5 * time.Second,
		FailureThreshold:           5,
		BreakerReset:               time.Minute,
		IPAPIEnabled:               false,
		IPAPIURL:                   "http://ip-api.com/json",
		IPAPIRequestsPerMinute:     45,
		IPAPIRequestsPerDay:        0,
		IPInfoEnabled:              false,
		IPInfoURL:                  "https://api.ipinfo.io/lite",
		IPInfoRequestsPerMinute:    0,
		IPInfoRequestsPerDay:       0,
		IPInfoToken:                os.Getenv("SPEEDTEST_GEO_IPINFO_TOKEN"),
		IPWhoEnabled:               true,
		IPWhoURL:                   "https://ipwho.is",
		IPWhoRequestsPerMinute:     30,
		IPWhoRequestsPerDay:        1000,
		FreeIPAPIEnabled:           true,
		FreeIPAPIURL:               "https://free.freeipapi.com/api/v1/json",
		FreeIPAPIRequestsPerMinute: 60,
		GeoJSURL:                   "https://get.geojs.io/v1/ip/geo.json",
		GeoJSRequestsPerMinute:     30,
		RIPEStatEnabled:            true,
		RIPEStatURL:                "https://stat.ripe.net/data/network-info/data.json",
		RIPEStatRequestsPerMinute:  30,
		RIPEStatRequestsPerDay:     1000,
		Consensus:                  true,
		IPAPICoURL:                 "https://ipapi.co",
		IPAPICoRequestsPerMinute:   10,
		IPAPICoRequestsPerDay:      1000,
		IPGeoURL:                   "https://api.ipgeolocation.io/ipgeo",
		IPGeoRequestsPerMinute:     30,
		IPGeoRequestsPerDay:        1000,
		IPGeoKey:                   os.Getenv("SPEEDTEST_GEO_IPGEOLOCATION_KEY"),
	}
}

func primaryProviderEnv(cfg *ProviderConfig) error {
	var err error
	if cfg.Timeout, err = envDuration("SPEEDTEST_GEO_PROVIDER_TIMEOUT", cfg.Timeout); err != nil {
		return err
	}
	if cfg.FailureThreshold, err = envInt("SPEEDTEST_GEO_PROVIDER_FAILURE_THRESHOLD", cfg.FailureThreshold); err != nil {
		return err
	}
	if cfg.BreakerReset, err = envDuration("SPEEDTEST_GEO_PROVIDER_BREAKER_RESET", cfg.BreakerReset); err != nil {
		return err
	}
	if cfg.IPAPIEnabled, err = envBool("SPEEDTEST_GEO_IPAPI_ENABLED", cfg.IPAPIEnabled); err != nil {
		return err
	}
	cfg.IPAPIURL = envValue("SPEEDTEST_GEO_IPAPI_URL", cfg.IPAPIURL)
	if cfg.IPAPIRequestsPerMinute, err = envInt("SPEEDTEST_GEO_IPAPI_REQUESTS_PER_MINUTE", cfg.IPAPIRequestsPerMinute); err != nil {
		return err
	}
	if cfg.IPAPIRequestsPerDay, err = envInt("SPEEDTEST_GEO_IPAPI_REQUESTS_PER_DAY", cfg.IPAPIRequestsPerDay); err != nil {
		return err
	}
	if cfg.IPInfoEnabled, err = envBool("SPEEDTEST_GEO_IPINFO_ENABLED", cfg.IPInfoEnabled); err != nil {
		return err
	}
	cfg.IPInfoURL = envValue("SPEEDTEST_GEO_IPINFO_URL", cfg.IPInfoURL)
	if cfg.IPInfoRequestsPerMinute, err = envInt("SPEEDTEST_GEO_IPINFO_REQUESTS_PER_MINUTE", cfg.IPInfoRequestsPerMinute); err != nil {
		return err
	}
	if cfg.IPInfoRequestsPerDay, err = envInt("SPEEDTEST_GEO_IPINFO_REQUESTS_PER_DAY", cfg.IPInfoRequestsPerDay); err != nil {
		return err
	}
	return nil
}

func validateProviderConfig(cfg ProviderConfig) error {
	if cfg.Timeout <= 0 || cfg.FailureThreshold < 1 || cfg.BreakerReset <= 0 || cfg.IPAPIRequestsPerMinute < 0 || cfg.IPInfoRequestsPerMinute < 0 || cfg.IPAPIRequestsPerDay < 0 || cfg.IPInfoRequestsPerDay < 0 {
		return errors.New("geolocation provider limits must be positive and request rates cannot be negative")
	}
	if cfg.IPAPIEnabled && (cfg.IPAPIRequestsPerMinute == 0 || cfg.IPAPIRequestsPerMinute > 45) {
		return errors.New("SPEEDTEST_GEO_IPAPI_REQUESTS_PER_MINUTE must be between 1 and the free endpoint limit of 45")
	}
	if cfg.IPInfoEnabled && strings.TrimSpace(cfg.IPInfoToken) == "" {
		return errors.New("SPEEDTEST_GEO_IPINFO_TOKEN is required when IPinfo Lite is enabled")
	}
	return nil
}

func envValue(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envBool(key string, fallback bool) (bool, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean", key)
	}
	return value, nil
}

func envInt(key string, fallback int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", key)
	}
	return value, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration", key)
	}
	return value, nil
}

// NewFallbackProviders returns enabled providers in policy order, ending with
// the ASN-only RIPEstat fallback.
func NewFallbackProviders(cfg ProviderConfig) ([]Fallback, error) {
	providers := make([]Fallback, 0, 7)
	if cfg.IPInfoEnabled {
		provider, err := NewIPInfoLiteProvider(cfg)
		if err != nil {
			return nil, err
		}
		providers = append(providers, provider)
	}
	if cfg.IPAPIEnabled {
		provider, err := NewIPAPIProvider(cfg)
		if err != nil {
			return nil, err
		}
		providers = append(providers, provider)
	}
	providers, err := extraProviders(cfg, providers)
	if err != nil {
		return nil, err
	}
	return budgetProviders(providers, cfg.Budget), nil
}

// IPAPIProvider implements the free ip-api.com JSON endpoint.
type IPAPIProvider struct {
	core         *providerCore
	batchLimiter *rateLimiter
}

type ipAPIBatchResponse struct {
	Status       string  `json:"status"`
	Query        string  `json:"query"`
	City         string  `json:"city"`
	Region       string  `json:"regionName"`
	Country      string  `json:"countryCode"`
	Latitude     float64 `json:"lat"`
	Longitude    float64 `json:"lon"`
	TimeZone     string  `json:"timezone"`
	Organisation string  `json:"org"`
	AS           string  `json:"as"`
}

func NewIPAPIProvider(cfg ProviderConfig) (*IPAPIProvider, error) {
	core, err := newProviderCore("ip-api", cfg.IPAPIURL, cfg.IPAPIRequestsPerMinute, cfg)
	if err != nil {
		return nil, err
	}
	return &IPAPIProvider{core: core, batchLimiter: newRateLimiter(15)}, nil
}

func (p *IPAPIProvider) Source() string { return "ip-api" }

// LookupBatch refreshes up to one hundred addresses with one ip-api request.
func (p *IPAPIProvider) LookupBatch(ctx context.Context, addresses []string, purpose budget.Purpose) (map[string]Location, error) {
	if len(addresses) == 0 || len(addresses) > 100 {
		return nil, errors.New("ip-api batch must contain between one and one hundred addresses")
	}
	canonical := make([]string, len(addresses))
	for i, address := range addresses {
		ip, err := validatedIP(address)
		if err != nil {
			return nil, err
		}
		canonical[i] = ip
	}
	requestCtx, cancel := context.WithTimeout(ctx, p.core.timeout)
	defer cancel()
	if p.core.budget == nil {
		if err := p.batchLimiter.Wait(requestCtx); err != nil {
			return nil, err
		}
	}
	body, _ := json.Marshal(canonical)
	endpoint, err := ipAPIBatchEndpoint(p.core.endpoint)
	if err != nil {
		p.observe("error")
		return nil, err
	}
	data, status, err := p.core.requestWithQuota(withProviderPurpose(requestCtx, purpose), http.MethodPost, endpoint, "", bytes.NewReader(body), p.batchLimiter, "ip-api-batch")
	if err != nil {
		if errors.Is(err, ErrBudgetExhausted) {
			p.observe("budget_exhausted")
		} else {
			p.observe("error")
		}
		return nil, err
	}
	if status != http.StatusOK {
		p.observe("error")
		return nil, fmt.Errorf("ip-api batch HTTP status %d", status)
	}
	locations, err := parseIPAPIBatch(data, canonical)
	if err != nil {
		p.observe("error")
		return nil, err
	}
	if len(locations) == 0 {
		p.observe("not_found")
	} else {
		p.observe("success")
	}
	return locations, nil
}

func parseIPAPIBatch(data []byte, canonical []string) (map[string]Location, error) {
	var responses []ipAPIBatchResponse
	if err := json.Unmarshal(data, &responses); err != nil {
		return nil, errors.New("invalid ip-api batch response")
	}
	locations := make(map[string]Location, len(responses))
	requested := make(map[string]struct{}, len(canonical))
	for _, ip := range canonical {
		requested[ip] = struct{}{}
	}
	for _, response := range responses {
		ip, err := validatedIP(response.Query)
		if _, ok := requested[ip]; err != nil || !ok || response.Status != "success" {
			continue
		}
		location := Location{City: response.City, Region: response.Region, Country: response.Country,
			Latitude: response.Latitude, Longitude: response.Longitude, TimeZone: response.TimeZone, Organisation: response.Organisation}
		location.ASN, location.Organisation = parseAS(response.AS, location.Organisation)
		if validateLocation(location) == nil && !emptyLocation(location) {
			locations[ip] = location
		}
	}
	return locations, nil
}

func (p *IPAPIProvider) observe(outcome string) {
	if p.core.observe != nil {
		p.core.observe(p.Source(), outcome)
	}
}

func ipAPIBatchEndpoint(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return "", errors.New("invalid ip-api batch endpoint")
	}
	u.Path = strings.TrimSuffix(u.Path, "/json")
	u.Path = path.Join(u.Path, "batch")
	query := u.Query()
	query.Set("fields", "status,message,city,regionName,countryCode,lat,lon,timezone,org,as,query")
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func (p *IPAPIProvider) Lookup(ctx context.Context, ip string) (Location, error) {
	canonical, err := validatedIP(ip)
	if err != nil {
		return Location{}, err
	}
	ip = canonical
	return p.core.lookup(ctx, func(ctx context.Context) (Location, bool, error) {
		endpoint, err := endpointForIP(p.core.endpoint, ip, true)
		if err != nil {
			return Location{}, true, errors.New("invalid ip-api endpoint")
		}
		body, status, err := p.core.get(ctx, endpoint, "")
		if err != nil {
			return Location{}, true, err
		}
		if status != http.StatusOK {
			return Location{}, true, fmt.Errorf("ip-api HTTP status %d", status)
		}
		var response struct {
			Status       string  `json:"status"`
			Query        string  `json:"query"`
			City         string  `json:"city"`
			Region       string  `json:"regionName"`
			Country      string  `json:"countryCode"`
			Latitude     float64 `json:"lat"`
			Longitude    float64 `json:"lon"`
			TimeZone     string  `json:"timezone"`
			Organisation string  `json:"org"`
			AS           string  `json:"as"`
		}
		if err := json.Unmarshal(body, &response); err != nil {
			return Location{}, true, errors.New("invalid ip-api response")
		}
		if response.Status == "fail" {
			return Location{}, false, ErrNotFound
		}
		if response.Status != "success" || !matchesResponseIP(response.Query, ip) {
			return Location{}, true, errors.New("unexpected ip-api response")
		}
		location := Location{
			City: response.City, Region: response.Region, Country: response.Country,
			Latitude: response.Latitude, Longitude: response.Longitude,
			TimeZone: response.TimeZone, Organisation: response.Organisation,
		}
		location.ASN, location.Organisation = parseAS(response.AS, location.Organisation)
		if err := validateLocation(location); err != nil {
			return Location{}, true, err
		}
		if emptyLocation(location) {
			return Location{}, false, ErrNotFound
		}
		return location, false, nil
	})
}

// IPInfoLiteProvider implements IPinfo's authenticated free Lite endpoint.
// Lite returns country and basic ASN fields; city and coordinates are absent.
type IPInfoLiteProvider struct {
	core  *providerCore
	token string
}

func NewIPInfoLiteProvider(cfg ProviderConfig) (*IPInfoLiteProvider, error) {
	if strings.TrimSpace(cfg.IPInfoToken) == "" {
		return nil, errors.New("IPinfo Lite token is required")
	}
	core, err := newProviderCore("ipinfo-lite", cfg.IPInfoURL, cfg.IPInfoRequestsPerMinute, cfg)
	if err != nil {
		return nil, err
	}
	return &IPInfoLiteProvider{core: core, token: cfg.IPInfoToken}, nil
}

func (p *IPInfoLiteProvider) Source() string { return "ipinfo-lite" }

func (p *IPInfoLiteProvider) Lookup(ctx context.Context, ip string) (Location, error) {
	canonical, err := validatedIP(ip)
	if err != nil {
		return Location{}, err
	}
	ip = canonical
	return p.core.lookup(ctx, func(ctx context.Context) (Location, bool, error) {
		endpoint, err := endpointForIP(p.core.endpoint, ip, false)
		if err != nil {
			return Location{}, true, errors.New("invalid IPinfo endpoint")
		}
		body, status, err := p.core.get(ctx, endpoint, p.token)
		if err != nil {
			return Location{}, true, err
		}
		if status != http.StatusOK {
			return Location{}, true, fmt.Errorf("IPinfo HTTP status %d", status)
		}
		var response struct {
			IP      string `json:"ip"`
			Country string `json:"country_code"`
			ASN     string `json:"asn"`
			ASName  string `json:"as_name"`
		}
		if err := json.Unmarshal(body, &response); err != nil {
			return Location{}, true, errors.New("invalid IPinfo response")
		}
		if !matchesResponseIP(response.IP, ip) {
			return Location{}, true, errors.New("unexpected IPinfo response")
		}
		asn, organisation := parseAS(response.ASN+" "+response.ASName, "")
		location := Location{Country: response.Country, ASN: asn, Organisation: organisation}
		if err := validateLocation(location); err != nil {
			return Location{}, true, err
		}
		if emptyLocation(location) {
			return Location{}, false, ErrNotFound
		}
		return location, false, nil
	})
}

type providerCore struct {
	name     string
	endpoint string
	timeout  time.Duration
	limiter  *rateLimiter
	breaker  *circuitBreaker
	client   *http.Client
	budget   *budget.Broker
	observe  func(string, string)
}

func newProviderCore(name, endpoint string, requestsPerMinute int, cfg ProviderConfig) (*providerCore, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("provider endpoint must be an absolute HTTP URL without credentials or query")
	}
	if name != "ip-api" && u.Scheme != "https" && !isLoopbackHost(u.Hostname()) {
		return nil, errors.New("this provider endpoint requires HTTPS outside loopback")
	}
	if cfg.Timeout <= 0 || cfg.FailureThreshold < 1 || cfg.BreakerReset <= 0 || requestsPerMinute < 0 {
		return nil, errors.New("invalid geolocation provider limits")
	}
	if name == "ip-api" && (requestsPerMinute == 0 || requestsPerMinute > 45) {
		return nil, errors.New("ip-api free endpoint limit must be between 1 and 45 requests per minute")
	}
	client := &http.Client{
		Timeout: cfg.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 || len(via) == 0 || !strings.EqualFold(req.URL.Host, via[0].URL.Host) || (via[0].URL.Scheme == "https" && req.URL.Scheme != "https") {
				return errors.New("unsafe provider redirect")
			}
			return nil
		},
	}
	return &providerCore{
		name:     name,
		endpoint: strings.TrimRight(endpoint, "/"), timeout: cfg.Timeout,
		limiter: newRateLimiter(requestsPerMinute),
		breaker: newCircuitBreaker(cfg.FailureThreshold, cfg.BreakerReset),
		client:  client, budget: cfg.Budget, observe: cfg.Observe,
	}, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func endpointForIP(base, ip string, fields bool) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "", errors.New("invalid endpoint")
	}
	u.Path = path.Join(u.Path, url.PathEscape(ip))
	if fields {
		query := u.Query()
		query.Set("fields", "status,message,city,regionName,countryCode,lat,lon,timezone,org,as,query")
		u.RawQuery = query.Encode()
	}
	return u.String(), nil
}

func (c *providerCore) get(ctx context.Context, endpoint, token string) ([]byte, int, error) {
	return c.request(ctx, http.MethodGet, endpoint, token, nil)
}

func (c *providerCore) request(ctx context.Context, method, endpoint, token string, requestBody io.Reader) ([]byte, int, error) {
	return c.requestWithLimiter(ctx, method, endpoint, token, requestBody, c.limiter)
}

func (c *providerCore) requestWithLimiter(ctx context.Context, method, endpoint, token string, requestBody io.Reader, limiter *rateLimiter) ([]byte, int, error) {
	return c.requestWithQuota(ctx, method, endpoint, token, requestBody, limiter, c.name)
}

func (c *providerCore) requestWithQuota(ctx context.Context, method, endpoint, token string, requestBody io.Reader, limiter *rateLimiter, quotaProvider string) ([]byte, int, error) {
	request, err := http.NewRequestWithContext(ctx, method, endpoint, requestBody)
	if err != nil {
		return nil, 0, errors.New("cannot create provider request")
	}
	request.Header.Set("Accept", "application/json")
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if c.budget != nil {
		var allowed bool
		if quotaProvider == "ip-api-batch" {
			allowed, err = c.budget.AllowTogether(ctx, providerPurpose(ctx), c.name, quotaProvider)
		} else {
			allowed, err = c.budget.Allow(ctx, quotaProvider, providerPurpose(ctx))
		}
		if err != nil {
			return nil, 0, errors.New("cannot reserve provider quota")
		}
		if !allowed {
			return nil, 0, ErrBudgetExhausted
		}
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, 0, errors.New("provider request failed")
	}
	defer func() { _ = response.Body.Close() }()
	limiter.ObserveResponse(response.Header, response.StatusCode)
	if c.budget != nil {
		remaining := -1
		if value, parseErr := strconv.Atoi(response.Header.Get("X-Rl")); parseErr == nil {
			remaining = value
		}
		ttl := time.Duration(0)
		if value, parseErr := strconv.Atoi(response.Header.Get("X-Ttl")); parseErr == nil && value > 0 {
			ttl = time.Duration(value) * time.Second
		}
		if err := c.budget.ObserveQuota(ctx, quotaProvider, remaining, ttl, response.StatusCode); err != nil {
			return nil, response.StatusCode, errors.New("cannot persist provider quota cooldown")
		}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxProviderResponse+1))
	if err != nil || len(body) > maxProviderResponse {
		return nil, response.StatusCode, errors.New("cannot read provider response")
	}
	return body, response.StatusCode, nil
}

func (c *providerCore) lookup(ctx context.Context, request func(context.Context) (Location, bool, error)) (Location, error) {
	outcome := "success"
	defer func() {
		if c.observe != nil {
			c.observe(c.name, outcome)
		}
	}()
	if err := ctx.Err(); err != nil {
		outcome = "error"
		return Location{}, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	allowed, probe := c.breaker.Allow()
	if !allowed {
		outcome = "error"
		return Location{}, errors.New("geolocation provider circuit breaker is open")
	}
	if err := c.limiter.Wait(requestCtx); err != nil {
		outcome = "error"
		c.breaker.Abandon(probe)
		if ctx.Err() != nil {
			return Location{}, ctx.Err()
		}
		if requestCtx.Err() != nil {
			return Location{}, errors.New("provider rate limit wait timed out")
		}
		return Location{}, err
	}
	location, failed, err := request(requestCtx)
	switch {
	case errors.Is(err, ErrBudgetExhausted):
		outcome = "budget_exhausted"
	case errors.Is(err, ErrNotFound):
		outcome = "not_found"
	case err != nil:
		outcome = "error"
	}
	timedOut := requestCtx.Err() != nil && ctx.Err() == nil
	if ctx.Err() != nil {
		c.breaker.Abandon(probe)
		return Location{}, ctx.Err()
	}
	c.breaker.Complete(probe, failed || timedOut)
	return location, err
}

type rateLimiter struct {
	mu          sync.Mutex
	interval    time.Duration
	next        time.Time
	blockedTill time.Time
}

func newRateLimiter(requestsPerMinute int) *rateLimiter {
	limiter := &rateLimiter{}
	if requestsPerMinute > 0 {
		limiter.interval = time.Minute / time.Duration(requestsPerMinute)
	}
	return limiter
}

func (l *rateLimiter) Wait(ctx context.Context) error {
	for {
		l.mu.Lock()
		now := time.Now()
		target := l.next
		if l.blockedTill.After(target) {
			target = l.blockedTill
		}
		if !target.After(now) {
			l.next = now.Add(l.interval)
			l.mu.Unlock()
			return nil
		}
		l.mu.Unlock()
		timer := time.NewTimer(time.Until(target))
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (l *rateLimiter) Observe(header http.Header) {
	if header.Get("X-Rl") != "0" {
		return
	}
	l.BlockFor(header)
}

func (l *rateLimiter) ObserveResponse(header http.Header, status int) {
	l.Observe(header)
	if status == http.StatusTooManyRequests && header.Get("X-Rl") != "0" {
		l.BlockFor(header)
	}
}

func (l *rateLimiter) BlockFor(header http.Header) {
	seconds, err := strconv.Atoi(header.Get("X-Ttl"))
	if err != nil || seconds < 1 {
		seconds = 60
	}
	blockedTill := time.Now().Add(time.Duration(seconds) * time.Second)
	l.mu.Lock()
	if blockedTill.After(l.blockedTill) {
		l.blockedTill = blockedTill
	}
	l.mu.Unlock()
}

type circuitBreaker struct {
	mu        sync.Mutex
	threshold int
	reset     time.Duration
	failures  int
	openUntil time.Time
	probe     bool
}

func newCircuitBreaker(threshold int, reset time.Duration) *circuitBreaker {
	return &circuitBreaker{threshold: threshold, reset: reset}
}

func (b *circuitBreaker) Allow() (bool, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.IsZero() {
		return true, false
	}
	if time.Now().Before(b.openUntil) || b.probe {
		return false, false
	}
	b.probe = true
	return true, true
}

func (b *circuitBreaker) Complete(probe, failed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if probe {
		if !b.probe {
			return
		}
		b.probe = false
		if !failed {
			b.failures = 0
			b.openUntil = time.Time{}
			return
		}
		b.openUntil = time.Now().Add(b.reset)
		return
	}
	// Requests already in flight when the breaker opened must not close a later
	// half-open probe.
	if !b.openUntil.IsZero() {
		return
	}
	if !failed {
		b.failures = 0
		return
	}
	b.failures++
	if b.failures >= b.threshold {
		b.openUntil = time.Now().Add(b.reset)
	}
}

func (b *circuitBreaker) Abandon(probe bool) {
	if !probe {
		return
	}
	b.mu.Lock()
	b.probe = false
	b.mu.Unlock()
}

func parseAS(raw, fallbackOrganisation string) (uint, string) {
	parts := strings.Fields(strings.TrimSpace(raw))
	if len(parts) == 0 {
		return 0, fallbackOrganisation
	}
	var asn uint
	if strings.HasPrefix(strings.ToUpper(parts[0]), "AS") {
		parsed, err := strconv.ParseUint(parts[0][2:], 10, 0)
		if err == nil {
			asn = uint(parsed)
			parts = parts[1:]
		}
	}
	organisation := strings.TrimSpace(strings.Join(parts, " "))
	if organisation == "" {
		organisation = fallbackOrganisation
	}
	return asn, organisation
}

func matchesResponseIP(responseIP, requestedIP string) bool {
	if responseIP == "" {
		return false
	}
	response, requested := net.ParseIP(responseIP), net.ParseIP(requestedIP)
	return response != nil && requested != nil && response.Equal(requested)
}

func validatedIP(address string) (string, error) {
	ip := net.ParseIP(address)
	if ip == nil {
		return "", ErrInvalidIP
	}
	return ip.String(), nil
}

func emptyLocation(location Location) bool {
	return location.City == "" && location.Region == "" && location.Country == "" && location.ASN == 0 && location.Organisation == "" && location.TimeZone == "" && location.Latitude == 0 && location.Longitude == 0
}

func validateLocation(location Location) error {
	if location.Latitude < -90 || location.Latitude > 90 || location.Longitude < -180 || location.Longitude > 180 {
		return errors.New("provider returned invalid coordinates")
	}
	return nil
}
