package geo

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jooservices/speedtest/internal/center/geo/budget"
)

type backfillMemoryDB struct {
	day       time.Time
	cursor    string
	addresses []string
	queryErr  error
	rowErr    error
	execErr   error
	scanErr   error
	rowsErr   error
}

func (d *backfillMemoryDB) Query(_ context.Context, _ string, args ...any) (pgx.Rows, error) {
	if d.queryErr != nil {
		return nil, d.queryErr
	}
	cursor, _ := args[1].(string)
	limit := args[2].(int)
	rows := &backfillMemoryRows{scanErr: d.scanErr, rowsErr: d.rowsErr}
	for _, address := range d.addresses {
		if address > cursor && len(rows.values) < limit {
			rows.values = append(rows.values, address)
		}
	}
	return rows, nil
}

func (d *backfillMemoryDB) QueryRow(context.Context, string, ...any) pgx.Row {
	if d.rowErr != nil {
		return backfillMemoryRow{err: d.rowErr}
	}
	if d.day.IsZero() {
		return backfillMemoryRow{err: pgx.ErrNoRows}
	}
	return backfillMemoryRow{day: d.day, cursor: d.cursor}
}

func (d *backfillMemoryDB) Exec(_ context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	if d.execErr != nil {
		return pgconn.CommandTag{}, d.execErr
	}
	d.day = args[0].(time.Time)
	switch {
	case strings.Contains(query, "cursor,updated_at"):
		d.cursor = args[1].(string)
	case strings.Contains(query, "cursor=NULL"):
		d.cursor = ""
	}
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

type backfillMemoryRow struct {
	day    time.Time
	cursor string
	err    error
}

func (r backfillMemoryRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*time.Time) = r.day
	if r.cursor != "" {
		value := r.cursor
		*dest[1].(**string) = &value
	}
	return nil
}

type backfillMemoryRows struct {
	pgx.Rows
	values  []string
	index   int
	scanErr error
	rowsErr error
}

func (r *backfillMemoryRows) Next() bool {
	return r.index < len(r.values)
}

func (r *backfillMemoryRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	*dest[0].(*string) = r.values[r.index]
	r.index++
	return nil
}

func (*backfillMemoryRows) Close()                                       {}
func (r *backfillMemoryRows) Err() error                                 { return r.rowsErr }
func (*backfillMemoryRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (*backfillMemoryRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (*backfillMemoryRows) Values() ([]any, error)                       { return nil, nil }
func (*backfillMemoryRows) RawValues() [][]byte                          { return nil }
func (*backfillMemoryRows) Conn() *pgx.Conn                              { return nil }

type backfillMemoryCache struct {
	merged []string
	err    error
}

func (c *backfillMemoryCache) Merge(_ context.Context, ip string, location Location, _ string) (Location, string, error) {
	if c.err != nil {
		return Location{}, "", c.err
	}
	c.merged = append(c.merged, ip)
	return location, "ip-api", nil
}

type backfillMemoryBatch struct {
	calls int
	err   error
	empty bool
}

func (b *backfillMemoryBatch) LookupBatch(_ context.Context, ips []string, _ budget.Purpose) (map[string]Location, error) {
	b.calls++
	if b.err != nil {
		return nil, b.err
	}
	locations := make(map[string]Location, len(ips))
	if b.empty {
		return locations, nil
	}
	for _, ip := range ips {
		locations[ip] = Location{City: "London", Country: "GB"}
	}
	return locations, nil
}

type backfillMemoryFallback struct{ err error }

func (backfillMemoryFallback) Source() string { return "ipwho" }
func (f backfillMemoryFallback) Lookup(context.Context, string) (Location, error) {
	return Location{Country: "GB"}, f.err
}

func TestBackfillRunOncePersistsCursorAndStopsAtLimit(t *testing.T) {
	db := &backfillMemoryDB{addresses: []string{"1.1.1.1", "8.8.8.8"}}
	cache, batch := &backfillMemoryCache{}, &backfillMemoryBatch{}
	now := time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC)
	job := &Backfill{DB: db, Cache: cache, Batch: batch, MaxPerRun: 1, BatchSize: 1, Now: func() time.Time { return now }}
	for _, want := range []int{1, 1, 0} {
		processed, err := job.RunOnce(context.Background())
		if err != nil || processed != want {
			t.Fatalf("RunOnce = (%d,%v), want (%d,nil)", processed, err, want)
		}
	}
	if db.cursor != "8.8.8.8" || batch.calls != 2 || len(cache.merged) != 2 {
		t.Fatalf("cursor=%q batches=%d merged=%v", db.cursor, batch.calls, cache.merged)
	}
}

func TestBackfillRunOnceRetainsCursorOnErrorsAndExhaustion(t *testing.T) {
	ctx := context.Background()
	address := []string{"1.1.1.1"}
	for _, tc := range []struct {
		name      string
		batchErr  error
		cacheErr  error
		queryErr  error
		wantErr   error
		wantCount int
	}{
		{name: "provider error", batchErr: errors.New("provider"), wantErr: errors.New("provider")},
		{name: "cache error", cacheErr: errors.New("cache"), wantErr: errors.New("cache")},
		{name: "query error", queryErr: errors.New("query"), wantErr: errors.New("query")},
		{name: "budget exhausted", batchErr: ErrBudgetExhausted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := &backfillMemoryDB{addresses: address, queryErr: tc.queryErr}
			batch, cache := &backfillMemoryBatch{err: tc.batchErr}, &backfillMemoryCache{err: tc.cacheErr}
			job := &Backfill{DB: db, Cache: cache, Batch: batch, MaxPerRun: 1, Now: func() time.Time { return time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC) }}
			count, err := job.RunOnce(ctx)
			if (err == nil) != (tc.wantErr == nil) || (err != nil && !strings.Contains(err.Error(), tc.wantErr.Error())) || count != tc.wantCount || db.cursor != "" {
				t.Fatalf("RunOnce = (%d,%v), cursor=%q", count, err, db.cursor)
			}
		})
	}
}

