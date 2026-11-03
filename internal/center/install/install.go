// Package install serves the agent installer script and the release artifacts
// it downloads from the command center.
package install

import (
	_ "embed"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

//go:embed install.sh
var scriptTemplate string

// artifactName lists the only files the center serves from the release directory.
var artifactName = regexp.MustCompile(`^(speedtest-agent-linux-(amd64|arm64)|speedtest-agent\.service|SHA256SUMS)$`)

// publicURLPattern keeps the URL inert when it is embedded into a shell script.
var publicURLPattern = regexp.MustCompile(`^https?://[A-Za-z0-9._-]+(:[0-9]{1,5})?(/[A-Za-z0-9._~/-]*)?$`)

// Config configures the installer endpoints.
type Config struct {
	// PublicURL is the address agents use to reach the center, for example
	// https://speedtest.example.com. It is written into the script, so it is
	// configured, never taken from the request.
	PublicURL string
	// ReleaseDir holds the files produced by `make release` (the dist directory).
	ReleaseDir string
}

// Handler serves GET /install.sh and GET /download/<artifact>.
func Handler(cfg Config) (http.Handler, error) {
	base := strings.TrimRight(cfg.PublicURL, "/")
	if !publicURLPattern.MatchString(base) {
		return nil, errors.New("install: public URL must be an http(s) address without credentials, query or unusual characters")
	}
	if u, err := url.Parse(base); err != nil || u.Host == "" {
		return nil, errors.New("install: invalid public URL")
	}
	info, err := os.Stat(cfg.ReleaseDir)
	if err != nil || !info.IsDir() {
		return nil, errors.New("install: release directory does not exist")
	}
	script := strings.ReplaceAll(scriptTemplate, "__CENTER_URL__", base)
	releaseDir := cfg.ReleaseDir

	mux := http.NewServeMux()
	mux.HandleFunc("GET /install.sh", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write([]byte(script))
	})
	mux.HandleFunc("GET /download/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !artifactName.MatchString(name) {
			http.NotFound(w, r)
			return
		}
		file := filepath.Join(releaseDir, name)
		if st, err := os.Stat(file); err != nil || !st.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeFile(w, r, file)
	})
	return mux, nil
}
