package release

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func publish(t *testing.T, dir string, m manifest) {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "release.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}
func fixture(t *testing.T) (string, manifest) {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{"speedtest-agent-linux-amd64": strings.Repeat("a", 64), "speedtest-agent-linux-arm64": strings.Repeat("b", 64)}
	for name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("binary"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	m := manifest{Version: "2.0.0", Commit: "abc123", BuiltAt: time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC), Files: files}
	publish(t, dir, m)
	return dir, m
}
func TestAdvertisementCache(t *testing.T) {
	dir, m := fixture(t)
	now := time.Now()
	var logs bytes.Buffer
	c := &Catalog{Dir: dir, PublicURL: "https://center.test/", Logger: slog.New(slog.NewTextHandler(&logs, nil)), now: func() time.Time { return now }}
	a := c.Advertisement("speedtest-agent/1.0.0")
	if a["version"] != "2.0.0" || a["url"] != "https://center.test/download/speedtest-agent-linux-amd64" || a["sha256"] != strings.Repeat("a", 64) {
		t.Fatal(a)
	}
	a["version"] = "mutated"
	m.Version = "3.0.0"
	m.Files["speedtest-agent-linux-arm64"] = strings.Repeat("B", 64)
	publish(t, dir, m)
	if c.Advertisement("speedtest-agent/1.0.0")["version"] != "2.0.0" {
		t.Fatal("cache not isolated")
	}
	now = now.Add(time.Minute)
	if c.Advertisement("speedtest-agent/1.0.0")["version"] != "3.0.0" {
		t.Fatal("cache did not refresh")
	}
	if c.Advertisement("speedtest-agent/1.0.0 (linux/arm64)")["sha256"] != strings.Repeat("b", 64) {
		t.Fatal("wrong architecture")
	}
	if c.Advertisement("speedtest-agent/1.0.0 (darwin/arm64)") != nil {
		t.Fatal("unsupported platform")
	}
	if err := os.Remove(filepath.Join(dir, "release.json")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if c.Advertisement("speedtest-agent/1.0.0") != nil || !strings.Contains(logs.String(), "WARN") {
		t.Fatal("missing manifest must warn and suppress")
	}
	publish(t, dir, m)
	if c.Advertisement("speedtest-agent/1.0.0") != nil {
		t.Fatal("invalid result not cached")
	}
	now = now.Add(time.Minute)
	if c.Advertisement("speedtest-agent/1.0.0") == nil {
		t.Fatal("no recovery")
	}
	for _, identity := range []string{"speedtest-agent/3.0.0", "speedtest-agent/4.0.0", "speedtest-agent/0.0.0-dev+abc", "unknown"} {
		if c.Advertisement(identity) != nil {
			t.Fatalf("unsafe advertisement for %s", identity)
		}
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() { _ = c.Advertisement("speedtest-agent/1.0.0") })
	}
	wg.Wait()
	var empty *Catalog
	if empty.Advertisement("speedtest-agent/1.0.0") != nil || (&Catalog{}).Advertisement("speedtest-agent/1.0.0") != nil {
		t.Fatal("unconfigured catalog")
	}
}
func TestValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*manifest)
	}{
		{"version", func(m *manifest) { m.Version = "1.2" }},
		{"leading zero", func(m *manifest) { m.Version = "01.2.3" }},
		{"prerelease leading zero", func(m *manifest) { m.Version = "1.2.3-01" }},
		{"too long", func(m *manifest) { m.Version = "1.2.3+" + strings.Repeat("a", 65) }},
		{"commit", func(m *manifest) { m.Commit = "" }},
		{"time", func(m *manifest) { m.BuiltAt = time.Time{} }},
		{"empty", func(m *manifest) { m.Files = nil }},
		{"hash", func(m *manifest) { m.Files["speedtest-agent-linux-amd64"] = "oops" }},
		{"traversal", func(m *manifest) { m.Files["../escape"] = strings.Repeat("a", 64) }},
		{"missing", func(m *manifest) { m.Files["speedtest-center-linux-amd64"] = strings.Repeat("a", 64) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, m := fixture(t)
			tc.change(&m)
			publish(t, dir, m)
			if _, err := load(dir); err == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
	dir, m := fixture(t)
	for _, v := range []string{"1.2.3", "1.2.3-beta.1", "0.0.0-dev+abc", "1.2.3+build.01"} {
		m.Version = v
		publish(t, dir, m)
		if _, err := load(dir); err != nil {
			t.Fatal(v, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "release.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := load(dir); err == nil {
		t.Fatal("bad JSON accepted")
	}
}
func TestUpdateVersion(t *testing.T) {
	for _, tc := range []struct {
		current, target string
		want            bool
	}{
		{"1.2.3", "1.2.4", true}, {"1.9.9", "1.10.0", true}, {"9.9.9", "10.0.0", true},
		{"2.0.0", "1.9.9", false}, {"1.2.3", "1.2.3", false}, {"1.2.3+abc", "1.2.3+def", false},
		{"1.2.3-rc.1", "2.0.0", false}, {"0.0.0-dev+abc", "2.0.0", false}, {"dev", "2.0.0", false},
		{"1.0.0", "bad", false}, {"1.0.0", "2.0.0-rc.1", true}, {"2.0.0", "2.0.0-rc.1", false},
	} {
		got := UpdateVersion("speedtest-agent/"+tc.current+" (linux/arm64)", map[string]any{"version": tc.target})
		if (got != nil) != tc.want || tc.want && got != tc.target {
			t.Errorf("%+v: %v", tc, got)
		}
	}
	if UpdateVersion("1.0.0", nil) != nil {
		t.Fatal("nil advertisement")
	}
}

func TestManifestWriterProcess(t *testing.T) {
	dir := t.TempDir()
	checks := ""
	for _, name := range []string{"speedtest-agent-linux-amd64", "speedtest-agent-linux-arm64", "speedtest-center-linux-amd64", "speedtest-migrate-linux-amd64", "speedtest-agent.service"} {
		checks += strings.Repeat("a", 64) + "  " + name + "\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte("binary"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(checks), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", "../../../scripts/release-manifest.py", dir, "1.2.3", "abcdef", "2026-11-05T00:00:00Z")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("writer: %v: %s", err, output)
	}
	m, err := load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "1.2.3" || m.Commit != "abcdef" || m.BuiltAt.Format(time.RFC3339) != "2026-11-05T00:00:00Z" || len(m.Files) != 5 {
		t.Fatal(m)
	}
	for _, checksum := range m.Files {
		if checksum != strings.Repeat("a", 64) {
			t.Fatal(checksum)
		}
	}
}

func TestArtifactMustBeRegular(t *testing.T) {
	for _, kind := range []string{"directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir, _ := fixture(t)
			name := filepath.Join(dir, "speedtest-agent-linux-amd64")
			if err := os.Remove(name); err != nil {
				t.Fatal(err)
			}
			var err error
			if kind == "directory" {
				err = os.Mkdir(name, 0700)
			} else {
				err = os.Symlink("speedtest-agent-linux-arm64", name)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := load(dir); err == nil {
				t.Fatal("nonregular artifact accepted")
			}
		})
	}
}
