package geo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// extraProviderEnv reads the provider controls added after the original fallback chain.
func extraProviderEnv(cfg *ProviderConfig) error {
	var err error
	for _, p := range []struct {
		enabled *bool
		url     *string
		rpm     *int
		rpd     *int
		key     string
	}{
		{&cfg.IPWhoEnabled, &cfg.IPWhoURL, &cfg.IPWhoRequestsPerMinute, &cfg.IPWhoRequestsPerDay, "IPWHO"},
		{&cfg.FreeIPAPIEnabled, &cfg.FreeIPAPIURL, &cfg.FreeIPAPIRequestsPerMinute, &cfg.FreeIPAPIRequestsPerDay, "FREEIPAPI"},
		{&cfg.GeoJSEnabled, &cfg.GeoJSURL, &cfg.GeoJSRequestsPerMinute, &cfg.GeoJSRequestsPerDay, "GEOJS"},
		{&cfg.RIPEStatEnabled, &cfg.RIPEStatURL, &cfg.RIPEStatRequestsPerMinute, &cfg.RIPEStatRequestsPerDay, "RIPESTAT"},
		{&cfg.IPAPICoEnabled, &cfg.IPAPICoURL, &cfg.IPAPICoRequestsPerMinute, &cfg.IPAPICoRequestsPerDay, "IPAPICO"},
		{&cfg.IPGeoEnabled, &cfg.IPGeoURL, &cfg.IPGeoRequestsPerMinute, &cfg.IPGeoRequestsPerDay, "IPGEOLOCATION"},
	} {
		if *p.enabled, err = envBool("SPEEDTEST_GEO_"+p.key+"_ENABLED", *p.enabled); err != nil {
			return err
		}
		*p.url = envValue("SPEEDTEST_GEO_"+p.key+"_URL", *p.url)
		if *p.rpm, err = envInt("SPEEDTEST_GEO_"+p.key+"_REQUESTS_PER_MINUTE", *p.rpm); err != nil {
			return err
		}
		if *p.rpm < 0 {
			return errors.New("geolocation provider request rates cannot be negative")
		}
		if *p.rpd, err = envInt("SPEEDTEST_GEO_"+p.key+"_REQUESTS_PER_DAY", *p.rpd); err != nil {
			return err
		}
		if *p.rpd < 0 {
			return errors.New("geolocation provider daily limits cannot be negative")
		}
	}
	if cfg.IPGeoEnabled && strings.TrimSpace(cfg.IPGeoKey) == "" {
		return errors.New("SPEEDTEST_GEO_IPGEOLOCATION_KEY is required when ipgeolocation.io is enabled")
	}
	cfg.Consensus, err = envBool("SPEEDTEST_GEO_CONSENSUS", cfg.Consensus)
	return err
}

// extraProviders appends the enabled optional providers after the ones already built.
func extraProviders(cfg ProviderConfig, providers []Fallback) ([]Fallback, error) {
	if cfg.IPWhoEnabled {
		p, err := newJSONProvider("ipwho", cfg.IPWhoURL, cfg.IPWhoRequestsPerMinute, "", cfg, parseIPWho, ipwhoEndpoint)
		if err != nil {
			return nil, err
		}
		providers = append(providers, p)
	}
	if cfg.FreeIPAPIEnabled {
		p, err := newJSONProvider("freeipapi", cfg.FreeIPAPIURL, cfg.FreeIPAPIRequestsPerMinute, "", cfg, parseFreeIPAPI, freeIPAPIEndpoint)
		if err != nil {
			return nil, err
		}
		providers = append(providers, p)
	}
	if cfg.GeoJSEnabled {
		p, err := newJSONProvider("geojs", cfg.GeoJSURL, cfg.GeoJSRequestsPerMinute, "", cfg, parseGeoJS, geoJSEndpoint)
		if err != nil {
			return nil, err
		}
		providers = append(providers, p)
	}
	if cfg.IPAPICoEnabled {
		p, err := newJSONProvider("ipapi-co", cfg.IPAPICoURL, cfg.IPAPICoRequestsPerMinute, "", cfg, parseIPAPICo, ipapicoEndpoint)
		if err != nil {
			return nil, err
		}
		providers = append(providers, p)
	}
	if cfg.IPGeoEnabled {
		if strings.TrimSpace(cfg.IPGeoKey) == "" {
			return nil, errors.New("ipgeolocation.io key is required")
		}
		p, err := newJSONProvider("ipgeolocation", cfg.IPGeoURL, cfg.IPGeoRequestsPerMinute, cfg.IPGeoKey, cfg, parseIPGeo, ipgeoEndpoint)
		if err != nil {
			return nil, err
		}
		providers = append(providers, p)
	}
	if cfg.RIPEStatEnabled {
		p, err := newJSONProvider("ripestat", cfg.RIPEStatURL, cfg.RIPEStatRequestsPerMinute, "", cfg, parseRIPEStat, ripeStatEndpoint)
		if err != nil {
			return nil, err
		}
		providers = append(providers, p)
	}
	return providers, nil
}

