package geo

import (
	"bytes"
	"context"
	"errors"
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
	data := fixture(t, "City")
	const source, replacement = "GeoIP2-City", "IPinfo Lite"
	if len(source) != len(replacement) || !bytes.Contains(data, []byte(source)) {
		t.Fatal("unexpected MaxMind fixture metadata")
	}
	return bytes.Replace(data, []byte(source), []byte(replacement), 1)
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
