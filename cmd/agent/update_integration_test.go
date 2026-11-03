//go:build integration

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/agent/enroll"
)

func buildAgent(t *testing.T, out, version string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-ldflags", "-X main.version="+version, "-o", out, ".")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", version, err, output)
	}
}

func TestSelfUpdateWithRealBinariesInstallsConfirmsAndRollsBack(t *testing.T) {
	dir := t.TempDir()
	installed := filepath.Join(dir, "bin", "speedtest-agent")
	next := filepath.Join(dir, "release", "speedtest-agent-2.0.0")
	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(next), 0o755); err != nil {
		t.Fatal(err)
	}
	buildAgent(t, installed, "1.0.0")
	buildAgent(t, next, "2.0.0")
	body, err := os.ReadFile(next)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)

	release := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) }))
	defer release.Close()
	certFile := filepath.Join(dir, "release-ca.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: release.Certificate().Raw}), 0o644); err != nil {
		t.Fatal(err)
	}
	center := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"success":true,"code":200,"message":"ok","data":{"revision":"1","name":"n","expectedDown":1,"expectedUp":1,`+
			`"schedule":{"interval":30,"offset":0,"enabled":true},"quiet":{"quietStart":"","quietEnd":"","timezone":"UTC"},"autoUpdate":true,`+
			`"release":{"version":"2.0.0","url":%q,"sha256":%q}},"meta":{},"errors":null}`, release.URL+"/agent", hex.EncodeToString(sum[:]))
	}))
	defer center.Close()
	credential := filepath.Join(dir, "credential")
	if err := enroll.StoreCredential(credential, "node-secret"); err != nil {
		t.Fatal(err)
	}
	stateDir, inbox := filepath.Join(dir, "state"), filepath.Join(dir, "inbox")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "SPEEDTEST_AGENT_BINARY="+installed, "SPEEDTEST_AGENT_UPDATE_DIR="+stateDir, "SPEEDTEST_AGENT_INBOX_DIR="+inbox,
		"SPEEDTEST_AGENT_CREDENTIAL="+credential, "SPEEDTEST_AGENT_CENTER="+center.URL, "SSL_CERT_FILE="+certFile)
	runInstalled := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(installed, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	version := func() string { return runInstalled("version") }
	request := func() {
		t.Helper()
		if err := os.WriteFile(filepath.Join(inbox, "request.json"), []byte(`{"version":"2.0.0"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pastDeadline := func() {
		t.Helper()
		path := filepath.Join(stateDir, "state.json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var state map[string]any
		if err := json.Unmarshal(data, &state); err != nil {
			t.Fatal(err)
		}
		state["deadline"] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
		data, _ = json.Marshal(state)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if got := version(); got != "speedtest-agent 1.0.0" {
		t.Fatalf("starting version = %q", got)
	}

	// Update 1: installed, never confirmed, so the next start after the window restores 1.0.0.
	request()
	runInstalled("update-apply")
	if got := version(); got != "speedtest-agent 2.0.0" {
		t.Fatalf("after install = %q", got)
	}
	runInstalled("update-apply") // a restart inside the window keeps the new version
	if got := version(); got != "speedtest-agent 2.0.0" {
		t.Fatalf("inside the window = %q", got)
	}
	pastDeadline()
	runInstalled("update-apply")
	if got := version(); got != "speedtest-agent 1.0.0" {
		t.Fatalf("after the window without a heartbeat the old binary must be back, got %q", got)
	}
	var result map[string]any
	data, _ := os.ReadFile(filepath.Join(stateDir, "result.json"))
	if err := json.Unmarshal(data, &result); err != nil || result["status"] != "rolled_back" || result["version"] != "2.0.0" {
		t.Fatalf("result = %s", data)
	}

	// Rolled back versions are not installed again by a stale request.
	request()
	runInstalled("update-apply")
	if got := version(); got != "speedtest-agent 1.0.0" {
		t.Fatalf("a rolled back version was installed again: %q", got)
	}
	_ = os.Remove(filepath.Join(stateDir, "state.json"))

	// Update 2: confirmed by the new version, so it stays and the previous binary is removed.
	request()
	runInstalled("update-apply")
	if got := version(); got != "speedtest-agent 2.0.0" {
		t.Fatalf("second install = %q", got)
	}
	if err := os.WriteFile(filepath.Join(inbox, "confirmed"), []byte("2.0.0"), 0o600); err != nil {
		t.Fatal(err)
	}
	runInstalled("update-apply")
	if _, err := os.Stat(installed + ".prev"); !os.IsNotExist(err) {
		t.Fatal("the previous binary is removed after confirmation")
	}
	pastDeadline()
	runInstalled("update-apply")
	if got := version(); got != "speedtest-agent 2.0.0" {
		t.Fatalf("a confirmed update must stay: %q", got)
	}
	data, _ = os.ReadFile(filepath.Join(stateDir, "result.json"))
	if err := json.Unmarshal(data, &result); err != nil || result["status"] != "succeeded" {
		t.Fatalf("result = %s", data)
	}
}
