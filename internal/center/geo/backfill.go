package geo

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jooservices/speedtest/internal/center/geo/budget"
)

const maxBackfillBatch = 100

// BackfillDB is the PostgreSQL boundary used by the resumable nightly refresh.
type BackfillDB interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// BatchLookup performs one provider request for up to one hundred addresses.
type BatchLookup interface {
	LookupBatch(context.Context, []string, budget.Purpose) (map[string]Location, error)
}

// Backfill refreshes low-quality cache rows in bounded, resumable batches.
type Backfill struct {
	DB    BackfillDB
	Cache interface {
		Merge(context.Context, string, Location, string) (Location, string, error)
	}
	Resolver   *Resolver
	Batch      BatchLookup
	MaxPerRun  int
	BatchSize  int
	Pause      time.Duration
	HourUTC    int
	Now        func() time.Time
	Logger     *slog.Logger
	ObserveRun func(bool)
}

// BackfillConfig controls the nightly work budget; batch sizes cannot exceed ip-api's documented limit.
type BackfillConfig struct {
	MaxPerRun int
	BatchSize int
	Pause     time.Duration
	HourUTC   int
}

// BackfillConfigFromEnv reads the bounded nightly schedule settings.
func BackfillConfigFromEnv() (BackfillConfig, error) {
	cfg := BackfillConfig{MaxPerRun: 100, BatchSize: 100, Pause: 10 * time.Second, HourUTC: 2}
	var err error
	if cfg.MaxPerRun, err = readEnvInt("SPEEDTEST_GEO_BACKFILL_MAX_PER_NIGHT", cfg.MaxPerRun); err != nil || cfg.MaxPerRun < 1 || cfg.MaxPerRun > 10000 {
		return BackfillConfig{}, errors.New("SPEEDTEST_GEO_BACKFILL_MAX_PER_NIGHT must be between 1 and 10000")
	}
	if cfg.BatchSize, err = readEnvInt("SPEEDTEST_GEO_BACKFILL_BATCH_SIZE", cfg.BatchSize); err != nil || cfg.BatchSize < 1 || cfg.BatchSize > maxBackfillBatch {
		return BackfillConfig{}, errors.New("SPEEDTEST_GEO_BACKFILL_BATCH_SIZE must be between 1 and 100")
	}
	if raw := os.Getenv("SPEEDTEST_GEO_BACKFILL_PAUSE"); raw != "" {
		cfg.Pause, err = time.ParseDuration(raw)
		if err != nil || cfg.Pause < 0 || cfg.Pause > time.Hour {
			return BackfillConfig{}, errors.New("SPEEDTEST_GEO_BACKFILL_PAUSE must be a duration from 0 to 1h")
		}
	}
	if cfg.HourUTC, err = readEnvInt("SPEEDTEST_GEO_BACKFILL_HOUR_UTC", cfg.HourUTC); err != nil || cfg.HourUTC < 0 || cfg.HourUTC > 23 {
		return BackfillConfig{}, errors.New("SPEEDTEST_GEO_BACKFILL_HOUR_UTC must be between 0 and 23")
	}
	return cfg, nil
}

func readEnvInt(key string, fallback int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	return value, err
}

// RunOnce refreshes at most MaxPerRun weak entries and persists its cursor after each completed batch.
func (b *Backfill) RunOnce(ctx context.Context) (int, error) {
	if b.DB == nil || b.Cache == nil || (b.Batch == nil && b.Resolver == nil) {
		return 0, errors.New("geolocation backfill dependencies are required")
	}
	now := b.now()
	maxItems := b.MaxPerRun
	if maxItems <= 0 {
		maxItems = 100
	}
	batchSize := b.BatchSize
	if batchSize <= 0 || batchSize > maxBackfillBatch {
		batchSize = maxBackfillBatch
	}
	cursor, err := b.loadCursor(ctx, now)
	if err != nil {
		return 0, err
	}
	processed := 0
	for processed < maxItems {
		limit := min(batchSize, maxItems-processed)
		addresses, err := b.candidates(ctx, cursor, limit)
		if err != nil || len(addresses) == 0 {
			return processed, err
		}
		if b.Batch != nil {
			if err := b.refreshBatch(ctx, addresses); err != nil {
				if errors.Is(err, ErrBudgetExhausted) {
					return processed, nil
				}
				return processed, err
			}
		} else if err := b.refreshIndividually(ctx, addresses); err != nil {
			if errors.Is(err, ErrBudgetExhausted) {
				return processed, nil
			}
			return processed, err
		}
		cursor = addresses[len(addresses)-1]
		if err := b.saveCursor(ctx, now, cursor); err != nil {
			return processed, err
		}
		processed += len(addresses)
		if processed < maxItems && b.Pause > 0 {
			timer := time.NewTimer(b.Pause)
			select {
			case <-ctx.Done():
				timer.Stop()
				return processed, ctx.Err()
			case <-timer.C:
			}
		}
	}
	return processed, nil
}

