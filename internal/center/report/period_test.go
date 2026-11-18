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
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			period, err := NewPeriod(tc.kind, tc.date, tc.zone)
			if err != nil {
				t.Fatalf("NewPeriod() error = %v", err)
			}
			if !period.From().Equal(tc.from) || !period.To().Equal(tc.to) {
				t.Fatalf("bounds = [%s, %s), want [%s, %s)", period.From(), period.To(), tc.from, tc.to)
			}
			if got := period.To().Sub(period.From()); got != tc.wantSpan {
				t.Fatalf("period duration = %s, want %s", got, tc.wantSpan)
			}
			if period.From().Location() != time.UTC || period.To().Location() != time.UTC {
				t.Fatalf("period bounds are not UTC: %s %s", period.From().Location(), period.To().Location())
			}
			if period.Kind() != tc.kind || period.LocalDate() != tc.date || period.Timezone() != tc.zone {
				t.Fatalf("period identity = %q %q %q", period.Kind(), period.LocalDate(), period.Timezone())
			}
		})
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
