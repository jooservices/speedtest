package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func enrollServer(t *testing.T, nodeID, credential string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"success":true,"code":201,"message":"enrolled","data":{"nodeId":"` + nodeID + `","credential":"` + credential + `","config":{}},"meta":{},"errors":null}`))
	}))
}

func writeCredential(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write credential: %v", err)
	}
}

func writeConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func TestRunNoArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), nil, &stdout, &stderr); code != 2 {
		t.Fatalf("run() = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Fatalf("stderr = %q, want usage", stderr.String())
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"bogus"}, &stdout, &stderr); code != 2 {
		t.Fatalf("run() = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), `unknown command "bogus"`) {
		t.Fatalf("stderr = %q, want unknown command", stderr.String())
	}
}

func TestRunVersion(t *testing.T) {
	version = "9.8.7"
	t.Cleanup(func() { version = "dev" })

	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run() = %d, want 0", code)
	}
	if got := strings.TrimSpace(stdout.String()); got != "speedtest-agent 9.8.7" {
		t.Fatalf("stdout = %q, want speedtest-agent 9.8.7", got)
	}
}

func TestRunEnrollSuccess(t *testing.T) {
	version = "1.2.3"
	t.Cleanup(func() { version = "dev" })

	credential := "super-secret-credential"
	s := enrollServer(t, "node-1", credential)
	defer s.Close()

	credPath := filepath.Join(t.TempDir(), "credential")
	t.Setenv("SPEEDTEST_AGENT_CREDENTIAL", credPath)

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"enroll",
		"--center", s.URL,
		"--token", "one-time-token",
		"--name", "viettel-fast-50",
	}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("run() = %d, stderr = %q, want 0", code, stderr.String())
	}
	out := stdout.String() + stderr.String()
	if strings.Contains(out, credential) {
		t.Fatalf("output = %q, must never log the credential", out)
	}
	if strings.Contains(out, "one-time-token") {
		t.Fatalf("output = %q, must never log the token", out)
	}
	if !strings.Contains(stdout.String(), `enrolled node "viettel-fast-50" (node-1)`) {
		t.Fatalf("stdout = %q, want enrolled message", stdout.String())
	}

	data, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatalf("read credential file: %v", err)
	}
	if string(data) != credential {
		t.Fatalf("credential file = %q, want %q", string(data), credential)
	}
	info, err := os.Stat(credPath)
	if err != nil {
		t.Fatalf("stat credential: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("credential mode = %04o, want 0600", mode)
	}
}

func TestRunEnrollReadsTokenFromStandardInput(t *testing.T) {
	version = "1.2.3"
	t.Cleanup(func() { version = "dev" })
	const token = "one-time-token-from-stdin"
	const credential = "stdin-enrolled-credential"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode enroll body: %v", err)
		}
		if body.Token != token {
			t.Errorf("enroll token = %q, want stdin token", body.Token)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"success":true,"code":201,"message":"enrolled","data":{"nodeId":"node-stdin","credential":"` + credential + `","config":{}},"meta":{},"errors":null}`))
	}))
	defer server.Close()

	credentialPath := filepath.Join(t.TempDir(), "credential")
	t.Setenv("SPEEDTEST_AGENT_CREDENTIAL", credentialPath)
	var stdout, stderr bytes.Buffer
	code := runEnrollWithInput(context.Background(), []string{
		"--center", server.URL, "--token-stdin", "--name", "stdin-node",
	}, strings.NewReader(token+"\n"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runEnrollWithInput() = %d, stderr=%q", code, stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), token) {
		t.Fatal("enrollment token was written to command output")
	}
	stored, err := os.ReadFile(credentialPath)
	if err != nil || string(stored) != credential {
		t.Fatalf("stored credential = %q, err=%v", stored, err)
	}
}

func TestRunEnrollBadCenter(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"enroll", "--center", "not-a-url", "--token", "t", "--name", "n",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("run() = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--center must be an absolute http or https URL") {
		t.Fatalf("stderr = %q, want center error", stderr.String())
	}
}

