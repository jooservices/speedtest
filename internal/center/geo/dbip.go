package geo

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// DB-IP "IP to City Lite" and "IP to ASN Lite" are free monthly databases in the MaxMind MMDB format, published
// under CC BY 4.0 (attribution: "IP Geolocation by DB-IP", https://db-ip.com). No key or account is needed.
const (
	// DBIPInterval is how often the updater looks at the files; a database is replaced only when it is older than
	// dbipMaxAge, so a restart never downloads again.
	DBIPInterval = 24 * time.Hour
	dbipMaxAge   = 25 * 24 * time.Hour
	maxDBIP      = 192 << 20 // compressed size limit; the City file is about 60 MB compressed
	// DBIPAttribution must be shown wherever DB-IP data is used.
	DBIPAttribution = "IP Geolocation by DB-IP"
)

// DBIPConfig controls the built-in DB-IP Lite updater.
type DBIPConfig struct {
	Enabled bool
	URL     string
	// Now is the clock; nil uses time.Now. Tests set it.
	Now func() time.Time
}

// DBIPConfigFromEnv reads SPEEDTEST_GEO_DBIP_AUTO_UPDATE and SPEEDTEST_GEO_DBIP_URL. The updater is on by default only when
// APP_ENV=production, so development, tests and E2E runs never download a 130 MB database unasked.
func DBIPConfigFromEnv() (DBIPConfig, error) {
	cfg := DBIPConfig{Enabled: os.Getenv("APP_ENV") == "production", URL: envDefault("SPEEDTEST_GEO_DBIP_URL", "https://download.db-ip.com")}
	if raw := os.Getenv("SPEEDTEST_GEO_DBIP_AUTO_UPDATE"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return DBIPConfig{}, errors.New("SPEEDTEST_GEO_DBIP_AUTO_UPDATE must be a boolean")
		}
		cfg.Enabled = value
	}
	return cfg, nil
}

// RunDBIP keeps the City and ASN databases fresh from DB-IP Lite until the context ends. It does nothing when a
// MaxMind licence key is configured (GeoLite2 then takes precedence) or when it is disabled. It blocks.
func (r *Reader) RunDBIP(ctx context.Context, cfg DBIPConfig) {
	r.runDBIP(ctx, cfg, DBIPInterval)
}

func (r *Reader) runDBIP(ctx context.Context, cfg DBIPConfig, interval time.Duration) {
	if !cfg.Enabled || strings.TrimSpace(os.Getenv("SPEEDTEST_GEOLITE2_LICENSE_KEY")) != "" || ctx.Err() != nil {
		return
	}
	check := func() {
		if err := r.UpdateDBIP(ctx, cfg); err != nil && ctx.Err() == nil {
			r.log.Error("DB-IP update failed; failed editions retain previous databases", "error", err)
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

// UpdateDBIP refreshes each edition whose file is missing, unreadable or older than 25 days. A failed edition
// keeps its previous file and reader. Errors never contain the download URL or a response body.
func (r *Reader) UpdateDBIP(ctx context.Context, cfg DBIPConfig) error {
	r.updateMu.Lock()
	defer r.updateMu.Unlock()
	now := time.Now
	if cfg.Now != nil {
		now = cfg.Now
	}
	client := &http.Client{Timeout: 5 * time.Minute, CheckRedirect: safeRedirect}
	var failures []error
	for _, edition := range []struct{ kind, slug, target string }{{"City", "city", r.cfg.CityPath}, {"ASN", "asn", r.cfg.ASNPath}} {
		if info, err := os.Stat(edition.target); err == nil && now().Sub(info.ModTime()) < dbipMaxAge && r.loaded(edition.kind) {
			continue
		}
		if err := r.updateDBIPEdition(ctx, client, cfg.URL, edition.kind, edition.slug, edition.target, now()); err != nil {
			failures = append(failures, fmt.Errorf("DB-IP %s: %w", edition.kind, err))
		}
	}
	return errors.Join(failures...)
}

// loaded reports whether the reader for the edition holds a usable database.
func (r *Reader) loaded(kind string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if kind == "City" {
		return r.city != nil
	}
	return r.asn != nil
}

func (r *Reader) updateDBIPEdition(ctx context.Context, client *http.Client, origin, kind, slug, target string, now time.Time) error {
	var data []byte
	var lastErr error
	// The file for the current month may not be published yet on the first days of a month.
	for _, month := range []string{now.UTC().Format("2006-01"), now.UTC().AddDate(0, -1, 0).Format("2006-01")} {
		archive, err := downloadDBIP(ctx, client, origin, slug, month)
		if errors.Is(err, errDBIPNotPublished) {
			lastErr = err
			continue
		}
		if err != nil {
			return err
		}
		data, err = gunzip(archive)
		if err != nil {
			return err
		}
		break
	}
	if data == nil {
		return lastErr
	}
	db, err := decode(data, kind)
	if err != nil {
		return err
	}
	return r.install(ctx, kind, target, data, db, "DB-IP")
}

var errDBIPNotPublished = errors.New("DB-IP file is not published yet")

func downloadDBIP(ctx context.Context, client *http.Client, origin, slug, month string) ([]byte, error) {
	base, err := url.Parse(origin)
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" ||
		(base.Scheme != "https" && (base.Scheme != "http" || !isLoopbackHost(base.Hostname()))) {
		return nil, errors.New("invalid download origin (HTTPS required outside loopback)")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/free/dbip-" + slug + "-lite-" + month + ".mmdb.gz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return nil, errors.New("cannot create download request")
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, errors.New("download request failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNotFound {
		return nil, errDBIPNotPublished
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download HTTP status %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxDBIP+1))
	if err != nil {
		return nil, errors.New("cannot read download")
	}
	if len(data) > maxDBIP {
		return nil, errors.New("download exceeds size limit")
	}
	return data, nil
}

func gunzip(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, errors.New("invalid gzip archive")
	}
	defer func() { _ = gz.Close() }()
	plain, err := io.ReadAll(io.LimitReader(gz, maxDatabase+1))
	if err != nil || len(plain) > maxDatabase || len(plain) == 0 {
		return nil, errors.New("invalid or oversized decompressed database")
	}
	return plain, nil
}
