package geo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testSettings struct {
	enabled         bool
	key             string
	autoErr, keyErr error
}

func (s testSettings) AutoUpdate(context.Context) (bool, error)   { return s.enabled, s.autoErr }
func (s testSettings) LicenseKey(context.Context) (string, error) { return s.key, s.keyErr }

var enabledSettings = testSettings{enabled: true, key: "test-secret"}

func archiveFor(t *testing.T, filename string, data []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "release/" + filename, Mode: 0600, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func serveUpdates(t *testing.T, r *Reader, mode string) (*atomic.Int64, func()) {
	t.Helper()
	archives := map[string][]byte{}
	for _, kind := range []string{"City", "ASN"} {
		data := fixture(t, kind)
		if mode == "wrong-edition" {
			data = fixture(t, "ASN")
		}
		if mode == "invalid-mmdb" {
			data = []byte("corrupt")
		}
		name := "GeoLite2-" + kind + ".mmdb"
		if mode == "missing-entry" {
			name = "LICENSE.txt"
		}
		archives["GeoLite2-"+kind] = archiveFor(t, name, data)
	}
	var count atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		count.Add(1)
		edition := req.URL.Query().Get("edition_id")
		if r.cfg.AccountID != "" {
			user, key, ok := req.BasicAuth()
			if !ok || user != "123" || key != enabledSettings.key {
				t.Error("incorrect Basic Auth")
			}
			edition = strings.Split(req.URL.Path, "/")[3]
		} else if req.URL.Query().Get("license_key") != enabledSettings.key {
			t.Error("missing licence")
		}
		if mode == "status" {
			http.Error(w, "test-secret", http.StatusUnauthorized)
			return
		}
		if mode == "redirect" {
			http.Redirect(w, req, "http://example.test/", http.StatusFound)
			return
		}
		data := archives[edition]
		if req.URL.Query().Get("suffix") == "tar.gz.sha256" {
			checksum := fmt.Sprintf("%x  release.tar.gz\n", sha256.Sum256(data))
			if mode == "checksum" {
				checksum = "bad"
			}
			if mode == "checksum-empty" {
				checksum = ""
			}
			if mode == "checksum-status" {
				w.WriteHeader(500)
				return
			}
			_, _ = io.WriteString(w, checksum)
			return
		}
		if mode == "invalid-gzip" {
			data = []byte("bad gzip")
		}
		_, _ = w.Write(data)
	}))
	r.cfg.DownloadURL = server.URL
	return &count, server.Close
}

func TestUpdateSuccessAndConcurrentLookup(t *testing.T) {
	for _, account := range []string{"", "123"} {
		t.Run("account="+account, func(t *testing.T) {
			r, _ := reader(t, true)
			r.cfg.AccountID = account
			count, closeServer := serveUpdates(t, r, "ok")
			defer closeServer()
			var wg sync.WaitGroup
			for range 4 {
				wg.Go(func() {
					for range 50 {
						if _, err := r.Lookup(context.Background(), "81.2.69.160"); err != nil {
							t.Error(err)
						}
					}
				})
			}
			if err := r.Update(context.Background(), enabledSettings); err != nil {
				t.Fatal(err)
			}
			wg.Wait()
			if count.Load() != 4 {
				t.Fatal(count.Load())
			}
			for kind, target := range map[string]string{"City": r.cfg.CityPath, "ASN": r.cfg.ASNPath} {
				data, err := os.ReadFile(target)
				if err != nil || !bytes.Equal(data, fixture(t, kind)) {
					t.Fatalf("%s not installed: %v", kind, err)
				}
				info, err := os.Stat(target)
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatal("database permissions")
				}
				matches, _ := filepath.Glob(filepath.Join(filepath.Dir(target), ".geolite2-*"))
				if len(matches) != 0 {
					t.Fatal("temporary file leaked")
				}
			}
		})
	}
}

