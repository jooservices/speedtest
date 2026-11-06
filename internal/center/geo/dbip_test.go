package geo

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func gz(t *testing.T, data []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// dbipServer serves the fixtures under the DB-IP file names. months lists the months that exist; mode changes the answer.
func dbipServer(t *testing.T, months []string, mode string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var count atomic.Int32
	city, asn := gz(t, fixture(t, "City")), gz(t, fixture(t, "ASN"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		var body []byte
		for _, m := range months {
			if r.URL.Path == "/free/dbip-city-lite-"+m+".mmdb.gz" {
				body = city
			}
			if r.URL.Path == "/free/dbip-asn-lite-"+m+".mmdb.gz" {
				body = asn
			}
		}
		switch {
		case body == nil:
			http.NotFound(w, r)
		case mode == "status":
			http.Error(w, "no", http.StatusInternalServerError)
		case mode == "bad-gzip":
			_, _ = w.Write([]byte("not gzip"))
		case mode == "bad-mmdb":
			_, _ = w.Write(gz(t, []byte("not an mmdb")))
		case mode == "empty":
			_, _ = w.Write(gz(t, nil))
		default:
			_, _ = w.Write(body)
		}
	}))
	t.Cleanup(server.Close)
	return server, &count
}

func fixedClock(s string) func() time.Time {
	tm, _ := time.Parse("2006-01-02", s)
	return func() time.Time { return tm }
}

