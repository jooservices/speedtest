package geo

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/oschwald/maxminddb-golang"
)

const ipInfoUpdateInterval = 24 * time.Hour

// UpdateIPInfo downloads, verifies and atomically installs IPinfo Lite MMDB data.
func (r *Reader) UpdateIPInfo(ctx context.Context) error {
	r.updateMu.Lock()
	defer r.updateMu.Unlock()
	if strings.TrimSpace(r.cfg.IPInfoToken) == "" {
		return nil
	}
	data, err := r.downloadIPInfo(ctx)
	if err != nil {
		return err
	}
	db, err := decodeIPInfo(data)
	if err != nil {
		return err
	}
	return r.installIPInfo(ctx, data, db)
}

func (r *Reader) downloadIPInfo(ctx context.Context) ([]byte, error) {
	u, err := url.Parse(r.cfg.IPInfoURL)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("invalid IPinfo download origin")
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !isLoopbackHost(u.Hostname())) {
		return nil, errors.New("invalid IPinfo download origin")
	}
	query := u.Query()
	query.Set("token", r.cfg.IPInfoToken)
	u.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("cannot create IPinfo download request")
	}
	client := &http.Client{Timeout: 2 * time.Minute, CheckRedirect: ipInfoRedirect(u)}
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("IPinfo download request failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("IPinfo download returned a non-success status")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxDatabase+1))
	if err != nil || len(data) > maxDatabase {
		return nil, errors.New("IPinfo database is unreadable or oversized")
	}
	return data, nil
}

func ipInfoRedirect(origin *url.URL) func(*http.Request, []*http.Request) error {
	return func(request *http.Request, via []*http.Request) error {
		if len(via) >= 3 || len(via) == 0 || request.URL.Scheme != "https" {
			return errors.New("unsafe IPinfo database redirect")
		}
		request.Header.Del("Authorization")
		if !strings.EqualFold(request.URL.Host, origin.Host) && request.URL.Query().Has("token") {
			return errors.New("IPinfo database token cannot be forwarded to another host")
		}
		return nil
	}
}

func decodeIPInfo(data []byte) (*maxminddb.Reader, error) {
	db, err := maxminddb.FromBytes(data)
	if err != nil {
		return nil, errors.New("invalid IPinfo database")
	}
	typeName := strings.ToLower(db.Metadata.DatabaseType)
	if !strings.Contains(typeName, "ipinfo") || !strings.Contains(typeName, "lite") || db.Verify() != nil {
		_ = db.Close()
		return nil, errors.New("unexpected or invalid IPinfo Lite database")
	}
	return db, nil
}

func (r *Reader) installIPInfo(ctx context.Context, data []byte, db *maxminddb.Reader) error {
	if err := ctx.Err(); err != nil {
		_ = db.Close()
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := replace(r.cfg.IPInfoPath, data); err != nil {
		_ = db.Close()
		return err
	}
	old := r.info
	r.info = db
	if old != nil {
		_ = old.Close()
	}
	r.log.Info("IPinfo Lite offline database updated")
	return nil
}

// RunIPInfo checks once then refreshes the offline database daily until canceled.
func (r *Reader) RunIPInfo(ctx context.Context) {
	if ctx.Err() != nil || strings.TrimSpace(r.cfg.IPInfoToken) == "" {
		return
	}
	ticker := time.NewTicker(ipInfoUpdateInterval)
	defer ticker.Stop()
	runIPInfoUpdates(ctx, ticker.C, r.UpdateIPInfo, r.log)
}

func runIPInfoUpdates(ctx context.Context, ticks <-chan time.Time, update func(context.Context) error, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	if err := update(ctx); err != nil && ctx.Err() == nil {
		logger.Error("IPinfo Lite database update failed; previous database retained", "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			if err := update(ctx); err != nil && ctx.Err() == nil {
				logger.Error("IPinfo Lite database update failed; previous database retained", "error", err)
			}
		}
	}
}
