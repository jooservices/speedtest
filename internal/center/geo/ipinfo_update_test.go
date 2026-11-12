package geo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oschwald/maxminddb-golang"
)

func TestIPInfoUpdateRejectsInvalidDatabaseWithoutLeakingToken(t *testing.T) {
	const token = "private-test-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Query().Get("token") != token {
			t.Errorf("IPinfo token was not sent to its origin")
		}
		_, _ = w.Write([]byte("not an MMDB"))
	}))
	defer server.Close()
	target := filepath.Join(t.TempDir(), "ipinfo.mmdb")
	var logs bytes.Buffer
	r := &Reader{cfg: Config{IPInfoPath: target, IPInfoURL: server.URL, IPInfoToken: token},
		log: slog.New(slog.NewTextHandler(&logs, nil))}
	err := r.UpdateIPInfo(context.Background())
	if err == nil || strings.Contains(err.Error(), token) || strings.Contains(logs.String(), token) {
		t.Fatalf("unsafe update result: err=%v log=%q", err, logs.String())
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid database was installed: %v", err)
	}
}

func TestIPInfoDownloadRejectsUnsafeOriginsAndFailedResponses(t *testing.T) {
	for _, origin := range []string{"://invalid", "http://example.test/database", "https://user:pass@example.test/database", "https://example.test/database#fragment"} {
		r := &Reader{cfg: Config{IPInfoURL: origin, IPInfoToken: "test-token"}}
		if _, err := r.downloadIPInfo(context.Background()); err == nil {
			t.Errorf("unsafe origin %q was accepted", origin)
		}
	}
	status := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	r := &Reader{cfg: Config{IPInfoURL: status.URL, IPInfoToken: "test-token"}}
	if _, err := r.downloadIPInfo(context.Background()); err == nil {
		t.Fatal("non-success HTTP status was accepted")
	}
	status.Close()
	badContextReader := &Reader{cfg: Config{IPInfoURL: "http://127.0.0.1/database"}}
	//nolint:staticcheck // Exercise net/http's nil-context rejection; production callers pass valid contexts.
	_, err := badContextReader.downloadIPInfo(nil)
	if err == nil || !strings.Contains(err.Error(), "cannot create") {
		t.Fatalf("nil request context error = %v", err)
	}
	if _, err := r.downloadIPInfo(context.Background()); err == nil {
		t.Fatal("transport failure was accepted")
	}
	truncated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = fmt.Fprint(rw, "HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\nx")
		_ = rw.Flush()
		_ = conn.Close()
	}))
	defer truncated.Close()
	r.cfg.IPInfoURL = truncated.URL
	if _, err := r.downloadIPInfo(context.Background()); err == nil {
		t.Fatal("truncated database response was accepted")
	}
}

func TestReaderLookupReturnsClosedDatabaseErrors(t *testing.T) {
	cityPath, asnPath, infoPath := writeGeoFixture(t, "City"), writeGeoFixture(t, "ASN"), writeGeoFixture(t, "IPinfo")
	cases := []struct {
		name   string
		config Config
		close  func(*Reader) error
		ip     string
		want   string
	}{
		{name: "city", config: Config{CityPath: cityPath, ASNPath: asnPath}, close: func(r *Reader) error { return r.city.Close() }, ip: "81.2.69.160", want: "city lookup failed"},
		{name: "ASN", config: Config{CityPath: cityPath, ASNPath: asnPath}, close: func(r *Reader) error { return r.asn.Close() }, ip: "1.0.0.1", want: "ASN lookup failed"},
		{name: "IPinfo", config: Config{CityPath: cityPath, ASNPath: asnPath, IPInfoPath: infoPath}, close: func(r *Reader) error { return r.info.Close() }, ip: "1.1.1.1", want: "IPinfo Lite database lookup failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Open(tc.config, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close() }()
			if err := tc.close(r); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Lookup(context.Background(), tc.ip); err == nil || err.Error() != tc.want {
				t.Fatalf("Lookup error = %v, want %q", err, tc.want)
			}
		})
	}
}

