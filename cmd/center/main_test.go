package main

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/shared/config"
)

func TestUIFilesystemEmbedded(t *testing.T) {
	ui, source, err := uiFilesystem("")
	if err != nil {
		t.Fatalf("uiFilesystem() error = %v, want nil", err)
	}
	if source != "embedded" {
		t.Fatalf("uiFilesystem() source = %q, want embedded", source)
	}

	assertUIFile(t, ui, "index.html")
	assertUIFile(t, ui, "assets/css/app.css")
}

func TestUIFilesystemFromDisk(t *testing.T) {
	root := t.TempDir()
	cssDirectory := filepath.Join(root, "assets", "css")
	if err := os.MkdirAll(cssDirectory, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	files := map[string]string{
		"index.html":         "<!doctype html><title>development UI</title>",
		"assets/css/app.css": "body { color: black; }",
	}
	for name, contents := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", name, err)
		}
	}

	ui, source, err := uiFilesystem(root)
	if err != nil {
		t.Fatalf("uiFilesystem() error = %v, want nil", err)
	}
	if source != "disk" {
		t.Fatalf("uiFilesystem() source = %q, want disk", source)
	}
	for name, want := range files {
		contents, err := fs.ReadFile(ui, name)
		if err != nil {
			t.Fatalf("ReadFile(%q) error = %v", name, err)
		}
		if string(contents) != want {
			t.Errorf("ReadFile(%q) = %q, want %q", name, contents, want)
		}
	}
}

func TestUIFilesystemInvalidPath(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(file, []byte("file"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	tests := []struct {
		name string
		path string
	}{
		{name: "missing", path: filepath.Join(root, "missing")},
		{name: "file", path: file},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := uiFilesystem(tt.path); err == nil {
				t.Fatal("uiFilesystem() error = nil, want an error")
			}
		})
	}
}

func TestRunServesEmbeddedUIAndStops(t *testing.T) {
	assertRunServesUI(t, nil, "", "embedded")
}

func TestRunServesDiskUIFromFlagAndStops(t *testing.T) {
	root := writeDiskUI(t)
	assertRunServesUI(t, []string{"-ui-dir", root}, "", "disk")
}

func TestRunServesDiskUIFromEnvironment(t *testing.T) {
	root := writeDiskUI(t)
	assertRunServesUI(t, nil, root, "disk")
}

func TestRunRejectsInvalidConfiguration(t *testing.T) {
	resetCenterEnvironment(t)
	t.Setenv("SPEEDTEST_DB_PORT", "invalid")
	if err := run(context.Background(), nil, slog.Default(), io.Discard); err == nil {
		t.Fatal("run() error = nil, want invalid configuration error")
	}
}

func TestRunRejectsInvalidArgumentsAndUIPath(t *testing.T) {
	resetCenterEnvironment(t)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	if err := run(context.Background(), []string{"unexpected"}, logger, io.Discard); err == nil {
		t.Error("run() with positional argument error = nil, want an error")
	}
	if err := run(context.Background(), []string{"-unknown"}, logger, io.Discard); err == nil {
		t.Error("run() with unknown flag error = nil, want an error")
	}
	if err := run(context.Background(), []string{"-ui-dir", filepath.Join(t.TempDir(), "missing")}, logger, io.Discard); err == nil {
		t.Error("run() with missing UI directory error = nil, want an error")
	}
}

func TestRunRejectsOccupiedAddress(t *testing.T) {
	resetCenterEnvironment(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Errorf("listener.Close() error = %v", err)
		}
	})
	t.Setenv("SPEEDTEST_HTTP_ADDR", listener.Addr().String())

	if err := run(context.Background(), nil, slog.Default(), io.Discard); err == nil {
		t.Fatal("run() error = nil, want listen error")
	}
}

func TestMainRunHelp(t *testing.T) {
	resetCenterEnvironment(t)
	var stdout, stderr bytes.Buffer
	if code := mainRun([]string{"-h"}, &stdout, &stderr); code != 0 {
		t.Fatalf("mainRun() = %d, want 0", code)
	}
	if !strings.Contains(stderr.String(), "-ui-dir") {
		t.Errorf("help output = %q, want the -ui-dir flag", stderr.String())
	}
	if code := mainRun([]string{"-unknown"}, &stdout, &stderr); code != 1 {
		t.Errorf("mainRun() with unknown flag = %d, want 1", code)
	}
}

