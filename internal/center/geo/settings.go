// Package geo provides offline City/ASN lookups and verified GeoLite2 updates.
package geo

import (
	"context"
	"errors"
	"os"
	"strconv"
)

// UpdaterSettings is implemented by environment configuration today and stored
// Settings later. Implementations must never log the returned licence key.
type UpdaterSettings interface {
	LicenseKey(context.Context) (string, error)
	AutoUpdate(context.Context) (bool, error)
}

// EnvironmentSettings reads current environment values on every update.
type EnvironmentSettings struct{}

func (EnvironmentSettings) LicenseKey(context.Context) (string, error) {
	return os.Getenv("SPEEDTEST_GEOLITE2_LICENSE_KEY"), nil
}

func (EnvironmentSettings) AutoUpdate(context.Context) (bool, error) {
	raw := os.Getenv("SPEEDTEST_GEOLITE2_AUTO_UPDATE")
	if raw == "" {
		return true, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, errors.New("SPEEDTEST_GEOLITE2_AUTO_UPDATE must be a boolean")
	}
	return value, nil
}

// Config contains paths and the download service origin. No IP address is sent
// to that service. Paths must be distinct and writable by the center process.
type Config struct {
	CityPath    string
	ASNPath     string
	DownloadURL string
	// AccountID selects the current Basic Auth download API. If absent, the
	// backwards-compatible licence-key download API is used.
	AccountID string
}

// ConfigFromEnv supplies defaults without reading or exposing any secret.
func ConfigFromEnv() Config {
	return Config{
		CityPath:    envDefault("SPEEDTEST_GEOIP_CITY_DB", "data/geoip/GeoLite2-City.mmdb"),
		ASNPath:     envDefault("SPEEDTEST_GEOIP_ASN_DB", "data/geoip/GeoLite2-ASN.mmdb"),
		DownloadURL: envDefault("SPEEDTEST_GEOLITE2_DOWNLOAD_URL", "https://download.maxmind.com"),
		AccountID:   os.Getenv("SPEEDTEST_GEOLITE2_ACCOUNT_ID"),
	}
}

func envDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
