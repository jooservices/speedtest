package budget

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const testProvider = "fixture"

var (
	errBoom = errors.New("store unavailable")
	noon    = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
)

type fakeStore struct {
	mu       sync.Mutex
	counts   map[string]int
	minutes  map[string]int
	blocked  map[string]time.Time
	usedErr  error
	addErr   error
	blockErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{counts: map[string]int{}, minutes: map[string]int{}, blocked: map[string]time.Time{}}
}

func (f *fakeStore) key(provider string, day time.Time) string {
	return provider + "|" + day.UTC().Format(time.DateOnly)
}

func (f *fakeStore) Used(_ context.Context, provider string, day time.Time) (int, error) {
	if f.usedErr != nil {
		return 0, f.usedErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[f.key(provider, day)], nil
}

func (f *fakeStore) Add(_ context.Context, provider string, day time.Time, n int) (int, error) {
	if f.addErr != nil {
		return 0, f.addErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts[f.key(provider, day)] += n
	return f.counts[f.key(provider, day)], nil
}

func (f *fakeStore) Reserve(_ context.Context, provider string, day, minute, now time.Time, dailyLimit, minuteLimit int) (bool, error) {
	if f.usedErr != nil {
		return false, f.usedErr
	}
	if f.addErr != nil {
		return false, f.addErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	dayKey := f.key(provider, day)
	minuteKey := provider + "|" + minute.UTC().Format("2006-01-02T15:04")
	if blocked := f.blocked[provider]; blocked.After(now) ||
		(dailyLimit > 0 && f.counts[dayKey] >= dailyLimit) ||
		(minuteLimit > 0 && f.minutes[minuteKey] >= minuteLimit) {
		return false, nil
	}
	f.counts[dayKey]++
	f.minutes[minuteKey]++
	return true, nil
}

func (f *fakeStore) BlockUntil(_ context.Context, provider string, _, until time.Time) error {
	if f.blockErr != nil {
		return f.blockErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if until.After(f.blocked[provider]) {
		f.blocked[provider] = until
	}
	return nil
}

func TestUsageAndQuotaCooldown(t *testing.T) {
	store := newFakeStore()
	broker, err := New(map[string]Limit{testProvider: {PerDay: 10}}, store, 0.2)
	if err != nil {
		t.Fatal(err)
	}
	broker.WithClock((&fakeClock{now: noon}).read)
	if _, err := store.Add(context.Background(), testProvider, noon, 3); err != nil {
		t.Fatal(err)
	}
	if usage, err := broker.Usage(context.Background(), testProvider); err != nil || usage.RequestsToday != 3 || usage.DailyBudget != 10 {
		t.Fatalf("Usage = %+v, %v", usage, err)
	}
	if err := broker.ObserveQuota(context.Background(), testProvider, 0, 30*time.Second, 200); err != nil {
		t.Fatal(err)
	}
	if allowed, err := broker.Allow(context.Background(), testProvider, Interactive); err != nil || allowed {
		t.Fatalf("exhausted quota was allowed: (%v,%v)", allowed, err)
	}
	if err := broker.ObserveQuota(context.Background(), testProvider, 5, 0, 200); err != nil {
		t.Fatal(err)
	}
	if err := broker.ObserveQuota(context.Background(), "unknown", 0, time.Minute, 429); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("unknown quota provider error = %v", err)
	}
	store.blockErr = errBoom
	if err := broker.ObserveQuota(context.Background(), testProvider, 0, time.Minute, 429); !errors.Is(err, errBoom) {
		t.Fatalf("store cooldown error = %v", err)
	}
	store.usedErr = errBoom
	if _, err := broker.Usage(context.Background(), testProvider); !errors.Is(err, errBoom) {
		t.Fatalf("store usage error = %v", err)
	}
}

func (f *fakeStore) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	sum := 0
	for _, n := range f.counts {
		sum += n
	}
	return sum
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) read() time.Time { return c.now }

type step struct {
	at       time.Duration
	purpose  Purpose
	provider string
	before   func(*fakeStore)
}

type outcome struct {
	allowed bool
	err     error
}

func repeat(n int, o outcome) []outcome {
	out := make([]outcome, n)
	for i := range out {
		out[i] = o
	}
	return out
}

func repeatSteps(n int, s step) []step {
	out := make([]step, n)
	for i := range out {
		out[i] = s
	}
	return out
}

func TestAllowDecisions(t *testing.T) {
	yes, no := outcome{true, nil}, outcome{false, nil}
	clearFaults := func(f *fakeStore) { f.usedErr, f.addErr = nil, nil }
	cases := []struct {
		name        string
		limits      map[string]Limit
		reserve     float64
		start       time.Time
		steps       []step
		want        []outcome
		wantCounted int
	}{
		{
			name:   "minute window",
			limits: map[string]Limit{testProvider: {PerMinute: 3}},
			start:  noon,
			steps: []step{
				{at: 0}, {at: 0}, {at: 0}, {at: 10 * time.Second}, {at: 61 * time.Second},
			},
			want:        []outcome{yes, yes, yes, no, yes},
			wantCounted: 4,
		},
		{
			name:        "daily interactive",
			limits:      map[string]Limit{testProvider: {PerDay: 10}},
			reserve:     0.2,
			start:       noon,
			steps:       repeatSteps(11, step{purpose: Interactive}),
			want:        append(repeat(10, yes), no),
			wantCounted: 10,
		},
		{
			name:    "daily background reserve",
			limits:  map[string]Limit{testProvider: {PerDay: 10}},
			reserve: 0.2,
			start:   noon,
			steps: append(repeatSteps(9, step{purpose: Background}),
				step{purpose: Interactive}, step{purpose: Interactive}),
			want:        append(append(repeat(8, yes), no), yes, yes),
			wantCounted: 10,
		},
		{
			name:   "day rollover",
			limits: map[string]Limit{testProvider: {PerDay: 2}},
			start:  time.Date(2026, 11, 9, 23, 59, 0, 0, time.UTC),
			steps: []step{
				{at: 0}, {at: 0}, {at: 61 * time.Second},
			},
			want:        []outcome{yes, yes, yes},
			wantCounted: 3,
		},
		{
			name:        "unlimited",
			limits:      map[string]Limit{testProvider: {}},
			start:       noon,
			steps:       repeatSteps(1000, step{purpose: Interactive}),
			want:        repeat(1000, yes),
			wantCounted: 1000,
		},
		{
			name:   "unknown provider",
			limits: map[string]Limit{testProvider: {PerDay: 10}},
			start:  noon,
			steps:  []step{{provider: "nope"}},
			want:   []outcome{{false, ErrUnknownProvider}},
		},
		{
			name:   "store error on used counts nothing",
			limits: map[string]Limit{testProvider: {PerDay: 10}},
			start:  noon,
			steps: []step{
				{before: func(f *fakeStore) { f.usedErr = errBoom }},
				{before: clearFaults},
			},
			want:        []outcome{{false, errBoom}, yes},
			wantCounted: 1,
		},
		{
			name:   "store error on add counts nothing",
			limits: map[string]Limit{testProvider: {PerDay: 10}},
			start:  noon,
			steps: []step{
				{before: func(f *fakeStore) { f.addErr = errBoom }},
				{before: clearFaults},
			},
			want:        []outcome{{false, errBoom}, yes},
			wantCounted: 1,
		},
		{
			name:   "store error releases the minute slot",
			limits: map[string]Limit{testProvider: {PerMinute: 1, PerDay: 10}},
			start:  noon,
			steps: []step{
				{before: func(f *fakeStore) { f.usedErr = errBoom }},
				{before: clearFaults},
			},
			want:        []outcome{{false, errBoom}, yes},
			wantCounted: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			clock := &fakeClock{now: tc.start}
			broker, err := New(tc.limits, store, tc.reserve)
			if err != nil {
				t.Fatal(err)
			}
			broker.WithClock(clock.read)
			for i, s := range tc.steps {
				clock.now = tc.start.Add(s.at)
				if s.before != nil {
					s.before(store)
				}
				provider := s.provider
				if provider == "" {
					provider = testProvider
				}
				allowed, err := broker.Allow(context.Background(), provider, s.purpose)
				if allowed != tc.want[i].allowed || !errors.Is(err, tc.want[i].err) {
					t.Fatalf("call %d: got (%v, %v) want (%v, %v)", i+1, allowed, err, tc.want[i].allowed, tc.want[i].err)
				}
			}
			if got := store.total(); got != tc.wantCounted {
				t.Fatalf("counted %d requests, want %d", got, tc.wantCounted)
			}
		})
	}
}

func TestRemaining(t *testing.T) {
	cases := []struct {
		name     string
		limit    Limit
		reserve  float64
		spent    int
		purpose  Purpose
		storeErr error
		provider string
		want     int
		wantErr  error
	}{
		{name: "background after three", limit: Limit{PerDay: 10}, reserve: 0.2, spent: 3, purpose: Background, want: 5},
		{name: "interactive after three", limit: Limit{PerDay: 10}, reserve: 0.2, spent: 3, purpose: Interactive, want: 7},
		{name: "background never below zero", limit: Limit{PerDay: 10}, reserve: 0.2, spent: 9, purpose: Background, want: 0},
		{name: "unlimited", limit: Limit{}, purpose: Interactive, want: -1},
		{name: "unknown provider", provider: "nope", purpose: Interactive, wantErr: ErrUnknownProvider},
		{name: "store error", limit: Limit{PerDay: 10}, purpose: Interactive, storeErr: errBoom, wantErr: errBoom},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			broker, err := New(map[string]Limit{testProvider: tc.limit}, store, tc.reserve)
			if err != nil {
				t.Fatal(err)
			}
			broker.WithClock((&fakeClock{now: noon}).read)
			for range tc.spent {
				if _, err := broker.Allow(context.Background(), testProvider, Background); err != nil {
					t.Fatal(err)
				}
			}
			store.usedErr = tc.storeErr
			provider := tc.provider
			if provider == "" {
				provider = testProvider
			}
			got, err := broker.Remaining(context.Background(), provider, tc.purpose)
			if got != tc.want || !errors.Is(err, tc.wantErr) {
				t.Fatalf("got (%d, %v) want (%d, %v)", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestNewValidatesArguments(t *testing.T) {
	cases := []struct {
		name    string
		store   Store
		reserve float64
		limits  map[string]Limit
		wantErr bool
	}{
		{name: "valid", store: newFakeStore(), reserve: 0.2, limits: map[string]Limit{testProvider: {PerDay: 10}}},
		{name: "nil store", store: nil, reserve: 0.2, wantErr: true},
		{name: "reserve of one", store: newFakeStore(), reserve: 1.0, wantErr: true},
		{name: "negative reserve", store: newFakeStore(), reserve: -0.1, wantErr: true},
		{name: "NaN reserve", store: newFakeStore(), reserve: nanValue(), wantErr: true},
		{name: "negative per minute", store: newFakeStore(), limits: map[string]Limit{testProvider: {PerMinute: -1}}, wantErr: true},
		{name: "negative per day", store: newFakeStore(), limits: map[string]Limit{testProvider: {PerDay: -1}}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			broker, err := New(tc.limits, tc.store, tc.reserve)
			if (err != nil) != tc.wantErr {
				t.Fatalf("New error = %v, want error %v", err, tc.wantErr)
			}
			if err == nil && broker == nil {
				t.Fatal("valid configuration returned no broker")
			}
		})
	}
}

func nanValue() float64 {
	zero := 0.0
	return zero / zero
}

func TestAllowConcurrentRespectsPerMinute(t *testing.T) {
	broker, err := New(map[string]Limit{testProvider: {PerMinute: 10}}, newFakeStore(), 0)
	if err != nil {
		t.Fatal(err)
	}
	broker.WithClock((&fakeClock{now: noon}).read)
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			ok, err := broker.Allow(context.Background(), testProvider, Interactive)
			if err != nil {
				t.Error(err)
			}
			if ok {
				allowed.Add(1)
			}
		})
	}
	wg.Wait()
	if got := allowed.Load(); got != 10 {
		t.Fatalf("allowed %d concurrent calls, want 10", got)
	}
}

// fakeDB stands in for the pgx pool so the SQL wrapper is covered without a database.
type fakeDB struct {
	row      pgx.Row
	args     []any
	execArgs []any
	execErr  error
}

func (d *fakeDB) Exec(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
	d.execArgs = args
	return pgconn.CommandTag{}, d.execErr
}

func (d *fakeDB) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	d.args = args
	return d.row
}