func ripeStatEndpoint(base, ip, _ string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "", errors.New("invalid endpoint")
	}
	q := u.Query()
	q.Set("resource", ip)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func parseRIPEStat(body []byte, _ string) (Location, error) {
	var r struct {
		Data struct {
			ASNs []json.RawMessage `json:"asns"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return Location{}, errors.New("invalid RIPEstat response")
	}
	for _, raw := range r.Data.ASNs {
		var number uint64
		if json.Unmarshal(raw, &number) == nil && number > 0 {
			return Location{ASN: uint(number)}, nil
		}
		var text string
		if json.Unmarshal(raw, &text) == nil {
			asn, _ := parseAS(text, "")
			if asn > 0 {
				return Location{ASN: asn}, nil
			}
		}
	}
	return Location{}, ErrNotFound
}

func freeIPAPIEndpoint(base, ip, _ string) (string, error) { return pathEndpoint(base, ip, "") }

func geoJSEndpoint(base, ip, _ string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "", errors.New("invalid endpoint")
	}
	q := u.Query()
	q.Set("ip", ip)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// jsonProvider is a provider whose only differences are the URL shape and the response mapping.
type jsonProvider struct {
	name     string
	core     *providerCore
	key      string
	endpoint func(base, ip, key string) (string, error)
	parse    func(body []byte, ip string) (Location, error)
}

func newJSONProvider(name, base string, rpm int, key string, cfg ProviderConfig,
	parse func([]byte, string) (Location, error), endpoint func(string, string, string) (string, error)) (*jsonProvider, error) {
	core, err := newProviderCore(name, base, rpm, cfg)
	if err != nil {
		return nil, err
	}
	return &jsonProvider{name: name, core: core, key: key, endpoint: endpoint, parse: parse}, nil
}

func (p *jsonProvider) Source() string { return p.name }

func (p *jsonProvider) Lookup(ctx context.Context, ip string) (Location, error) {
	canonical, err := validatedIP(ip)
	if err != nil {
		return Location{}, err
	}
	return p.core.lookup(ctx, func(ctx context.Context) (Location, bool, error) {
		target, err := p.endpoint(p.core.endpoint, canonical, p.key)
		if err != nil {
			return Location{}, true, fmt.Errorf("invalid %s endpoint", p.name)
		}
		body, status, err := p.core.get(ctx, target, "")
		if err != nil {
			return Location{}, true, err
		}
		if status == http.StatusNotFound {
			return Location{}, false, ErrNotFound
		}
		if status != http.StatusOK {
			return Location{}, true, fmt.Errorf("%s HTTP status %d", p.name, status)
		}
		location, err := p.parse(body, canonical)
		if errors.Is(err, ErrNotFound) {
			return Location{}, false, ErrNotFound
		}
		if err != nil {
			return Location{}, true, err
		}
		if err := validateLocation(location); err != nil {
			return Location{}, true, err
		}
		if emptyLocation(location) {
			return Location{}, false, ErrNotFound
		}
		return location, false, nil
	})
}

func pathEndpoint(base, ip, suffix string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "", errors.New("invalid endpoint")
	}
	u.Path = path.Join(u.Path, url.PathEscape(ip)) + suffix
	return u.String(), nil
}

func ipwhoEndpoint(base, ip, _ string) (string, error) { return pathEndpoint(base, ip, "") }

func ipapicoEndpoint(base, ip, _ string) (string, error) { return pathEndpoint(base, ip, "/json/") }

// ipgeoEndpoint puts the key in the query as the service requires; it never appears in errors or logs.
func ipgeoEndpoint(base, ip, key string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "", errors.New("invalid endpoint")
	}
	q := url.Values{"apiKey": {key}, "ip": {ip}}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func parseIPWho(body []byte, ip string) (Location, error) {
	var r struct {
		IP         string  `json:"ip"`
		Success    bool    `json:"success"`
		Country    string  `json:"country_code"`
		Region     string  `json:"region"`
		City       string  `json:"city"`
		Latitude   float64 `json:"latitude"`
		Longitude  float64 `json:"longitude"`
		Connection struct {
			ASN uint   `json:"asn"`
			Org string `json:"org"`
			ISP string `json:"isp"`
		} `json:"connection"`
		TimeZone struct {
			ID string `json:"id"`
		} `json:"timezone"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return Location{}, errors.New("invalid ipwho response")
	}
	if !r.Success {
		return Location{}, ErrNotFound
	}
	if !matchesResponseIP(r.IP, ip) {
		return Location{}, errors.New("unexpected ipwho response")
	}
	org := r.Connection.Org
	if org == "" {
		org = r.Connection.ISP
	}
	return Location{City: r.City, Region: r.Region, Country: r.Country, Latitude: r.Latitude, Longitude: r.Longitude,
		TimeZone: r.TimeZone.ID, ASN: r.Connection.ASN, Organisation: org}, nil
}

func parseFreeIPAPI(body []byte, ip string) (Location, error) {
	var r struct {
		IP           string          `json:"ipAddress"`
		Country      string          `json:"countryCode"`
		Region       string          `json:"regionName"`
		City         string          `json:"cityName"`
		Latitude     float64         `json:"latitude"`
		Longitude    float64         `json:"longitude"`
		TimeZones    []string        `json:"timeZones"`
		ASN          json.RawMessage `json:"asn"`
		Organisation string          `json:"asnOrganization"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return Location{}, errors.New("invalid freeipapi response")
	}
	if !matchesResponseIP(r.IP, ip) {
		return Location{}, errors.New("unexpected freeipapi response")
	}
	asn, organisation := parseASNValue(r.ASN, r.Organisation)
	location := Location{City: r.City, Region: r.Region, Country: r.Country, Latitude: r.Latitude,
		Longitude: r.Longitude, ASN: asn, Organisation: organisation}
	if len(r.TimeZones) != 0 {
		location.TimeZone = r.TimeZones[0]
	}
	return location, nil
}

func parseGeoJS(body []byte, ip string) (Location, error) {
	var r struct {
		IP           string          `json:"ip"`
		Country      string          `json:"country_code"`
		Region       string          `json:"region"`
		City         string          `json:"city"`
		Latitude     string          `json:"latitude"`
		Longitude    string          `json:"longitude"`
		TimeZone     string          `json:"timezone"`
		ASN          json.RawMessage `json:"asn"`
		Organization string          `json:"organization"`
		OrgName      string          `json:"organization_name"`
		Accuracy     uint16          `json:"accuracy"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return Location{}, errors.New("invalid GeoJS response")
	}
	if !matchesResponseIP(r.IP, ip) {
		return Location{}, errors.New("unexpected GeoJS response")
	}
	var asnNumber uint64
	_ = json.Unmarshal(r.ASN, &asnNumber)
	organisation := r.OrgName
	if organisation == "" {
		_, organisation = parseAS(string(r.ASN), r.Organization)
	}
	latitude, _ := strconv.ParseFloat(r.Latitude, 64)
	longitude, _ := strconv.ParseFloat(r.Longitude, 64)
	return Location{City: r.City, Region: r.Region, Country: r.Country, Latitude: latitude, Longitude: longitude,
		TimeZone: r.TimeZone, AccuracyRadius: r.Accuracy, ASN: uint(asnNumber), Organisation: organisation}, nil
}

func parseASNValue(raw json.RawMessage, fallbackOrganisation string) (uint, string) {
	var number uint64
	if json.Unmarshal(raw, &number) == nil {
		return uint(number), fallbackOrganisation
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return 0, fallbackOrganisation
	}
	if strings.HasPrefix(strings.ToUpper(value), "AS") {
		return parseAS(value, fallbackOrganisation)
	}
	number, _ = strconv.ParseUint(value, 10, 0)
	return uint(number), fallbackOrganisation
}

func parseIPAPICo(body []byte, ip string) (Location, error) {
	var r struct {
		IP        string  `json:"ip"`
		Error     bool    `json:"error"`
		City      string  `json:"city"`
		Region    string  `json:"region"`
		Country   string  `json:"country_code"`
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
		TimeZone  string  `json:"timezone"`
		ASN       string  `json:"asn"`
		Org       string  `json:"org"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return Location{}, errors.New("invalid ipapi.co response")
	}
	if r.Error {
		return Location{}, ErrNotFound
	}
	if !matchesResponseIP(r.IP, ip) {
		return Location{}, errors.New("unexpected ipapi.co response")
	}
	asn, org := parseAS(r.ASN+" "+r.Org, r.Org)
	return Location{City: r.City, Region: r.Region, Country: r.Country, Latitude: r.Latitude, Longitude: r.Longitude,
		TimeZone: r.TimeZone, ASN: asn, Organisation: org}, nil
}

func parseIPGeo(body []byte, ip string) (Location, error) {
	var r struct {
		IP           string `json:"ip"`
		Message      string `json:"message"`
		Country      string `json:"country_code2"`
		Region       string `json:"state_prov"`
		City         string `json:"city"`
		Latitude     string `json:"latitude"`
		Longitude    string `json:"longitude"`
		Organisation string `json:"organization"`
		ISP          string `json:"isp"`
		ASN          string `json:"asn"`
		TimeZone     struct {
			Name string `json:"name"`
		} `json:"time_zone"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return Location{}, errors.New("invalid ipgeolocation response")
	}
	if r.IP == "" {
		return Location{}, ErrNotFound
	}
	if !matchesResponseIP(r.IP, ip) {
		return Location{}, errors.New("unexpected ipgeolocation response")
	}
	lat, _ := strconv.ParseFloat(r.Latitude, 64)
	lon, _ := strconv.ParseFloat(r.Longitude, 64)
	org := r.Organisation
	if org == "" {
		org = r.ISP
	}
	asn, org := parseAS(r.ASN, org)
	return Location{City: r.City, Region: r.Region, Country: r.Country, Latitude: lat, Longitude: lon,
		TimeZone: r.TimeZone.Name, ASN: asn, Organisation: org}, nil
}