func TestDefaultSeedOnlyAllowsLocalAndTest(t *testing.T) {
	for _, env := range []string{"local", "test"} {
		if err := validateDefaultSeedEnvironment(env); err != nil {
			t.Errorf("APP_ENV=%s rejected: %v", env, err)
		}
	}
	for _, env := range []string{"staging", "production", "", "development"} {
		if err := validateDefaultSeedEnvironment(env); err == nil {
			t.Errorf("APP_ENV=%s accepted the default administrator seed", env)
		}
	}
}

func TestDefaultSeedRequiresExplicitSafeEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name       string
		env        string
		configured bool
		wantErr    bool
	}{
		{name: "local", env: "local", configured: true},
		{name: "test", env: "TEST", configured: true},
		{name: "missing", wantErr: true},
		{name: "blank", env: " ", configured: true, wantErr: true},
		{name: "staging", env: "staging", configured: true, wantErr: true},
		{name: "production", env: "production", configured: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateExplicitDefaultSeedEnvironment(tc.env, tc.configured)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateExplicitDefaultSeedEnvironment() error = %v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestLocalCenterRequiresSafeDatabaseName(t *testing.T) {
	for _, tc := range []struct {
		name    string
		appEnv  string
		dbName  string
		wantErr bool
	}{
		{name: "local worktree", appEnv: "local", dbName: "speedtest_wt12"},
		{name: "test database", appEnv: "test", dbName: "speedtest_test"},
		{name: "local live database", appEnv: "local", dbName: "speedtest", wantErr: true},
		{name: "production database", appEnv: "production", dbName: "speedtest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.CenterConfig{AppEnv: tc.appEnv, Database: config.DatabaseConfig{Name: tc.dbName}}
			err := validateLocalDatabaseName(cfg)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateLocalDatabaseName() error = %v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func assertUIFile(t *testing.T, ui fs.FS, name string) {
	t.Helper()
	file, err := ui.Open(name)
	if err != nil {
		t.Fatalf("Open(%q) error = %v", name, err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close(%q) error = %v", name, err)
	}
}

func assertRunServesUI(t *testing.T, args []string, uiDir, expectedSource string) {
	t.Helper()
	resetCenterEnvironment(t)
	address := freeTCPAddress(t)
	t.Setenv("SPEEDTEST_HTTP_ADDR", address)
	t.Setenv("SPEEDTEST_UI_DIR", uiDir)

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() {
		runDone <- run(ctx, args, logger, io.Discard)
	}()

	client := &http.Client{Timeout: time.Second}
	baseURL := "http://" + address
	deadline := time.Now().Add(3 * time.Second)
	for {
		response, err := client.Get(baseURL + "/healthz")
		if err == nil {
			if response.StatusCode != http.StatusOK {
				_ = response.Body.Close()
				cancel()
				t.Fatalf("GET /healthz status = %d, want 200", response.StatusCode)
			}
			if closeErr := response.Body.Close(); closeErr != nil {
				cancel()
				t.Fatalf("close /healthz response: %v", closeErr)
			}
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("command center did not start: %v", err)
		}
		select {
		case runErr := <-runDone:
			cancel()
			t.Fatalf("run() returned before accepting requests: %v", runErr)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}

	effectiveUIDir := uiDir
	if len(args) == 2 && args[0] == "-ui-dir" {
		effectiveUIDir = args[1]
	}
	expectedUI, _, err := uiFilesystem(effectiveUIDir)
	if err != nil {
		cancel()
		t.Fatalf("uiFilesystem() error = %v", err)
	}
	for _, asset := range []struct {
		path string
		file string
	}{
		{path: "/", file: "index.html"},
		{path: "/assets/css/app.css", file: "assets/css/app.css"},
	} {
		response, err := client.Get(baseURL + asset.path)
		if err != nil {
			cancel()
			t.Fatalf("GET %s error = %v", asset.path, err)
		}
		body, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil {
			cancel()
			t.Fatalf("read GET %s response: read=%v close=%v", asset.path, readErr, closeErr)
		}
		if response.StatusCode != http.StatusOK {
			cancel()
			t.Fatalf("GET %s status = %d, want 200", asset.path, response.StatusCode)
		}
		want, readErr := fs.ReadFile(expectedUI, asset.file)
		if readErr != nil {
			cancel()
			t.Fatalf("read expected UI file %s: %v", asset.file, readErr)
		}
		if !bytes.Equal(body, want) {
			cancel()
			t.Errorf("GET %s did not return %s", asset.path, asset.file)
		}
	}

	response, err := client.Get(baseURL + "/readyz")
	if err != nil {
		cancel()
		t.Fatalf("GET /readyz error = %v", err)
	}
	readyBody, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		cancel()
		t.Fatalf("read /readyz response: read=%v close=%v", readErr, closeErr)
	}
	if response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(readyBody), "not configured") {
		cancel()
		t.Fatalf("GET /readyz = %d %q, want 503 with not configured", response.StatusCode, readyBody)
	}

	client.CloseIdleConnections()
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("run() after cancellation error = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run() did not stop after cancellation")
	}
	if !strings.Contains(logs.String(), `"ui_source":"`+expectedSource+`"`) {
		t.Errorf("startup logs = %q, want ui_source %q", logs.String(), expectedSource)
	}
}

func freeTCPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("listener.Close() error = %v", err)
	}
	return address
}

func writeDiskUI(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "assets", "css"), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	for name, contents := range map[string]string{
		"index.html":         "<!doctype html><title>development UI</title>",
		"assets/css/app.css": "body { color: black; }",
	} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", name, err)
		}
	}
	return root
}