type fakeRow struct {
	value int
	err   error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*int) = r.value
	return nil
}

func TestPostgresStoreCallsAndErrors(t *testing.T) {
	local := time.FixedZone("UTC+7", 7*60*60)
	day := time.Date(2026, 11, 9, 7, 0, 0, 0, local)
	cases := []struct {
		name    string
		call    func(context.Context, *PostgresStore) (int, error)
		row     fakeRow
		want    int
		wantErr error
	}{
		{
			name: "add returns the new total",
			call: func(ctx context.Context, s *PostgresStore) (int, error) { return s.Add(ctx, testProvider, day, 1) },
			row:  fakeRow{value: 7},
			want: 7,
		},
		{
			name:    "add wraps a store error",
			call:    func(ctx context.Context, s *PostgresStore) (int, error) { return s.Add(ctx, testProvider, day, 1) },
			row:     fakeRow{err: errBoom},
			wantErr: errBoom,
		},
		{
			name: "used returns the stored count",
			call: func(ctx context.Context, s *PostgresStore) (int, error) { return s.Used(ctx, testProvider, day) },
			row:  fakeRow{value: 4},
			want: 4,
		},
		{
			name:    "used wraps a store error",
			call:    func(ctx context.Context, s *PostgresStore) (int, error) { return s.Used(ctx, testProvider, day) },
			row:     fakeRow{err: errBoom},
			wantErr: errBoom,
		},
		{
			name: "reserve allows and counts a request",
			call: func(ctx context.Context, s *PostgresStore) (int, error) {
				allowed, err := s.Reserve(ctx, testProvider, day, day, day, 10, 4)
				if allowed {
					return 1, err
				}
				return 0, err
			},
			row:  fakeRow{value: 7},
			want: 1,
		},
		{
			name: "reserve at a limit is denied without error",
			call: func(ctx context.Context, s *PostgresStore) (int, error) {
				allowed, err := s.Reserve(ctx, testProvider, day, day, day, 10, 4)
				if allowed {
					return 1, err
				}
				return 0, err
			},
			row: fakeRow{err: pgx.ErrNoRows},
		},
		{
			name: "reserve wraps a store error",
			call: func(ctx context.Context, s *PostgresStore) (int, error) {
				allowed, err := s.Reserve(ctx, testProvider, day, day, day, 10, 4)
				if allowed {
					return 1, err
				}
				return 0, err
			},
			row: fakeRow{err: errBoom}, wantErr: errBoom,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := &fakeDB{row: tc.row}
			got, err := tc.call(context.Background(), NewPostgresStore(db))
			if got != tc.want || !errors.Is(err, tc.wantErr) {
				t.Fatalf("got (%d, %v) want (%d, %v)", got, err, tc.want, tc.wantErr)
			}
			if db.args[0] != testProvider || !db.args[1].(time.Time).Equal(day) || db.args[1].(time.Time).Location() != time.UTC {
				t.Fatalf("query arguments %v: the day must reach PostgreSQL as UTC", db.args)
			}
		})
	}
	t.Run("provider cooldown is persisted in UTC", func(t *testing.T) {
		db := &fakeDB{}
		until := day.Add(time.Minute)
		if err := NewPostgresStore(db).BlockUntil(context.Background(), testProvider, day, until); err != nil {
			t.Fatal(err)
		}
		if db.execArgs[0] != testProvider || db.execArgs[1].(time.Time).Location() != time.UTC || !db.execArgs[2].(time.Time).Equal(until) {
			t.Fatalf("cooldown query arguments %v", db.execArgs)
		}
	})
	t.Run("provider cooldown wraps a store error", func(t *testing.T) {
		db := &fakeDB{execErr: errBoom}
		if err := NewPostgresStore(db).BlockUntil(context.Background(), testProvider, day, day.Add(time.Minute)); !errors.Is(err, errBoom) {
			t.Fatalf("cooldown store error = %v", err)
		}
	})
}
