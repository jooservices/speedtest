package report

import (
	"errors"
	"testing"
	"time"
)

func TestAC1NewPeriodBuildsOwnerLocalDayAndWeekBounds(t *testing.T) {
	tests := []struct {
		name     string
		kind     PeriodKind
		date     string
		zone     string
		from     time.Time
		to       time.Time
		wantSpan time.Duration
	}{
		{
			name:     "regular daily period",
			kind:     DailyPeriod,
			date:     "2026-03-10",
			zone:     "Asia/Ho_Chi_Minh",
			from:     time.Date(2026, 3, 9, 17, 0, 0, 0, time.UTC),
			to:       time.Date(2026, 3, 10, 17, 0, 0, 0, time.UTC),
			wantSpan: 24 * time.Hour,
		},
		{
			name:     "daily spring-forward day",
			kind:     DailyPeriod,
			date:     "2026-03-08",
			zone:     "America/New_York",
			from:     time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC),
			to:       time.Date(2026, 3, 9, 4, 0, 0, 0, time.UTC),
			wantSpan: 23 * time.Hour,
		},
		{
			name:     "daily fall-back day",
			kind:     DailyPeriod,
			date:     "2026-11-01",
			zone:     "America/New_York",
			from:     time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC),
			to:       time.Date(2026, 11, 2, 5, 0, 0, 0, time.UTC),
			wantSpan: 25 * time.Hour,
		},
		{
			name:     "daily midnight DST gap",
			kind:     DailyPeriod,
			date:     "2018-11-04",
			zone:     "America/Sao_Paulo",
			from:     time.Date(2018, 11, 4, 3, 0, 0, 0, time.UTC),
			to:       time.Date(2018, 11, 5, 2, 0, 0, 0, time.UTC),
			wantSpan: 23 * time.Hour,
		},
		{
			name:     "daily boundary skips a missing local date",
			kind:     DailyPeriod,
			date:     "2011-12-29",
			zone:     "Pacific/Apia",
			from:     time.Date(2011, 12, 29, 10, 0, 0, 0, time.UTC),
			to:       time.Date(2011, 12, 30, 10, 0, 0, 0, time.UTC),
			wantSpan: 24 * time.Hour,
		},
		{
			name:     "weekly spring-forward week",
			kind:     WeeklyPeriod,
			date:     "2026-03-02",
			zone:     "America/New_York",
			from:     time.Date(2026, 3, 2, 5, 0, 0, 0, time.UTC),
			to:       time.Date(2026, 3, 9, 4, 0, 0, 0, time.UTC),
			wantSpan: 167 * time.Hour,
		},
		{
			name:     "weekly fall-back week",
			kind:     WeeklyPeriod,
			date:     "2026-10-26",
			zone:     "America/New_York",
			from:     time.Date(2026, 10, 26, 4, 0, 0, 0, time.UTC),
			to:       time.Date(2026, 11, 2, 5, 0, 0, 0, time.UTC),
			wantSpan: 169 * time.Hour,
		},
		{
			name:     "regular UTC week",
			kind:     WeeklyPeriod,
			date:     "2026-03-02",
			zone:     "UTC",
			from:     time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC),
			to:       time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC),
			wantSpan: 7 * 24 * time.Hour,
		},
		{
			name:     "daily period across stale Go zone bound",
			kind:     DailyPeriod,
			date:     "2040-12-31",
			zone:     "Europe/Warsaw",
			from:     time.Date(2040, 12, 30, 23, 0, 0, 0, time.UTC),
			to:       time.Date(2040, 12, 31, 23, 0, 0, 0, time.UTC),
			wantSpan: 24 * time.Hour,
		},
		{
			name:     "weekly period across stale Go zone bound",
			kind:     WeeklyPeriod,
			date:     "2040-12-31",
			zone:     "Europe/Warsaw",
			from:     time.Date(2040, 12, 30, 23, 0, 0, 0, time.UTC),
			to:       time.Date(2041, 1, 6, 23, 0, 0, 0, time.UTC),
			wantSpan: 7 * 24 * time.Hour,
		},
		{
			name:     "November sixth with a repeated local segment",
			kind:     DailyPeriod,
			date:     "2010-11-06",
			zone:     "America/St_Johns",
			from:     time.Date(2010, 11, 6, 2, 30, 0, 0, time.UTC),
			to:       time.Date(2010, 11, 7, 3, 30, 0, 0, time.UTC),
			wantSpan: 25 * time.Hour,
		},
		{
			name:     "November seventh with an early and repeated date segment",
			kind:     DailyPeriod,
			date:     "2010-11-07",
			zone:     "America/St_Johns",
			from:     time.Date(2010, 11, 7, 2, 30, 0, 0, time.UTC),
			to:       time.Date(2010, 11, 8, 3, 30, 0, 0, time.UTC),
			wantSpan: 25 * time.Hour,
		},
		{
			name:     "weekly period spanning a midnight rollback",
			kind:     WeeklyPeriod,
			date:     "2010-11-01",
			zone:     "America/St_Johns",
			from:     time.Date(2010, 11, 1, 2, 30, 0, 0, time.UTC),
			to:       time.Date(2010, 11, 8, 3, 30, 0, 0, time.UTC),
			wantSpan: 169 * time.Hour,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			period, err := NewPeriod(tc.kind, tc.date, tc.zone)
			if err != nil {
				t.Fatalf("NewPeriod() error = %v", err)
			}
			assertPeriodBounds(t, period, tc.from, tc.to)
			assertPeriodDuration(t, period, tc.wantSpan)
			assertPeriodUTC(t, period)
			assertPeriodIdentity(t, period, tc.kind, tc.date, tc.zone)
		})
	}
}