func TestBackfillRunOnceUsesResolverWithoutBatch(t *testing.T) {
	db := &backfillMemoryDB{addresses: []string{"1.1.1.1"}}
	cache := &backfillMemoryCache{}
	resolver := NewResolver(nil, nil, backfillMemoryFallback{}).WithPurpose(budget.Background)
	job := &Backfill{DB: db, Cache: cache, Resolver: resolver, MaxPerRun: 1, Now: func() time.Time { return time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC) }}
	if count, err := job.RunOnce(context.Background()); err != nil || count != 1 || db.cursor != "1.1.1.1" {
		t.Fatalf("RunOnce = (%d,%v), cursor=%q", count, err, db.cursor)
	}
}

func TestBackfillLoopCancellationAndObservedPass(t *testing.T) {
	now := time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC)
	db := &backfillMemoryDB{}
	ctx, cancel := context.WithCancel(context.Background())
	var observed bool
	clockCalls := 0
	job := &Backfill{DB: db, Cache: &backfillMemoryCache{}, Batch: &backfillMemoryBatch{}, HourUTC: 2,
		Now: func() time.Time {
			clockCalls++
			if clockCalls == 1 {
				return now
			}
			return now.Add(time.Second)
		},
		ObserveRun: func(ok bool) { observed = ok; cancel() }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	job.Loop(ctx)
	if !observed {
		t.Fatal("backfill pass result was not observed")
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	job.Loop(ctx)
}

func TestBackfillRunOnceValidatesDependenciesAndDatabaseFailures(t *testing.T) {
	if count, err := (&Backfill{}).RunOnce(context.Background()); count != 0 || err == nil {
		t.Fatalf("missing dependencies = (%d,%v)", count, err)
	}
	now := time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		db   *backfillMemoryDB
	}{
		{name: "cursor read", db: &backfillMemoryDB{day: now, rowErr: errors.New("read cursor")}},
		{name: "cursor create", db: &backfillMemoryDB{execErr: errors.New("create cursor")}},
		{name: "candidate scan", db: &backfillMemoryDB{day: now, addresses: []string{"1.1.1.1"}, scanErr: errors.New("scan candidate")}},
		{name: "candidate iteration", db: &backfillMemoryDB{day: now, addresses: []string{"1.1.1.1"}, rowsErr: errors.New("iterate candidates")}},
		{name: "cursor save", db: &backfillMemoryDB{day: now, addresses: []string{"1.1.1.1"}, execErr: errors.New("save cursor")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := &Backfill{DB: tc.db, Cache: &backfillMemoryCache{}, Batch: &backfillMemoryBatch{}, Now: func() time.Time { return now }}
			if count, err := job.RunOnce(context.Background()); count != 0 || err == nil {
				t.Fatalf("RunOnce = (%d,%v), want an error before cursor progress", count, err)
			}
		})
	}
}