func TestUpdateFailuresPreserveOldFiles(t *testing.T) {
	for _, mode := range []string{"status", "redirect", "checksum", "checksum-empty", "checksum-status", "invalid-gzip", "invalid-mmdb", "wrong-edition", "missing-entry"} {
		t.Run(mode, func(t *testing.T) {
			r, logs := reader(t, true)
			_, closeServer := serveUpdates(t, r, mode)
			defer closeServer()
			err := r.Update(context.Background(), enabledSettings)
			if err == nil {
				t.Fatal("expected update error")
			}
			if strings.Contains(err.Error(), enabledSettings.key) || strings.Contains(logs.String(), enabledSettings.key) {
				t.Fatal("secret disclosed")
			}
			// A successful ASN refresh in the wrong-edition case is independent of City.
			for kind, target := range map[string]string{"City": r.cfg.CityPath, "ASN": r.cfg.ASNPath} {
				data, readErr := os.ReadFile(target)
				if readErr != nil || !bytes.Equal(data, fixture(t, kind)) {
					t.Fatal("old database lost")
				}
			}
			if _, err := r.Lookup(context.Background(), "81.2.69.160"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSettingsAndRecovery(t *testing.T) {
	r, logs := reader(t, false)
	count, closeServer := serveUpdates(t, r, "ok")
	defer closeServer()
	for _, s := range []testSettings{{}, {enabled: true}, {enabled: true}} {
		if err := r.Update(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	if count.Load() != 0 || strings.Count(logs.String(), "licence key missing") != 1 {
		t.Fatal(logs.String())
	}
	for _, s := range []testSettings{{autoErr: errors.New("test-secret")}, {enabled: true, keyErr: errors.New("test-secret")}} {
		err := r.Update(context.Background(), s)
		if err == nil || strings.Contains(err.Error(), "test-secret") {
			t.Fatal("unsafe settings error")
		}
	}
	if err := r.Update(context.Background(), enabledSettings); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Lookup(context.Background(), "81.2.69.160"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Update(ctx, enabledSettings); err == nil {
		t.Fatal("accepted canceled request")
	}
}

func TestDownloadValidationAndBounds(t *testing.T) {
	r, _ := reader(t, false)
	client := &http.Client{Timeout: time.Second}
	for _, origin := range []string{"%", "https:///", "http://example.test", "https://user:pass@example.test", "https://example.test?secret=x", "https://example.test#x"} {
		r.cfg.DownloadURL = origin
		if _, err := r.download(context.Background(), client, "secret", "City", "tar.gz", 10); err == nil {
			t.Fatalf("accepted %s", origin)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("123456")) }))
	defer server.Close()
	r.cfg.DownloadURL = server.URL
	if _, err := r.download(context.Background(), client, "secret", "City", "tar.gz", 5); err == nil {
		t.Fatal("size limit ignored")
	}
	for _, limit := range []int64{6, 7} {
		if data, err := r.download(context.Background(), client, "secret", "City", "tar.gz", limit); err != nil || string(data) != "123456" {
			t.Fatal("size boundary", err)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, "https://storage.example.test", nil)
	req.SetBasicAuth("user", "secret")
	if err := safeRedirect(req, nil); err != nil || req.Header.Get("Authorization") != "" {
		t.Fatal("redirect auth leaked")
	}
	if err := safeRedirect(req, make([]*http.Request, 10)); err == nil {
		t.Fatal("redirect limit")
	}
}

func TestExtractAndReplacement(t *testing.T) {
	for _, data := range [][]byte{nil, []byte("bad"), archiveFor(t, "other", []byte("x")), archiveFor(t, "GeoLite2-City.mmdb", nil)} {
		if _, err := extract(data, "GeoLite2-City.mmdb"); err == nil {
			t.Fatal("accepted invalid archive")
		}
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte("not tar"))
	_ = gz.Close()
	if _, err := extract(buf.Bytes(), "x"); err == nil {
		t.Fatal("accepted invalid tar")
	}
	data := archiveFor(t, "GeoLite2-City.mmdb", []byte("x"))
	data[len(data)-5] ^= 0xff
	if _, err := extract(data, "GeoLite2-City.mmdb"); err == nil {
		t.Fatal("accepted corrupt gzip trailer")
	}
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := replace(filepath.Join(blocker, "nested"), []byte("new")); err == nil {
		t.Fatal("invalid directory")
	}
	if err := replace(dir, []byte("new")); err == nil {
		t.Fatal("replaced directory")
	}
	nested := filepath.Join(dir, "nested", "new")
	if err := replace(nested, []byte("new")); err != nil {
		t.Fatal(err)
	}
}

func TestScheduler(t *testing.T) {
	if UpdateInterval != 168*time.Hour {
		t.Fatal("not weekly")
	}
	r, logs := reader(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Run(ctx, enabledSettings)
	count, closeServer := serveUpdates(t, r, "status")
	defer closeServer()
	ctx, cancel = context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	r.run(ctx, enabledSettings, 10*time.Millisecond)
	if count.Load() < 4 {
		t.Fatal("scheduler did not repeat")
	}
	if !strings.Contains(logs.String(), "update failed") || strings.Contains(logs.String(), enabledSettings.key) {
		t.Fatal("unsafe or missing scheduler logging")
	}
}
