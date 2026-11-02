package geo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	// UpdateInterval is the required weekly refresh period.
	UpdateInterval = 7 * 24 * time.Hour
	maxArchive     = 128 << 20
	maxDatabase    = 256 << 20
)

// Update checks Settings afresh, then updates each edition independently. A
// failed edition retains its previous file and reader; errors contain no URLs,
// remote response bodies, Settings errors or credentials.
func (r *Reader) Update(ctx context.Context, settings UpdaterSettings) error {
	r.updateMu.Lock()
	defer r.updateMu.Unlock()
	enabled, err := settings.AutoUpdate(ctx)
	if err != nil {
		return errors.New("cannot read GeoLite2 update setting")
	}
	if !enabled {
		return nil
	}
	key, err := settings.LicenseKey(ctx)
	if err != nil {
		return errors.New("cannot read GeoLite2 licence key")
	}
	if strings.TrimSpace(key) == "" {
		r.keyMissing.Do(func() { r.log.Warn("GeoLite2 licence key missing; automatic updates disabled until configured") })
		return nil
	}
	client := &http.Client{Timeout: 2 * time.Minute, CheckRedirect: safeRedirect}
	var failures []error
	for _, edition := range []struct{ kind, target string }{{"City", r.cfg.CityPath}, {"ASN", r.cfg.ASNPath}} {
		if err := r.updateEdition(ctx, client, key, edition.kind, edition.target); err != nil {
			failures = append(failures, fmt.Errorf("GeoLite2-%s: %w", edition.kind, err))
		}
	}
	return errors.Join(failures...)
}

// Run performs an initial check and then checks weekly until cancellation.
// It blocks so the application controls cancellation and joins its goroutine.
func (r *Reader) Run(ctx context.Context, settings UpdaterSettings) {
	r.run(ctx, settings, UpdateInterval)
}

func (r *Reader) run(ctx context.Context, settings UpdaterSettings, interval time.Duration) {
	if ctx.Err() != nil {
		return
	}
	check := func() {
		if err := r.Update(ctx, settings); err != nil && ctx.Err() == nil {
			r.log.Error("GeoLite2 update failed; failed editions retain previous databases", "error", err)
		}
	}
	check()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		}
	}
}

func safeRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("too many download redirects")
	}
	if req.URL.Scheme != "https" {
		return errors.New("download redirect must use HTTPS")
	}
	// Signed storage URLs do not need MaxMind credentials, even on subdomains.
	req.Header.Del("Authorization")
	return nil
}

func (r *Reader) download(ctx context.Context, client *http.Client, key, kind, suffix string, limit int64) ([]byte, error) {
	base, err := url.Parse(r.cfg.DownloadURL)
	if err != nil || base.Host == "" || (base.Scheme != "https" && (base.Scheme != "http" || (base.Hostname() != "127.0.0.1" && base.Hostname() != "localhost" && base.Hostname() != "::1"))) || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("invalid download origin (HTTPS required outside loopback)")
	}
	query := url.Values{"suffix": {suffix}}
	if r.cfg.AccountID == "" {
		base.Path = path.Join(base.Path, "app/geoip_download")
		query.Set("edition_id", "GeoLite2-"+kind)
		query.Set("license_key", key)
	} else {
		base.Path = path.Join(base.Path, "geoip/databases/GeoLite2-"+kind+"/download")
	}
	base.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return nil, errors.New("cannot create download request")
	}
	if r.cfg.AccountID != "" {
		req.SetBasicAuth(r.cfg.AccountID, key)
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, errors.New("download request failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download HTTP status %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, errors.New("cannot read download")
	}
	if int64(len(data)) > limit {
		return nil, errors.New("download exceeds size limit")
	}
	return data, nil
}

func (r *Reader) updateEdition(ctx context.Context, client *http.Client, key, kind, target string) error {
	archive, err := r.download(ctx, client, key, kind, "tar.gz", maxArchive)
	if err != nil {
		return err
	}
	checksum, err := r.download(ctx, client, key, kind, "tar.gz.sha256", 4096)
	if err != nil {
		return err
	}
	fields := strings.Fields(string(checksum))
	digest := sha256.Sum256(archive)
	if len(fields) == 0 || !strings.EqualFold(fields[0], hex.EncodeToString(digest[:])) {
		return errors.New("archive checksum mismatch")
	}
	data, err := extract(archive, "GeoLite2-"+kind+".mmdb")
	if err != nil {
		return err
	}
	db, err := decode(data, kind)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		_ = db.Close()
		return err
	}
	// Rename and reader replacement share the lookup lock. A reader never sees a
	// partially written file. The temporary file is on the destination filesystem.
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := replace(target, data); err != nil {
		_ = db.Close()
		return err
	}
	old := r.asn
	if kind == "City" {
		old = r.city
		r.city = db
	} else {
		r.asn = db
	}
	if old != nil {
		_ = old.Close()
	}
	r.log.Info("GeoLite2 database updated", "edition", kind)
	return nil
}

func extract(archive []byte, filename string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, errors.New("invalid gzip archive")
	}
	defer func() { _ = gz.Close() }()
	// Bound all decompression, including unrelated archive entries and padding.
	plain, err := io.ReadAll(io.LimitReader(gz, maxDatabase+1))
	if err != nil || len(plain) > maxDatabase {
		return nil, errors.New("invalid or oversized decompressed archive")
	}
	tr := tar.NewReader(bytes.NewReader(plain))
	var result []byte
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.New("invalid tar archive")
		}
		if path.Base(header.Name) != filename {
			continue
		}
		if header.Typeflag != tar.TypeReg || result != nil {
			return nil, errors.New("invalid or duplicate database entry")
		}
		result, err = io.ReadAll(tr)
		if err != nil {
			return nil, errors.New("cannot extract database")
		}
	}
	if len(result) == 0 {
		return nil, errors.New("database missing from archive")
	}
	return result, nil
}

func replace(target string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return errors.New("cannot create database directory")
	}
	file, err := os.CreateTemp(filepath.Dir(target), ".geolite2-*")
	if err != nil {
		return errors.New("cannot stage database")
	}
	defer func() { _ = os.Remove(file.Name()) }()
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return errors.New("cannot write staged database")
	}
	if err := os.Rename(file.Name(), target); err != nil {
		return errors.New("cannot replace database")
	}
	return nil
}