func writeGeoFixture(t *testing.T, kind string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), strings.ToLower(kind)+".mmdb")
	if err := os.WriteFile(path, fixture(t, kind), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseIPInfoLiteRecordRejectsEmptyAndInvalidRecords(t *testing.T) {
	for _, tc := range []struct{ country, asn, name string }{
		{}, {asn: "AS0"}, {asn: "not-an-asn"},
	} {
		if got, err := parseIPInfoLiteRecord(tc.country, tc.asn, tc.name); !errors.Is(err, ErrNotFound) || !emptyLocation(got) {
			t.Errorf("parseIPInfoLiteRecord(%q,%q,%q) = %+v, %v", tc.country, tc.asn, tc.name, got, err)
		}
	}
}

func TestIPInfoRedirectUsesHTTPSWithoutForwardingToken(t *testing.T) {
	origin, err := url.Parse("https://ipinfo.io/data/ipinfo_lite.mmdb")
	if err != nil {
		t.Fatal(err)
	}
	initial, _ := http.NewRequest(http.MethodGet, origin.String(), nil)
	redirect := ipInfoRedirect(origin)
	cdn, _ := http.NewRequest(http.MethodGet, "https://cdn.example.test/database?signature=temporary", nil)
	if err := redirect(cdn, []*http.Request{initial}); err != nil {
		t.Fatalf("signed HTTPS CDN redirect was refused: %v", err)
	}
	withToken, _ := http.NewRequest(http.MethodGet, "https://cdn.example.test/database?token=private", nil)
	if err := redirect(withToken, []*http.Request{initial}); err == nil {
		t.Fatal("redirect forwarded the IPinfo token to another host")
	}
}

func ipInfoDatabase(t *testing.T) []byte {
	t.Helper()
	return fixture(t, "IPinfo")
}

func TestIPInfoUpdateInstallsVerifiedDatabase(t *testing.T) {
	data := ipInfoDatabase(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Query().Get("token") != "test-token" {
			t.Error("IPinfo token was not sent to its configured origin")
		}
		_, _ = w.Write(data)
	}))
	defer server.Close()
	target := filepath.Join(t.TempDir(), "ipinfo.mmdb")
	r := &Reader{cfg: Config{IPInfoPath: target, IPInfoURL: server.URL, IPInfoToken: "test-token"}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := r.UpdateIPInfo(context.Background()); err != nil {
		t.Fatalf("UpdateIPInfo: %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, data) || r.info == nil {
		t.Fatalf("verified database was not atomically installed: bytes=%t err=%v reader=%t", bytes.Equal(got, data), err, r.info != nil)
	}
	loaded := loadIPInfo(target)
	if loaded == nil {
		t.Fatal("installed IPinfo Lite database did not pass the offline loader")
	}
	_ = loaded.Close()
	if err := r.UpdateIPInfo(context.Background()); err != nil {
		t.Fatalf("second update should replace and close the prior reader: %v", err)
	}
	_ = r.Close()
}

func TestReaderUsesVendorIPInfoLiteSchemaAndKeepsOfflineDataOnFailure(t *testing.T) {
	dir := t.TempDir()
	infoPath := filepath.Join(dir, "ipinfo.mmdb")
	if err := os.WriteFile(infoPath, ipInfoDatabase(t), 0600); err != nil {
		t.Fatal(err)
	}
	infoOnly, err := Open(Config{IPInfoPath: infoPath}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := infoOnly.Lookup(context.Background(), "1.1.1.1")
	if err != nil || got.Country != "AU" || got.ASN != 13335 || got.Organisation != "Cloudflare, Inc." ||
		got.FieldSources.Country != "ipinfo-lite-mmdb" || got.FieldSources.ASN != "ipinfo-lite-mmdb" {
		t.Fatalf("vendor IPinfo Lite record = %+v, %v", got, err)
	}
	_ = infoOnly.Close()

	cityPath, asnPath := filepath.Join(dir, "city.mmdb"), filepath.Join(dir, "asn.mmdb")
	if err := os.WriteFile(cityPath, fixture(t, "City"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(asnPath, fixture(t, "ASN"), 0600); err != nil {
		t.Fatal(err)
	}
	offline, err := Open(Config{CityPath: cityPath, ASNPath: asnPath, IPInfoPath: infoPath}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = offline.Close() }()
	if err := offline.info.Close(); err != nil {
		t.Fatal(err)
	}
	got, err = offline.Lookup(context.Background(), "1.0.0.1")
	if err != nil || got.ASN != 15169 || got.FieldSources.ASN != GeoLiteSource {
		t.Fatalf("offline result was lost after supplemental lookup failure: %+v, %v", got, err)
	}
}

func TestLoadIPInfoRequiresVerifiedLiteDatabase(t *testing.T) {
	if loadIPInfo("") != nil || loadIPInfo(filepath.Join(t.TempDir(), "missing.mmdb")) != nil {
		t.Fatal("empty or missing IPinfo database path was accepted")
	}
	wrongEdition := filepath.Join(t.TempDir(), "city.mmdb")
	if err := os.WriteFile(wrongEdition, fixture(t, "City"), 0600); err != nil {
		t.Fatal(err)
	}
	if db := loadIPInfo(wrongEdition); db != nil {
		_ = db.Close()
		t.Fatal("GeoLite City database was accepted as IPinfo Lite")
	}
	path := filepath.Join(t.TempDir(), "ipinfo.mmdb")
	if err := os.WriteFile(path, ipInfoDatabase(t), 0600); err != nil {
		t.Fatal(err)
	}
	db := loadIPInfo(path)
	if db == nil {
		t.Fatal("verified IPinfo Lite database was rejected")
	}
	_ = db.Close()
}

func TestDecodeIPInfoRejectsInvalidAndWrongEditionDatabases(t *testing.T) {
	if db, err := decodeIPInfo([]byte("not an MMDB")); err == nil || db != nil {
		t.Fatalf("invalid MMDB decode = (%v,%v)", db, err)
	}
	if db, err := decodeIPInfo(fixture(t, "City")); err == nil || db != nil {
		t.Fatalf("GeoLite City decode = (%v,%v)", db, err)
	}
	db, err := decodeIPInfo(ipInfoDatabase(t))
	if err != nil || db == nil {
		t.Fatalf("vendor IPinfo Lite decode = (%v,%v)", db, err)
	}
	_ = db.Close()
}

func TestIPInfoInstallFailuresAndNoopScheduling(t *testing.T) {
	data := fixture(t, "City")
	db, err := maxminddb.FromBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &Reader{cfg: Config{IPInfoPath: filepath.Join(t.TempDir(), "cancelled.mmdb")}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := r.installIPInfo(ctx, data, db); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled installation error = %v", err)
	}
	if err := r.UpdateIPInfo(context.Background()); err != nil {
		t.Fatalf("missing token should skip online update: %v", err)
	}
	r.RunIPInfo(ctx)

	db, err = maxminddb.FromBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	r.cfg.IPInfoPath = t.TempDir()
	if err := r.installIPInfo(context.Background(), data, db); err == nil {
		t.Fatal("failed atomic replacement was accepted")
	}
}

func TestIPInfoSchedulerStopsAfterInitialFailedUpdateIsCanceled(t *testing.T) {
	var requests atomic.Int32
	var cancel context.CancelFunc
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		cancel()
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	ctx, stop := context.WithCancel(context.Background())
	cancel = stop
	r := &Reader{cfg: Config{IPInfoPath: filepath.Join(t.TempDir(), "ipinfo.mmdb"), IPInfoURL: server.URL, IPInfoToken: "test-token"}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r.RunIPInfo(ctx)
	if requests.Load() != 1 {
		t.Fatalf("initial provider checks = %d, want 1", requests.Load())
	}
}

func TestIPInfoSchedulerRunsDailyUpdateAndKeepsGoingAfterFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time, 2)
	ticks <- time.Now()
	ticks <- time.Now().Add(ipInfoUpdateInterval)
	var logs strings.Builder
	var calls int
	update := func(context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("sanitized update failure")
		}
		cancel()
		return nil
	}
	runIPInfoUpdates(ctx, ticks, update, slog.New(slog.NewTextHandler(&logs, nil)))
	if calls != 3 || strings.Count(logs.String(), "IPinfo Lite database update failed") != 2 || strings.Contains(logs.String(), "private") {
		t.Fatalf("scheduler calls=%d logs=%q", calls, logs.String())
	}
}

func TestIPInfoSchedulerDefaultsLoggerAndStopsWhenInitialFailureCancels(t *testing.T) {
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(previous)

	ctx, cancel := context.WithCancel(context.Background())
	update := func(context.Context) error {
		cancel()
		return errors.New("sanitized update failure")
	}
	runIPInfoUpdates(ctx, nil, update, nil)
}

func TestIPInfoRedirectRejectsUnsafeRequests(t *testing.T) {
	origin, _ := url.Parse("https://ipinfo.io/data/ipinfo_lite.mmdb")
	redirect := ipInfoRedirect(origin)
	for name, request := range map[string]*http.Request{
		"insecure scheme": {URL: &url.URL{Scheme: "http", Host: "cdn.example.test"}},
		"empty chain":     {URL: &url.URL{Scheme: "https", Host: "cdn.example.test"}},
		"too many hops":   {URL: &url.URL{Scheme: "https", Host: "cdn.example.test"}},
	} {
		via := []*http.Request{{URL: origin}}
		if name == "empty chain" {
			via = nil
		}
		if name == "too many hops" {
			via = []*http.Request{{URL: origin}, {URL: origin}, {URL: origin}}
		}
		if err := redirect(request, via); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	request, _ := http.NewRequest(http.MethodGet, "https://ipinfo.io/data?token=private", nil)
	request.Header.Set("Authorization", "Basic secret")
	if err := redirect(request, []*http.Request{{URL: origin}}); err != nil || request.Header.Get("Authorization") != "" {
		t.Fatalf("same-host redirect should be accepted and strip authorization: %v", err)
	}
}
