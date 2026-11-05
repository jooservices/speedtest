//go:build integration

package ownerapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/agentapi"
	"github.com/jooservices/speedtest/internal/center/install"
	"github.com/jooservices/speedtest/internal/center/release"
)

// This browser-free integration crosses PostgreSQL, the published filesystem,
// owner views, agent configuration HTTP responses, ETags and the real download handler.
func TestPublishedReleaseFlow(t *testing.T) {
	pool, ctx := openPool(t)
	id := seedNode(t, ctx, pool, 1, "release-node", nil)
	dir := t.TempDir()
	files := map[string]string{}
	for _, arch := range []string{"amd64", "arm64"} {
		name := "speedtest-agent-linux-" + arch
		data := []byte("binary-" + arch)
		sum := sha256.Sum256(data)
		files[name] = hex.EncodeToString(sum[:])
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	data, err := json.Marshal(map[string]any{"version": "2.0.0", "commit": "abcdef", "builtAt": time.Now().UTC().Format(time.RFC3339), "files": files})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "release.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	downloads, err := install.Handler(install.Config{PublicURL: "http://center.test", ReleaseDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	downloadServer := httptest.NewServer(downloads)
	defer downloadServer.Close()
	catalog := &release.Catalog{Dir: dir, PublicURL: downloadServer.URL}
	owner := &Store{DB: pool, Releases: catalog}
	agent := &agentapi.Postgres{Pool: pool, Releases: catalog}
	handler := agentapi.Handler{Store: agent}
	configRequest := func(etag string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/agent/config", nil)
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		response := httptest.NewRecorder()
		handler.ServeNode(response, req, id, "")
		return response
	}
	previousTag := ""
	for _, tc := range []struct {
		identity, platform string
		update             bool
	}{
		{"speedtest-agent/1.0.0", "amd64", true},
		{"speedtest-agent/1.0.0 (linux/arm64)", "arm64", true},
		{"speedtest-agent/2.0.0 (linux/arm64)", "arm64", false},
		{"speedtest-agent/3.0.0 (linux/amd64)", "amd64", false},
		{"speedtest-agent/0.0.0-dev+abc (linux/amd64)", "amd64", false},
		{"speedtest-agent/1.0.0 (darwin/arm64)", "", false},
	} {
		if _, err := agent.Heartbeat(ctx, id, agentapi.Heartbeat{Agent: tc.identity}); err != nil {
			t.Fatal(err)
		}
		view, _, err := owner.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if (view["updateVersion"] != nil) != tc.update || strings.Contains(view["agent"].(string), " (") {
			t.Fatal(tc, view)
		}
		list, _, err := owner.List(ctx, 1, 10)
		if err != nil || len(list) != 1 || list[0]["updateVersion"] != view["updateVersion"] {
			t.Fatal(list, err)
		}
		cfg, err := agent.Config(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !tc.update {
			if cfg["release"] != nil {
				t.Fatal(cfg)
			}
			continue
		}
		responseConfig := configRequest("")
		if responseConfig.Code != 200 || !strings.Contains(responseConfig.Body.String(), files["speedtest-agent-linux-"+tc.platform]) {
			t.Fatal(responseConfig.Code, responseConfig.Body.String())
		}
		tag := responseConfig.Header().Get("ETag")
		if tag == "" || configRequest(tag).Code != 304 {
			t.Fatal("config ETag not honored")
		}
		if tc.platform == "arm64" && tc.update && tag == previousTag {
			t.Fatal("platform change did not invalidate ETag")
		}
		previousTag = tag
		ad := cfg["release"].(map[string]any)
		name := "speedtest-agent-linux-" + tc.platform
		if ad["version"] != "2.0.0" || ad["sha256"] != files[name] || ad["url"] != downloadServer.URL+"/download/"+name {
			t.Fatal(ad)
		}
		response, err := http.Get(ad["url"].(string))
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatal(response.Status)
		}
		_ = response.Body.Close()
	}
	if _, err := agent.Heartbeat(ctx, id, agentapi.Heartbeat{Agent: "speedtest-agent/1.0.0"}); err != nil {
		t.Fatal(err)
	}
	explicit := `{"version":"4.0.0","url":"https://example.test/custom","sha256":"` + strings.Repeat("c", 64) + `"}`
	if _, err := pool.Exec(ctx, `INSERT INTO settings(key,value) VALUES ('agentRelease',$1)`, explicit); err != nil {
		t.Fatal(err)
	}
	cfg, err := agent.Config(ctx, id)
	if err != nil || cfg["release"].(map[string]any)["version"] != "4.0.0" {
		t.Fatal(cfg, err)
	}
	if response := configRequest(previousTag); response.Code != 200 || !strings.Contains(response.Body.String(), "4.0.0") {
		t.Fatal("override did not invalidate ETag", response.Code)
	}
	view, _, err := owner.Get(ctx, id)
	if err != nil || view["updateVersion"] != "4.0.0" {
		t.Fatal(view, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM settings WHERE key='agentRelease'`); err != nil {
		t.Fatal(err)
	}
	// A new catalog models startup with an invalid publication; neither API fails.
	broken := &release.Catalog{Dir: t.TempDir(), PublicURL: downloadServer.URL}
	owner.Releases = broken
	agent.Releases = broken
	view, _, err = owner.Get(ctx, id)
	if err != nil || view["updateVersion"] != nil {
		t.Fatal(view, err)
	}
	cfg, err = agent.Config(ctx, id)
	if err != nil || cfg["release"] != nil {
		t.Fatal(cfg, err)
	}
}