func TestRunEnrollServerError(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"invalid token"}`))
	}))
	defer s.Close()

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"enroll", "--center", s.URL, "--token", "t", "--name", "n",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "invalid token") {
		t.Fatalf("stderr = %q, want the server error", stderr.String())
	}
}

func TestRunEnrollUnexpectedArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"enroll", "extra"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("run() = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "unexpected arguments") {
		t.Fatalf("stderr = %q, want unexpected arguments", stderr.String())
	}
}

func TestRunEnrollStoreError(t *testing.T) {
	s := enrollServer(t, "n", "c")
	defer s.Close()

	parentFile := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(parentFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Setenv("SPEEDTEST_AGENT_CREDENTIAL", filepath.Join(parentFile, "credential"))

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"enroll", "--center", s.URL, "--token", "t", "--name", "n",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "agent enroll:") {
		t.Fatalf("stderr = %q, want an enroll error", stderr.String())
	}
}

func TestRunAgentInvalidConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, configPath, "expected_download_mbps: -1\n")
	t.Setenv("SPEEDTEST_AGENT_CONFIG", configPath)
	t.Setenv("SPEEDTEST_AGENT_CREDENTIAL", filepath.Join(t.TempDir(), "credential"))

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"run"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "invalid configuration") {
		t.Fatalf("stdout = %q, want invalid configuration log", stdout.String())
	}
}

func TestRunAgentWorldReadableCredential(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, filepath.Join(dir, "config.yaml"), "name: n\n")
	credPath := filepath.Join(dir, "credential")
	writeCredential(t, credPath, "secret", 0o644)

	t.Setenv("SPEEDTEST_AGENT_CONFIG", filepath.Join(dir, "config.yaml"))
	t.Setenv("SPEEDTEST_AGENT_CREDENTIAL", credPath)

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"run"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "invalid credential") {
		t.Fatalf("stdout = %q, want invalid credential log", stdout.String())
	}
}

func TestRunAgentSuccess(t *testing.T) {
	t.Setenv("SPEEDTEST_SPOOL_DIR", filepath.Join(t.TempDir(), "spool"))
	version = "1.2.3"
	t.Cleanup(func() { version = "dev" })

	dir := t.TempDir()
	writeConfig(t, filepath.Join(dir, "config.yaml"), "name: viettel-fast-50\nexpected_download_mbps: 300\n")
	credPath := filepath.Join(dir, "credential")
	writeCredential(t, credPath, "secret", 0o600)

	t.Setenv("SPEEDTEST_AGENT_CONFIG", filepath.Join(dir, "config.yaml"))
	t.Setenv("SPEEDTEST_AGENT_CREDENTIAL", credPath)
	t.Setenv("SPEEDTEST_AGENT_CENTER", "http://127.0.0.1:1")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var stdout, stderr bytes.Buffer
	code := run(ctx, []string{"run"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, want 0", code)
	}
	if strings.Contains(stdout.String(), "secret") {
		t.Fatalf("stdout = %q, must never log the credential", stdout.String())
	}

	msgs := parseLogMessages(t, stdout.String())
	if !containsMsg(msgs, "agent started") {
		t.Fatalf("logs = %q, want agent started", stdout.String())
	}
	if !strings.Contains(stdout.String(), `"network_path":"default route"`) {
		t.Fatalf("logs = %q, want default route status", stdout.String())
	}
	if !containsMsg(msgs, "agent stopped") {
		t.Fatalf("logs = %q, want agent stopped", stdout.String())
	}
}

func TestRunAgentMissingInterfaceFailsClearly(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, filepath.Join(dir, "config.yaml"), "name: n\nnetwork_interface: missing0\n")
	credPath := filepath.Join(dir, "credential")
	writeCredential(t, credPath, "secret", 0o600)
	t.Setenv("SPEEDTEST_AGENT_CONFIG", filepath.Join(dir, "config.yaml"))
	t.Setenv("SPEEDTEST_AGENT_CREDENTIAL", credPath)

	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"run"}, &stdout, &stderr); code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "invalid network path") || !strings.Contains(stdout.String(), "missing0") || !strings.Contains(stdout.String(), "does not exist") {
		t.Fatalf("stdout = %q, want clear missing-interface error", stdout.String())
	}
}

func TestRunAgentMissingCenter(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, filepath.Join(dir, "config.yaml"), "name: n\n")
	credPath := filepath.Join(dir, "credential")
	writeCredential(t, credPath, "secret", 0o600)

	t.Setenv("SPEEDTEST_AGENT_CONFIG", filepath.Join(dir, "config.yaml"))
	t.Setenv("SPEEDTEST_AGENT_CREDENTIAL", credPath)
	t.Setenv("SPEEDTEST_AGENT_CENTER", "")

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"run"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "invalid center URL") {
		t.Fatalf("stdout = %q, want invalid center URL log", stdout.String())
	}
}

func TestRunAgentUnexpectedArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"run", "extra"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("run() = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "unexpected arguments") {
		t.Fatalf("stderr = %q, want unexpected arguments", stderr.String())
	}
}

func TestRunUninstall(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	credPath := filepath.Join(dir, "credential")
	writeConfig(t, configPath, "name: n\n")
	writeCredential(t, credPath, "secret", 0o600)

	t.Setenv("SPEEDTEST_AGENT_CONFIG", configPath)
	t.Setenv("SPEEDTEST_AGENT_CREDENTIAL", credPath)

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"uninstall"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, want 0", code)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("config still present: %v", err)
	}
	if _, err := os.Stat(credPath); !os.IsNotExist(err) {
		t.Fatalf("credential still present: %v", err)
	}
	if !strings.Contains(stdout.String(), "uninstalled") {
		t.Fatalf("stdout = %q, want uninstalled message", stdout.String())
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("override directory was removed, want it to survive: %v", err)
	}
}

func TestRunUninstallRemovesAgentDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "speedtest-agent")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	credPath := filepath.Join(dir, "credential")
	writeConfig(t, configPath, "name: n\n")
	writeCredential(t, credPath, "secret", 0o600)

	t.Setenv("SPEEDTEST_AGENT_CONFIG", configPath)
	t.Setenv("SPEEDTEST_AGENT_CREDENTIAL", credPath)

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"uninstall"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, want 0", code)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("agent directory still present, want it removed: %v", err)
	}
}

func TestRunUninstallRemoveFailure(t *testing.T) {
	dir := t.TempDir()
	nonEmptyDir := filepath.Join(dir, "busy")
	if err := os.MkdirAll(nonEmptyDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nonEmptyDir, "file"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	t.Setenv("SPEEDTEST_AGENT_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	t.Setenv("SPEEDTEST_AGENT_CREDENTIAL", nonEmptyDir)

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"uninstall"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "agent uninstall: remove") {
		t.Fatalf("stderr = %q, want a remove error", stderr.String())
	}
}

func parseLogMessages(t *testing.T, output string) []string {
	t.Helper()
	var msgs []string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		if msg, ok := rec["msg"].(string); ok {
			msgs = append(msgs, msg)
		}
	}
	return msgs
}

func containsMsg(msgs []string, want string) bool {
	for _, m := range msgs {
		if m == want {
			return true
		}
	}
	return false
}

func TestRepairEnrollmentPreservesValidCredential(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusServiceUnavailable, http.StatusFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var enrolled bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/agent/config" {
					if r.Header.Get("Authorization") != "Bearer test-existing-credential" {
						t.Error("missing existing bearer")
					}
					if status == http.StatusFound {
						w.Header().Set("Location", "http://other.test")
					}
					w.WriteHeader(status)
					return
				}
				enrolled = true
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(201)
				_, _ = w.Write([]byte(`{"success":true,"code":201,"message":"enrolled","data":{"nodeId":"node","credential":"new-test-credential","config":{}},"meta":{},"errors":null}`))
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "credential")
			writeCredential(t, path, "test-existing-credential", 0600)
			t.Setenv("SPEEDTEST_AGENT_CREDENTIAL", path)
			var stdout, stderr bytes.Buffer
			code := runEnrollWithInput(context.Background(), []string{"--center", server.URL, "--token-stdin", "--repair", "--name", "fixture"}, strings.NewReader("test-enrollment-token\n"), &stdout, &stderr)
			switch status {
			case http.StatusOK:
				data, _ := os.ReadFile(path)
				if code != 0 || enrolled || string(data) != "test-existing-credential" {
					t.Fatalf("valid enrollment changed: %d %t %s", code, enrolled, data)
				}
			case http.StatusUnauthorized:
				data, _ := os.ReadFile(path)
				if code != 0 || !enrolled || string(data) != "new-test-credential" {
					t.Fatalf("invalid enrollment not repaired: %d %s", code, stderr.String())
				}
			default:
				if code != 1 || enrolled {
					t.Fatalf("transient failure must preserve credential: %d %t", code, enrolled)
				}
			}
			if strings.Contains(stdout.String()+stderr.String(), "test-enrollment-token") || strings.Contains(stdout.String()+stderr.String(), "test-existing-credential") {
				t.Fatal("credential leaked")
			}
		})
	}
}
