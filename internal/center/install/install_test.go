package install

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestHandler(t *testing.T) (http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{
		"speedtest-agent-linux-amd64": "amd64-bytes", "speedtest-agent-linux-arm64": "arm64-bytes",
		"SHA256SUMS": "sums", "speedtest-agent.service": "[Unit]", "secret.txt": "do not serve",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h, err := Handler(Config{PublicURL: "https://speedtest.example.com/", ReleaseDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	return h, dir
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestInstallScriptUsesConfiguredURL(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := get(h, "/install.sh")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `CENTER_URL="https://speedtest.example.com"`) || strings.Contains(body, "__CENTER_URL__") {
		t.Fatalf("script does not carry the configured URL:\n%s", body[:200])
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/x-shellscript") {
		t.Fatalf("content type = %q", got)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("script must not be cached")
	}
}

func TestInstallScriptIgnoresHostHeader(t *testing.T) {
	h, _ := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/install.sh", nil)
	req.Host = "evil.example.net"
	req.Header.Set("X-Forwarded-Host", "evil.example.net")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "evil.example.net") {
		t.Fatal("the script must only use the configured public URL")
	}
}

func TestDownloadWhitelist(t *testing.T) {
	h, _ := newTestHandler(t)
	for _, name := range []string{"speedtest-agent-linux-amd64", "speedtest-agent-linux-arm64", "SHA256SUMS", "speedtest-agent.service"} {
		rec := get(h, "/download/"+name)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d", name, rec.Code)
		}
		if _, err := io.ReadAll(rec.Body); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"/download/secret.txt", "/download/..%2Fsecret.txt", "/download/speedtest-agent-linux-riscv64", "/download/", "/download/speedtest-agent-linux-amd64/extra"} {
		if rec := get(h, path); rec.Code != http.StatusNotFound && rec.Code != http.StatusMovedPermanently {
			t.Fatalf("%s status = %d, want 404", path, rec.Code)
		}
	}
}

func TestMissingArtifactAndWrongMethod(t *testing.T) {
	h, dir := newTestHandler(t)
	if err := os.Remove(filepath.Join(dir, "SHA256SUMS")); err != nil {
		t.Fatal(err)
	}
	if rec := get(h, "/download/SHA256SUMS"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing file status = %d", rec.Code)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/install.sh", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d", rec.Code)
	}
}

func TestHandlerRejectsUnsafeConfiguration(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{"", "ftp://x.example.com", "https://user:pw@x.example.com", "https://x.example.com/?q=1", "https://x.example.com/a b", "https://x.example.com\"; rm -rf /; \"", "https://"} {
		if _, err := Handler(Config{PublicURL: bad, ReleaseDir: dir}); err == nil {
			t.Fatalf("Handler accepted public URL %q", bad)
		}
	}
	if _, err := Handler(Config{PublicURL: "https://x.example.com", ReleaseDir: filepath.Join(dir, "missing")}); err == nil {
		t.Fatal("Handler accepted a missing release directory")
	}
	if _, err := Handler(Config{PublicURL: "http://127.0.0.1:8080", ReleaseDir: dir}); err != nil {
		t.Fatalf("loopback URL rejected: %v", err)
	}
}