// Loop waits for the next configured UTC hour and runs one bounded nightly pass.
func (b *Backfill) Loop(ctx context.Context) {
	logger := b.Logger
	if logger == nil {
		logger = slog.Default()
	}
	for {
		now := b.now()
		timer := time.NewTimer(nextBackfillAt(now, b.HourUTC).Sub(now))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		_, err := b.RunOnce(ctx)
		ok := err == nil || errors.Is(err, ErrBudgetExhausted)
		if b.ObserveRun != nil {
			b.ObserveRun(ok)
		}
		if err != nil && !errors.Is(err, ErrBudgetExhausted) && ctx.Err() == nil {
			logger.Error("geolocation backfill failed; cursor retained", "error", err)
		}
	}
}

func (b *Backfill) now() time.Time {
	if b.Now != nil {
		return b.Now().UTC()
	}
	return time.Now().UTC()
}

func nextBackfillAt(now time.Time, hour int) time.Time {
	utc := now.UTC()
	next := time.Date(utc.Year(), utc.Month(), utc.Day(), hour, 0, 0, 0, time.UTC)
	if next.Before(utc) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

func (b *Backfill) loadCursor(ctx context.Context, now time.Time) (string, error) {
	var day time.Time
	var cursor *string
	err := b.DB.QueryRow(ctx, `SELECT run_day,host(cursor) FROM geo_backfill_state WHERE singleton=true`).Scan(&day, &cursor)
	if errors.Is(err, pgx.ErrNoRows) {
		_, err = b.DB.Exec(ctx, `INSERT INTO geo_backfill_state(singleton,run_day) VALUES(true,$1::date) ON CONFLICT(singleton) DO NOTHING`, now)
		return "", err
	}
	if err != nil {
		return "", err
	}
	if !sameUTCDate(day, now) {
		_, err = b.DB.Exec(ctx, `UPDATE geo_backfill_state SET run_day=$1::date,cursor=NULL,updated_at=now() WHERE singleton=true`, now)
		return "", err
	}
	if cursor == nil {
		return "", nil
	}
	return *cursor, nil
}

func (b *Backfill) candidates(ctx context.Context, cursor string, limit int) ([]string, error) {
	rows, err := b.DB.Query(ctx, `SELECT host(ip) FROM ip_addresses
WHERE looked_up_at IS NOT NULL
AND (CASE WHEN geo->>'quality_score' ~ '^[0-9]+$' THEN (geo->>'quality_score')::integer ELSE 0 END
 - LEAST(GREATEST(floor(extract(epoch FROM (now()-looked_up_at))/1555200)::integer,0),10)) < $1
AND ($2::inet IS NULL OR ip>$2::inet) ORDER BY ip LIMIT $3`, QualityThreshold, nullableCursor(cursor), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	addresses := make([]string, 0, limit)
	for rows.Next() {
		var ip string
		if err := rows.Scan(&ip); err != nil {
			return nil, err
		}
		addresses = append(addresses, ip)
	}
	return addresses, rows.Err()
}

func (b *Backfill) refreshBatch(ctx context.Context, addresses []string) error {
	locations, err := b.Batch.LookupBatch(ctx, addresses, budget.Background)
	if err != nil {
		return err
	}
	for _, ip := range addresses {
		if location, ok := locations[ip]; ok && !emptyLocation(location) {
			if _, _, err := b.Cache.Merge(ctx, ip, location, "ip-api"); err != nil {
				return err
			}
		}
		if b.Resolver != nil {
			_, err := b.Resolver.Lookup(ctx, ip)
			if errors.Is(err, ErrBudgetExhausted) {
				return err
			}
			if err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
		}
	}
	return nil
}

func (b *Backfill) refreshIndividually(ctx context.Context, addresses []string) error {
	for _, ip := range addresses {
		_, err := b.Resolver.Lookup(ctx, ip)
		if err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrBudgetExhausted) {
			return err
		}
		if errors.Is(err, ErrBudgetExhausted) {
			return err
		}
	}
	return nil
}

func (b *Backfill) saveCursor(ctx context.Context, now time.Time, cursor string) error {
	_, err := b.DB.Exec(ctx, `INSERT INTO geo_backfill_state(singleton,run_day,cursor,updated_at)
VALUES(true,$1::date,$2::inet,now()) ON CONFLICT(singleton) DO UPDATE SET run_day=EXCLUDED.run_day,cursor=EXCLUDED.cursor,updated_at=now()`, now, cursor)
	return err
}

func nullableCursor(cursor string) any {
	if cursor == "" {
		return nil
	}
	return cursor
}

func sameUTCDate(first, second time.Time) bool {
	a, b := first.UTC(), second.UTC()
	return a.Year() == b.Year() && a.YearDay() == b.YearDay()
}
