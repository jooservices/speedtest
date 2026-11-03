package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jooservices/speedtest/internal/agent/enroll"
)

type applyFixture struct {
	dir     string
	binary  string
	center  *httptest.Server
	release *httptest.Server
	body    []byte
	version string
}

func newApplyFixture(t *testing.T) *applyFixture {
	t.Helper()
	f := &applyFixture{dir: t.TempDir(), version: "2.0.0"}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if f.body, err = os.ReadFile(self); err != nil {
		t.Fatal(err)
	}
	f.binary = filepath.Join(f.dir, "speedtest-agent")
	if err := os.WriteFile(f.binary, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"state", "inbox"} {
		if err := os.MkdirAll(filepath.Join(f.dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.release = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(f.body) }))
	t.Cleanup(f.release.Close)
	sum := sha256.Sum256(f.body)
	f.center = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer node-secret" || r.URL.Path != "/api/agent/config" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"success":true,"code":200,"message":"ok","data":{"revision":"1","name":"n","expectedDown":1,"expectedUp":1,`+
			`"schedule":{"interval":30,"offset":0,"enabled":true},"quiet":{"quietStart":"","quietEnd":"","timezone":"UTC"},"autoUpdate":true,`+
			`"release":{"version":%q,"url":%q,"sha256":%q}},"meta":{},"errors":null}`, f.version, f.release.URL+"/agent", hex.EncodeToString(sum[:]))
	}))
	t.Cleanup(f.center.Close)
	credential := filepath.Join(f.dir, "credential")
	if err := enroll.StoreCredential(credential, "node-secret"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPEEDTEST_AGENT_BINARY", f.binary)
	t.Setenv("SPEEDTEST_AGENT_UPDATE_DIR", filepath.Join(f.dir, "state"))
	t.Setenv("SPEEDTEST_AGENT_INBOX_DIR", filepath.Join(f.dir, "inbox"))
	t.Setenv("SPEEDTEST_AGENT_CREDENTIAL", credential)
	t.Setenv("SPEEDTEST_AGENT_CENTER", f.center.URL)
	old := updateHTTPClient
	updateHTTPClient = f.release.Client()
	t.Cleanup(func() { updateHTTPClient = old })
	return f
}

func (f *applyFixture) request(t *testing.T, version string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, "inbox", "request.json"), []byte(`{"version":"`+version+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateApplyInstallsTheAdvertisedRelease(t *testing.T) {
	f := newApplyFixture(t)
	f.request(t, "2.0.0")
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"update-apply"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s %s", code, stdout.String(), stderr.String())
	}
	got, err := os.ReadFile(f.binary)
	if err != nil || !bytes.Equal(got, f.body) {
		t.Fatalf("the binary was not replaced: %v", err)
	}
	if prev, _ := os.ReadFile(f.binary + ".prev"); string(prev) != "old-binary" {
		t.Fatal("the previous binary must be kept")
	}
	if strings.Contains(stdout.String()+stderr.String(), "node-secret") {
		t.Fatal("the credential must never be logged")
	}
}

func TestUpdateApplyRejectsAReleaseThatIsNotTheRequestedOne(t *testing.T) {
	f := newApplyFixture(t)
	f.version = "3.0.0"
	f.request(t, "2.0.0")
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"update-apply"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if got, _ := os.ReadFile(f.binary); string(got) != "old-binary" {
		t.Fatal("a release the owner did not ask for must not be installed")
	}
}

func TestUpdateApplyWithoutRequestOrCenterDoesNothingAndNeverBlocksStart(t *testing.T) {
	f := newApplyFixture(t)
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"update-apply"}, &stdout, &stderr); code != 0 {
		t.Fatalf("no request: exit %d", code)
	}
	f.request(t, "2.0.0")
	t.Setenv("SPEEDTEST_AGENT_CENTER", "")
	if code := run(context.Background(), []string{"update-apply"}, &stdout, &stderr); code != 0 {
		t.Fatalf("no center: exit %d", code)
	}
	f.request(t, "2.0.0")
	t.Setenv("SPEEDTEST_AGENT_CENTER", f.center.URL)
	t.Setenv("SPEEDTEST_AGENT_CREDENTIAL", filepath.Join(f.dir, "missing"))
	if code := run(context.Background(), []string{"update-apply"}, &stdout, &stderr); code != 0 {
		t.Fatalf("no credential: exit %d", code)
	}
	t.Setenv("SPEEDTEST_AGENT_CREDENTIAL", filepath.Join(f.dir, "credential"))
	f.request(t, "2.0.0")
	f.center.Close()
	if code := run(context.Background(), []string{"update-apply"}, &stdout, &stderr); code != 0 {
		t.Fatalf("center down: exit %d", code)
	}
	if got, _ := os.ReadFile(f.binary); string(got) != "old-binary" {
		t.Fatal("nothing may be installed in these cases")
	}
	if code := run(context.Background(), []string{"update-apply", "extra"}, &stdout, &stderr); code != 2 {
		t.Fatalf("unexpected argument: exit %d", code)
	}
}

func TestUpdateApplyReportsUnwritableState(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	f := newApplyFixture(t)
	blocked := filepath.Join(f.dir, "blocked")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPEEDTEST_AGENT_UPDATE_DIR", filepath.Join(blocked, "state"))
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"update-apply"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
}

func TestUsageListsUpdateApply(t *testing.T) {
	var out bytes.Buffer
	printUsage(&out)
	if !strings.Contains(out.String(), "update-apply") {
		t.Fatal(out.String())
	}
}
