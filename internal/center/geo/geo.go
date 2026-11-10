package geo

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/oschwald/geoip2-golang"
	"github.com/oschwald/maxminddb-golang"
)

var (
	// ErrNotFound allows a caller to invoke its own fallback policy.
	ErrNotFound  = errors.New("geolocation not found")
	ErrInvalidIP = errors.New("invalid IP address")
)

// Location is a combination of City and ASN data, using English place names.
// Country is the ISO 3166-1 alpha-2 code; accuracy radius is in kilometres.
type Location struct {
	City           string       `json:"city"`
	Region         string       `json:"region"`
	Country        string       `json:"country"`
	Latitude       float64      `json:"latitude"`
	Longitude      float64      `json:"longitude"`
	TimeZone       string       `json:"time_zone"`
	AccuracyRadius uint16       `json:"accuracy_radius"`
	ASN            uint         `json:"asn"`
	Organisation   string       `json:"organisation"`
	QualityScore   int          `json:"quality_score"`
	FieldSources   FieldSources `json:"field_sources"`
	AgeDays        int          `json:"-"`
}

// FieldSources keeps per-field provenance comparable with Location, which is
// useful to callers that treat locations as value objects.
type FieldSources struct {
	City           string `json:"city,omitempty"`
	Region         string `json:"region,omitempty"`
	Country        string `json:"country,omitempty"`
	TimeZone       string `json:"time_zone,omitempty"`
	Organisation   string `json:"organisation,omitempty"`
	Coordinates    string `json:"coordinates,omitempty"`
	ASN            string `json:"asn,omitempty"`
	AccuracyRadius string `json:"accuracy_radius,omitempty"`
}

// Geo is the offline lookup boundary used by the command center.
type Geo interface {
	Lookup(context.Context, string) (Location, error)
}

// Reader serializes replacement and close against concurrent lookups. Database
// contents live in memory, so no lookup sends an IP over the network.
type Reader struct {
	mu         sync.RWMutex
	updateMu   sync.Mutex
	cfg        Config
	city, asn  *geoip2.Reader
	info       *maxminddb.Reader
	log        *slog.Logger
	missing    sync.Once
	keyMissing sync.Once
}

// Open permits missing or invalid databases: lookups return ErrNotFound and a
// single warning. A later successful update makes the same Reader usable.
func Open(cfg Config, logger *slog.Logger) (*Reader, error) {
	if (cfg.CityPath == "" || cfg.ASNPath == "") && cfg.IPInfoPath == "" {
		return nil, errors.New("at least one offline geolocation database path is required")
	}
	var city, asn string
	var err error
	if cfg.CityPath != "" {
		city, err = filepath.Abs(cfg.CityPath)
		if err != nil {
			return nil, errors.New("invalid City path")
		}
	}
	if cfg.ASNPath != "" {
		asn, err = filepath.Abs(cfg.ASNPath)
		if err != nil {
			return nil, errors.New("invalid ASN path")
		}
	}
	if city != "" && city == asn {
		return nil, errors.New("city and ASN paths must differ")
	}
	if logger == nil {
		logger = slog.Default()
	}
	r := &Reader{cfg: cfg, log: logger}
	r.city = load(cfg.CityPath, "City")
	r.asn = load(cfg.ASNPath, "ASN")
	r.info = loadIPInfo(cfg.IPInfoPath)
	if (r.city == nil || r.asn == nil) && r.info == nil {
		r.warnMissing()
	}
	return r, nil
}

func load(path, kind string) *geoip2.Reader {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	db, err := decode(data, kind)
	if err != nil {
		return nil
	}
	return db
}

func decode(data []byte, kind string) (*geoip2.Reader, error) {
	verified, err := maxminddb.FromBytes(data)
	if err != nil {
		return nil, errors.New("invalid MMDB")
	}
	verifyErr := verified.Verify()
	_ = verified.Close()
	if verifyErr != nil {
		return nil, errors.New("invalid MMDB structure")
	}
	db, err := geoip2.FromBytes(data)
	if err != nil {
		return nil, errors.New("invalid MMDB")
	}
	if !editionMatches(db.Metadata().DatabaseType, kind) {
		_ = db.Close()
		return nil, errors.New("unexpected MMDB edition")
	}
	return db, nil
}