func resetCenterEnvironment(t *testing.T) {
	t.Helper()
	for key, value := range map[string]string{
		"APP_ENV":                             "",
		"SPEEDTEST_HTTP_ADDR":                 "",
		"SPEEDTEST_UI_DIR":                    "",
		"SPEEDTEST_SESSION_TTL":               "",
		"SPEEDTEST_LOGIN_MAX_ATTEMPTS":        "",
		"SPEEDTEST_LOGIN_WINDOW":              "",
		"SPEEDTEST_TRUSTED_PROXY_CIDRS":       "",
		"SPEEDTEST_DB_PORT":                   "",
		"SPEEDTEST_DB_SSLMODE":                "",
		"SPEEDTEST_DB_PASSWORD":               "",
		"SPEEDTEST_AGENT_REQUESTS_PER_MINUTE": "",
		"APP_KEY":                             "",
	} {
		t.Setenv(key, value)
	}
}

func TestStartupAndSeedFailClosedOnMalformedEnvironmentFile(t *testing.T) {
	root, err := filepath.Abs("../../.tmp")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(root, "center-env-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Chdir(dir)
	if err = os.WriteFile(".env", []byte("INVALID ENV KEY = value\ninvalid[syntax\n"), 0600); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	if err = loadDotEnv(); err == nil {
		t.Fatal("malformed dotenv accepted")
	}
	if err = runWithDatabase(t.Context(), nil, logger, io.Discard); err == nil {
		t.Fatal("startup accepted malformed dotenv")
	}
	if err = runSeedAdmin(t.Context(), nil, io.Discard); err == nil {
		t.Fatal("seed accepted malformed dotenv")
	}
	if status := mainRun([]string{"seed-admin", "unexpected"}, io.Discard, io.Discard); status != 1 {
		t.Fatal("seed failure exit", status)
	}
}

func TestStartupClosesListenerOnInstallerFailure(t *testing.T) {
	address := freeTCPAddress(t)
	cfg := config.CenterConfig{HTTPAddr: address, PublicURL: "https://speedtest.example.test", ReleaseDir: filepath.Join(t.TempDir(), "missing")}
	if err := serve(t.Context(), nil, slog.Default(), io.Discard, cfg, nil, nil, nil, nil); err == nil {
		t.Fatal("missing release directory accepted")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal("startup failure leaked HTTP listener", err)
	}
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
}