func TestBackfillRunOnceResetsUTCDateAndUsesDefaults(t *testing.T) {
	now := time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC)
	db := &backfillMemoryDB{day: now.Add(-24 * time.Hour), cursor: "9.9.9.9", addresses: []string{"1.1.1.1"}}
	job := &Backfill{DB: db, Cache: &backfillMemoryCache{}, Batch: &backfillMemoryBatch{}, BatchSize: 101, Now: func() time.Time { return now }}
	if count, err := job.RunOnce(context.Background()); err != nil || count != 1 || db.cursor != "1.1.1.1" {
		t.Fatalf("daily cursor reset = (%d,%v), cursor=%q", count, err, db.cursor)
	}
	db.addresses = []string{"8.8.8.8"}
	job.MaxPerRun, job.BatchSize = 0, 0
	db.day = time.Now().UTC()
	job.Now = nil
	if count, err := job.RunOnce(context.Background()); err != nil || count != 1 {
		t.Fatalf("default limits and clock = (%d,%v)", count, err)
	}
}

func TestBackfillRefreshBranchesAndPauseCancellation(t *testing.T) {
	ctx := context.Background()
	addresses := []string{"1.1.1.1"}
	db := &backfillMemoryDB{addresses: addresses}
	cache := &backfillMemoryCache{}
	batch := &backfillMemoryBatch{empty: true}
	job := &Backfill{DB: db, Cache: cache, Batch: batch, MaxPerRun: 1, Now: func() time.Time { return time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC) }}
	if count, err := job.RunOnce(ctx); err != nil || count != 1 || len(cache.merged) != 0 {
		t.Fatalf("empty batch results = (%d,%v), merged=%v", count, err, cache.merged)
	}
	for _, lookupErr := range []error{ErrNotFound, ErrBudgetExhausted, context.DeadlineExceeded} {
		db = &backfillMemoryDB{addresses: addresses}
		resolver := NewResolver(nil, nil, backfillMemoryFallback{err: lookupErr}).WithPurpose(budget.Background)
		job = &Backfill{DB: db, Cache: &backfillMemoryCache{}, Batch: &backfillMemoryBatch{empty: true}, Resolver: resolver, MaxPerRun: 1,
			Now: func() time.Time { return time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC) }}
		count, err := job.RunOnce(ctx)
		if errors.Is(lookupErr, ErrNotFound) && (err != nil || count != 1) {
			t.Fatalf("not found should advance: (%d,%v)", count, err)
		}
		if errors.Is(lookupErr, ErrBudgetExhausted) && (err != nil || count != 0) {
			t.Fatalf("budget exhaustion should stop safely: (%d,%v)", count, err)
		}
		if errors.Is(lookupErr, context.DeadlineExceeded) && (err == nil || count != 0) {
			t.Fatalf("resolver failure should retain cursor: (%d,%v)", count, err)
		}
	}
	for _, lookupErr := range []error{ErrNotFound, ErrBudgetExhausted, context.DeadlineExceeded} {
		db = &backfillMemoryDB{addresses: addresses}
		job = &Backfill{DB: db, Cache: &backfillMemoryCache{}, Resolver: NewResolver(nil, nil, backfillMemoryFallback{err: lookupErr}), MaxPerRun: 1,
			Now: func() time.Time { return time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC) }}
		count, err := job.RunOnce(ctx)
		if errors.Is(lookupErr, ErrNotFound) && (err != nil || count != 1) {
			t.Fatalf("individual not found = (%d,%v)", count, err)
		}
		if errors.Is(lookupErr, ErrBudgetExhausted) && (err != nil || count != 0) {
			t.Fatalf("individual budget exhaustion = (%d,%v)", count, err)
		}
		if errors.Is(lookupErr, context.DeadlineExceeded) && (err == nil || count != 0) {
			t.Fatalf("individual resolver failure = (%d,%v)", count, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db = &backfillMemoryDB{addresses: []string{"1.1.1.1", "8.8.8.8"}}
	job = &Backfill{DB: db, Cache: &backfillMemoryCache{}, Batch: &backfillMemoryBatch{}, MaxPerRun: 2, BatchSize: 1, Pause: time.Second,
		Now: func() time.Time { return time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC) }}
	if count, err := job.RunOnce(ctx); count != 1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled pause = (%d,%v)", count, err)
	}
}