func loadIPInfo(path string) *maxminddb.Reader {
	if path == "" {
		return nil
	}
	db, err := maxminddb.Open(path)
	if err != nil {
		return nil
	}
	typeName := strings.ToLower(db.Metadata.DatabaseType)
	if !strings.Contains(typeName, "ipinfo") || !strings.Contains(typeName, "lite") {
		_ = db.Close()
		return nil
	}
	if err := db.Verify(); err != nil {
		_ = db.Close()
		return nil
	}
	return db
}

// editionMatches accepts MaxMind editions ("GeoLite2-City") and the DB-IP Lite editions in the same format ("DBIP-City-Lite", "DBIP-ASN-Lite (compat=GeoLite2-ASN)").
func editionMatches(databaseType, kind string) bool {
	databaseType = strings.TrimSuffix(databaseType, " (compat=GeoLite2-"+kind+")")
	return strings.HasSuffix(databaseType, "-"+kind) || strings.HasSuffix(databaseType, "-"+kind+"-Lite")
}

func (r *Reader) warnMissing() {
	r.missing.Do(func() { r.log.Warn("GeoLite2 database unavailable; geolocation returns not found") })
}

func (r *Reader) Lookup(ctx context.Context, address string) (Location, error) {
	if err := ctx.Err(); err != nil {
		return Location{}, err
	}
	ip := net.ParseIP(address)
	if ip == nil {
		return Location{}, ErrInvalidIP
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.city == nil && r.asn == nil && r.info == nil {
		r.warnMissing()
		return Location{}, ErrNotFound
	}
	var result Location
	if r.city != nil {
		city, err := r.city.City(ip)
		if err != nil {
			return Location{}, errors.New("city lookup failed")
		}
		result = Location{City: city.City.Names["en"], Country: city.Country.IsoCode,
			Latitude: city.Location.Latitude, Longitude: city.Location.Longitude,
			TimeZone: city.Location.TimeZone, AccuracyRadius: city.Location.AccuracyRadius}
		if len(city.Subdivisions) > 0 {
			result.Region = city.Subdivisions[0].Names["en"]
		}
	}
	if r.asn != nil {
		asn, err := r.asn.ASN(ip)
		if err != nil {
			return Location{}, errors.New("ASN lookup failed")
		}
		result.ASN, result.Organisation = asn.AutonomousSystemNumber, asn.AutonomousSystemOrganization
	}
	result.FieldSources = sourcesFor(result, GeoLiteSource)
	if r.info != nil {
		var record struct {
			CountryCode string `maxminddb:"country_code"`
			ASN         string `maxminddb:"asn"`
			ASName      string `maxminddb:"as_name"`
		}
		if err := r.info.Lookup(ip, &record); err != nil {
			if emptyLocation(result) {
				return Location{}, errors.New("IPinfo Lite database lookup failed")
			}
		} else if info, err := parseIPInfoLiteRecord(record.CountryCode, record.ASN, record.ASName); err == nil {
			result, _ = MergeLocations(result, GeoLiteSource, info, "ipinfo-lite-mmdb")
		}
	}
	if emptyLocation(result) {
		return Location{}, ErrNotFound
	}
	return result, nil
}

func parseIPInfoLiteRecord(countryCode, rawASN, asName string) (Location, error) {
	location := Location{Country: strings.ToUpper(strings.TrimSpace(countryCode)), Organisation: strings.TrimSpace(asName)}
	if asn, _ := parseAS(strings.TrimSpace(rawASN), ""); asn > 0 {
		location.ASN = asn
	}
	if err := validateLocation(location); err != nil || emptyLocation(location) {
		return Location{}, ErrNotFound
	}
	location.FieldSources = sourcesFor(location, "ipinfo-lite-mmdb")
	return location, nil
}

// Close releases readers and is safe to call repeatedly. The lifecycle owner
// must cancel and join Run before Close to prevent further scheduled updates.
func (r *Reader) Close() error {
	r.updateMu.Lock()
	defer r.updateMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.city != nil {
		_ = r.city.Close()
		r.city = nil
	}
	if r.asn != nil {
		_ = r.asn.Close()
		r.asn = nil
	}
	if r.info != nil {
		_ = r.info.Close()
		r.info = nil
	}
	return nil
}
