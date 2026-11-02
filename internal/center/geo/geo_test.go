package geo

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, kind string) []byte {
	t.Helper()
	name := "GeoIP2-City-Test.mmdb"
	if kind == "ASN" {
		name = "GeoLite2-ASN-Test.mmdb"
	}
	data, err := os.ReadFile(filepath.Join("../../../.tmp", name))
	if err != nil {
		t.Fatalf("run python3 scripts/geo-test-fixtures.py first: %v", err)
	}
	return data
}

func reader(t *testing.T, populated bool) (*Reader, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{CityPath: filepath.Join(dir, "city.mmdb"), ASNPath: filepath.Join(dir, "asn.mmdb")}
	if populated {
		for kind, target := range map[string]string{"City": cfg.CityPath, "ASN": cfg.ASNPath} {
			if err := os.WriteFile(target, fixture(t, kind), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	var logs bytes.Buffer
	r, err := Open(cfg, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r, &logs
}

func TestLookup(t *testing.T) {
	r, _ := reader(t, true)
	var contract Geo = r
	city, err := contract.Lookup(context.Background(), "81.2.69.160")
	if err != nil {
		t.Fatal(err)
	}
	if city.City != "London" || city.Region != "England" || city.Country != "GB" || city.Latitude != 51.5142 || city.Longitude != -0.0931 || city.TimeZone != "Europe/London" || city.AccuracyRadius != 100 {
		t.Fatalf("unexpected city: %+v", city)
	}
	asn, err := r.Lookup(context.Background(), "1.0.0.1")
	if err != nil || asn.ASN != 15169 || asn.Organisation != "Google Inc." {
		t.Fatalf("unexpected ASN: %+v %v", asn, err)
	}
	ipv6, err := r.Lookup(context.Background(), "2001:218::")
	if err != nil || ipv6.Country != "JP" {
		t.Fatalf("unexpected IPv6: %+v %v", ipv6, err)
	}
	for _, ip := range []string{"192.0.2.1", "::1"} {
		if _, err := r.Lookup(context.Background(), ip); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: %v", ip, err)
		}
	}
	if _, err := r.Lookup(context.Background(), "bad"); !errors.Is(err, ErrInvalidIP) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Lookup(ctx, "1.0.0.1"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestMissingAndClose(t *testing.T) {
	r, logs := reader(t, false)
	for range 5 {
		if _, err := r.Lookup(context.Background(), "1.0.0.1"); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
	}
	if strings.Count(logs.String(), "database unavailable") != 1 {
		t.Fatal(logs.String())
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []Config{{}, {CityPath: "a", ASNPath: "a"}, {CityPath: "x", ASNPath: "./x"}} {
		if _, err := Open(cfg, nil); err == nil {
			t.Fatal("accepted invalid paths")
		}
	}
	cfg := Config{CityPath: filepath.Join(t.TempDir(), "city"), ASNPath: filepath.Join(t.TempDir(), "asn")}
	if err := os.WriteFile(cfg.CityPath, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	degraded, err := Open(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = degraded.Close()
	if _, err := decode(fixture(t, "ASN"), "City"); err == nil {
		t.Fatal("accepted wrong edition")
	}
}

func TestEnvironment(t *testing.T) {
	for _, key := range []string{"SPEEDTEST_GEOLITE2_LICENSE_KEY", "SPEEDTEST_GEOLITE2_AUTO_UPDATE", "SPEEDTEST_GEOIP_CITY_DB", "SPEEDTEST_GEOIP_ASN_DB", "SPEEDTEST_GEOLITE2_DOWNLOAD_URL", "SPEEDTEST_GEOLITE2_ACCOUNT_ID"} {
		t.Setenv(key, "")
	}
	settings := EnvironmentSettings{}
	if key, err := settings.LicenseKey(context.Background()); err != nil || key != "" {
		t.Fatal("default key")
	}
	if enabled, err := settings.AutoUpdate(context.Background()); err != nil || !enabled {
		t.Fatal("default enabled")
	}
	cfg := ConfigFromEnv()
	if cfg.CityPath != "data/geoip/GeoLite2-City.mmdb" || cfg.ASNPath != "data/geoip/GeoLite2-ASN.mmdb" || cfg.DownloadURL != "https://download.maxmind.com" {
		t.Fatalf("%+v", cfg)
	}
	t.Setenv("SPEEDTEST_GEOIP_CITY_DB", "custom-city")
	t.Setenv("SPEEDTEST_GEOIP_ASN_DB", "custom-asn")
	t.Setenv("SPEEDTEST_GEOLITE2_DOWNLOAD_URL", "https://example.test")
	t.Setenv("SPEEDTEST_GEOLITE2_ACCOUNT_ID", "123")
	cfg = ConfigFromEnv()
	if cfg.CityPath != "custom-city" || cfg.ASNPath != "custom-asn" || cfg.DownloadURL != "https://example.test" || cfg.AccountID != "123" {
		t.Fatal(cfg)
	}
	t.Setenv("SPEEDTEST_GEOLITE2_LICENSE_KEY", "test-key")
	if key, _ := settings.LicenseKey(context.Background()); key != "test-key" {
		t.Fatal("key not reloaded")
	}
	t.Setenv("SPEEDTEST_GEOLITE2_AUTO_UPDATE", "false")
	if enabled, err := settings.AutoUpdate(context.Background()); enabled || err != nil {
		t.Fatal("disabled")
	}
	t.Setenv("SPEEDTEST_GEOLITE2_AUTO_UPDATE", "secret-invalid")
	if _, err := settings.AutoUpdate(context.Background()); err == nil || strings.Contains(err.Error(), "secret-invalid") {
		t.Fatal("invalid boolean")
	}
}