func TestAC1NewPeriodRecordsDisjointOwnerLocalDateIntervals(t *testing.T) {
	tests := []struct {
		date string
		want []utcInterval
	}{
		{
			date: "2010-11-06",
			want: []utcInterval{
				{from: time.Date(2010, 11, 6, 2, 30, 0, 0, time.UTC), to: time.Date(2010, 11, 7, 2, 30, 0, 0, time.UTC)},
				{from: time.Date(2010, 11, 7, 2, 31, 0, 0, time.UTC), to: time.Date(2010, 11, 7, 3, 30, 0, 0, time.UTC)},
			},
		},
		{
			date: "2010-11-07",
			want: []utcInterval{
				{from: time.Date(2010, 11, 7, 2, 30, 0, 0, time.UTC), to: time.Date(2010, 11, 7, 2, 31, 0, 0, time.UTC)},
				{from: time.Date(2010, 11, 7, 3, 30, 0, 0, time.UTC), to: time.Date(2010, 11, 8, 3, 30, 0, 0, time.UTC)},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.date, func(t *testing.T) {
			period, err := NewPeriod(DailyPeriod, tc.date, "America/St_Johns")
			if err != nil {
				t.Fatal(err)
			}
			if len(period.intervals) != len(tc.want) {
				t.Fatalf("UTC interval count = %d, want %d: %+v", len(period.intervals), len(tc.want), period.intervals)
			}
			for index, want := range tc.want {
				if !period.intervals[index].from.Equal(want.from) || !period.intervals[index].to.Equal(want.to) {
					t.Errorf("UTC interval %d = [%s, %s), want [%s, %s)", index, period.intervals[index].from, period.intervals[index].to, want.from, want.to)
				}
			}
		})
	}
}

func assertPeriodBounds(t *testing.T, got Period, from, to time.Time) {
	t.Helper()
	if !got.From().Equal(from) {
		t.Errorf("inclusive UTC start = %s, want %s", got.From(), from)
	}
	if !got.To().Equal(to) {
		t.Errorf("exclusive UTC end = %s, want %s", got.To(), to)
	}
}

func assertPeriodDuration(t *testing.T, got Period, want time.Duration) {
	t.Helper()
	if duration := got.To().Sub(got.From()); duration != want {
		t.Errorf("period duration = %s, want %s", duration, want)
	}
}

func assertPeriodUTC(t *testing.T, got Period) {
	t.Helper()
	if got.From().Location() != time.UTC {
		t.Errorf("period start location = %s, want UTC", got.From().Location())
	}
	if got.To().Location() != time.UTC {
		t.Errorf("period end location = %s, want UTC", got.To().Location())
	}
}

func assertPeriodIdentity(t *testing.T, got Period, kind PeriodKind, date, timezone string) {
	t.Helper()
	if got.Kind() != kind {
		t.Errorf("period kind = %q, want %q", got.Kind(), kind)
	}
	if got.LocalDate() != date {
		t.Errorf("period local date = %q, want %q", got.LocalDate(), date)
	}
	if got.Timezone() != timezone {
		t.Errorf("period time zone = %q, want %q", got.Timezone(), timezone)
	}
}

func TestAC1NewPeriodRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name string
		kind PeriodKind
		date string
		zone string
	}{
		{name: "unknown kind", kind: "monthly", date: "2026-03-02", zone: "UTC"},
		{name: "invalid time zone", kind: DailyPeriod, date: "2026-03-02", zone: "Mars/Olympus"},
		{name: "local machine time zone is not an IANA name", kind: DailyPeriod, date: "2026-03-02", zone: "Local"},
		{name: "empty time zone", kind: DailyPeriod, date: "2026-03-02", zone: ""},
		{name: "invalid date", kind: DailyPeriod, date: "2026-02-30", zone: "UTC"},
		{name: "noncanonical date", kind: DailyPeriod, date: "2026-3-02", zone: "UTC"},
		{name: "local date skipped by time zone", kind: DailyPeriod, date: "2011-12-30", zone: "Pacific/Apia"},
		{name: "week does not start Monday", kind: WeeklyPeriod, date: "2026-03-08", zone: "UTC"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewPeriod(tc.kind, tc.date, tc.zone); !errors.Is(err, ErrInvalidPeriod) {
				t.Fatalf("NewPeriod() error = %v, want ErrInvalidPeriod", err)
			}
		})
	}
}

func TestAC1StaleZoneBoundFallbackStopsAtRealOffsetChange(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	before := time.Date(2026, 3, 8, 6, 59, 58, 0, time.UTC)
	transition := time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC)
	end := transition.Add(3 * time.Second)
	_, oldOffset := before.In(location).Zone()
	if got := nextOffsetChange(before, end, location, oldOffset); !got.Equal(transition) {
		t.Fatalf("fallback offset boundary = %s, want %s", got, transition)
	}
	_, newOffset := transition.In(location).Zone()
	if got := nextOffsetChange(transition, end, location, newOffset); !got.Equal(end) {
		t.Fatalf("fallback without another offset change = %s, want %s", got, end)
	}
}
