// Package release reads the published manifest shared by owner views and agent configuration.
package release

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jooservices/speedtest/internal/shared/agentid"
)

var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-([0-9A-Za-z-]+\.)*[0-9A-Za-z-]+)?(\+([0-9A-Za-z-]+\.)*[0-9A-Za-z-]+)?$`)
var hashPattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
var filePattern = regexp.MustCompile(`^(speedtest-agent-linux-(amd64|arm64)|speedtest-(center|migrate)-linux-amd64|speedtest-agent\.service)$`)

type manifest struct {
	Version string            `json:"version"`
	Commit  string            `json:"commit"`
	BuiltAt time.Time         `json:"builtAt"`
	Files   map[string]string `json:"files"`
}

// Catalog caches both valid and invalid manifests for one minute.
// It is safe to share between all HTTP handlers.
type Catalog struct {
	Dir, PublicURL string
	Logger         *slog.Logger
	mu             sync.Mutex
	checked        time.Time
	cached         *manifest
	now            func() time.Time
}

func validVersion(v string) bool {
	if len(v) > 64 || !versionPattern.MatchString(v) {
		return false
	}
	base := strings.SplitN(v, "+", 2)[0]
	if _, pre, ok := strings.Cut(base, "-"); ok {
		for _, part := range strings.Split(pre, ".") {
			if len(part) > 1 && part[0] == '0' && strings.Trim(part, "0123456789") == "" {
				return false
			}
		}
	}
	return true
}

func load(dir string) (*manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, "release.json"))
	if err != nil {
		return nil, err
	}
	var m manifest
	if json.Unmarshal(data, &m) != nil || !validVersion(m.Version) || m.Commit == "" || m.BuiltAt.IsZero() || len(m.Files) == 0 {
		return nil, errors.New("invalid release metadata")
	}
	for name, hash := range m.Files {
		if !filePattern.MatchString(name) || !hashPattern.MatchString(hash) {
			return nil, errors.New("invalid release artifact")
		}
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || !info.Mode().IsRegular() {
			return nil, errors.New("missing release artifact")
		}
	}
	return &m, nil
}

// Advertisement returns the release for this node's platform, or nil when unavailable.
func (c *Catalog) Advertisement(agent string) map[string]any {
	if c == nil || c.Dir == "" || c.PublicURL == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.now != nil {
		now = c.now()
	}
	if c.checked.IsZero() || now.Sub(c.checked) >= time.Minute {
		c.checked = now
		var err error
		c.cached, err = load(c.Dir)
		if err != nil {
			logger := c.Logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.Warn("agent release manifest unavailable", "error", err)
		}
	}
	if c.cached == nil {
		return nil
	}
	_, platform := agentid.Parse(agent)
	name := "speedtest-agent-" + platform
	hash, ok := c.cached.Files[name]
	if !ok {
		return nil
	}
	advertised := map[string]any{"version": c.cached.Version, "url": strings.TrimRight(c.PublicURL, "/") + "/download/" + name, "sha256": strings.ToLower(hash)}
	// The existing updater accepts any different version. Only advertise upgrades,
	// so automatic updates cannot downgrade a node or replace a development build.
	if UpdateVersion(agent, advertised) == nil {
		return nil
	}
	return advertised
}

// UpdateVersion only offers stable nodes a strictly newer SemVer release.
// Build metadata has no precedence; development and prerelease nodes are ignored.
func UpdateVersion(agent string, advertisement map[string]any) any {
	display, _ := agentid.Parse(agent)
	current := strings.TrimPrefix(display, "speedtest-agent/")
	target, _ := advertisement["version"].(string)
	if !validVersion(current) || !validVersion(target) || strings.Contains(strings.SplitN(current, "+", 2)[0], "-") {
		return nil
	}
	a := strings.Split(strings.SplitN(current, "+", 2)[0], ".")
	b := strings.Split(strings.SplitN(strings.SplitN(target, "+", 2)[0], "-", 2)[0], ".")
	for i := range a {
		if a[i] == b[i] {
			continue
		}
		if len(a[i]) < len(b[i]) || len(a[i]) == len(b[i]) && a[i] < b[i] {
			return target
		}
		return nil
	}
	return nil
}