func TestUpdateDBIPInstallsAndSkipsFreshFiles(t *testing.T) {
	r, logs := reader(t, false)
	server, count := dbipServer(t, []string{"2026-11"}, "ok")
	cfg := DBIPConfig{Enabled: true, URL: server.URL, Now: fixedClock("2026-11-10")}
	if err := r.UpdateDBIP(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Lookup(context.Background(), "81.2.69.160"); err != nil || got.City != "London" {
		t.Fatalf("Lookup() = %+v, %v", got, err)
	}
	if !strings.Contains(logs.String(), "DB-IP database updated") {
		t.Fatalf("log = %s", logs.String())
	}
	// The files were just written, so a second check (same clock as the file time is irrelevant: real mtime is now)
	// must not download again when the clock is close to the file time.
	before := count.Load()
	cfg.Now = time.Now
	if err := r.UpdateDBIP(context.Background(), cfg); err != nil || count.Load() != before {
		t.Fatalf("fresh files were downloaded again: %v (%d -> %d)", err, before, count.Load())
	}
	// An old file is replaced.
	old := time.Now().Add(-40 * 24 * time.Hour)
	for _, p := range []string{r.cfg.CityPath, r.cfg.ASNPath} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	cfg.Now = fixedClock(time.Now().UTC().Format("2006-01-02"))
	server2, count2 := dbipServer(t, []string{time.Now().UTC().Format("2006-01")}, "ok")
	cfg.URL = server2.URL
	if err := r.UpdateDBIP(context.Background(), cfg); err != nil || count2.Load() != 2 {
		t.Fatalf("old files were not refreshed: %v (%d requests)", err, count2.Load())
	}
}

func TestUpdateDBIPFallsBackToThePreviousMonth(t *testing.T) {
	r, _ := reader(t, false)
	server, _ := dbipServer(t, []string{"2026-10"}, "ok")
	if err := r.UpdateDBIP(context.Background(), DBIPConfig{Enabled: true, URL: server.URL, Now: fixedClock("2026-11-01")}); err != nil {
		t.Fatal(err)
	}
	if !r.loaded("City") || !r.loaded("ASN") {
		t.Fatal("databases were not loaded")
	}
}

func TestUpdateDBIPFailuresKeepTheOldFiles(t *testing.T) {
	for _, mode := range []string{"status", "bad-gzip", "bad-mmdb", "empty", "missing"} {
		t.Run(mode, func(t *testing.T) {
			r, logs := reader(t, true)
			old := time.Now().Add(-60 * 24 * time.Hour)
			for _, p := range []string{r.cfg.CityPath, r.cfg.ASNPath} {
				if err := os.Chtimes(p, old, old); err != nil {
					t.Fatal(err)
				}
			}
			months := []string{"2026-11", "2026-10"}
			if mode == "missing" {
				months = nil
			}
			server, _ := dbipServer(t, months, mode)
			err := r.UpdateDBIP(context.Background(), DBIPConfig{Enabled: true, URL: server.URL, Now: fixedClock("2026-11-10")})
			if err == nil {
				t.Fatal("expected an error")
			}
			if strings.Contains(err.Error(), server.URL) || strings.Contains(logs.String(), server.URL) {
				t.Fatal("the download URL was disclosed")
			}
			for kind, target := range map[string]string{"City": r.cfg.CityPath, "ASN": r.cfg.ASNPath} {
				data, readErr := os.ReadFile(target)
				if readErr != nil || !bytes.Equal(data, fixture(t, kind)) {
					t.Fatalf("old %s database lost", kind)
				}
			}
			if _, err := r.Lookup(context.Background(), "81.2.69.160"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUpdateDBIPRejectsUnsafeOriginsAndCancellation(t *testing.T) {
	r, _ := reader(t, false)
	for _, origin := range []string{"http://example.com", "ftp://x", "https://user:pw@example.com", "https://example.com?x=1", "::bad", "/relative"} {
		if err := r.UpdateDBIP(context.Background(), DBIPConfig{Enabled: true, URL: origin, Now: fixedClock("2026-11-10")}); err == nil {
			t.Errorf("origin %q was accepted", origin)
		}
	}
	server, _ := dbipServer(t, []string{"2026-11"}, "ok")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.UpdateDBIP(ctx, DBIPConfig{Enabled: true, URL: server.URL, Now: fixedClock("2026-11-10")}); err == nil {
		t.Fatal("a cancelled context must fail")
	}
}

func TestDBIPConfigFromEnvironment(t *testing.T) {
	cfg, err := DBIPConfigFromEnv()
	if err != nil || !cfg.Enabled || cfg.URL != "https://download.db-ip.com" {
		t.Fatalf("defaults = %+v, %v", cfg, err)
	}
	t.Setenv("SPEEDTEST_GEO_DBIP_AUTO_UPDATE", "false")
	t.Setenv("SPEEDTEST_GEO_DBIP_URL", "https://mirror.example")
	cfg, err = DBIPConfigFromEnv()
	if err != nil || cfg.Enabled || cfg.URL != "https://mirror.example" {
		t.Fatalf("overrides = %+v, %v", cfg, err)
	}
	t.Setenv("SPEEDTEST_GEO_DBIP_AUTO_UPDATE", "maybe")
	if _, err := DBIPConfigFromEnv(); err == nil {
		t.Fatal("expected a malformed boolean error")
	}
}

func TestRunDBIPSchedulerAndSwitches(t *testing.T) {
	if DBIPInterval != 24*time.Hour || DBIPAttribution == "" {
		t.Fatal("unexpected constants")
	}
	r, logs := reader(t, false)
	server, count := dbipServer(t, []string{"2026-11"}, "status")
	cfg := DBIPConfig{Enabled: true, URL: server.URL, Now: fixedClock("2026-11-10")}
	// Disabled and cancelled runs return immediately.
	r.RunDBIP(context.Background(), DBIPConfig{Enabled: false, URL: server.URL})
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	r.RunDBIP(cancelled, cfg)
	if count.Load() != 0 {
		t.Fatal("a disabled or cancelled run downloaded")
	}
	// A MaxMind licence key takes precedence.
	t.Setenv("SPEEDTEST_GEOLITE2_LICENSE_KEY", "key")
	r.RunDBIP(context.Background(), cfg)
	if count.Load() != 0 {
		t.Fatal("DB-IP ran although a MaxMind key is configured")
	}
	t.Setenv("SPEEDTEST_GEOLITE2_LICENSE_KEY", "")
	ctx, stop := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer stop()
	r.runDBIP(ctx, cfg, 10*time.Millisecond)
	if count.Load() < 4 || !strings.Contains(logs.String(), "DB-IP update failed") {
		t.Fatalf("scheduler did not repeat: %d requests, log=%s", count.Load(), logs.String())
	}
}
