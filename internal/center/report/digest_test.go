package report

import (
	"testing"
	"time"
)

func TestAC1DueDigestPeriodsUseOwnerLocalEightAndMonday(t *testing.T) {
	location, err := time.LoadLocation("Asia/Ho_Chi_Minh")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		now  time.Time
		want []digestPeriodKey
	}{
		{
			name: "Tuesday before the daily run still waits for the previous day",
			now:  time.Date(2026, 3, 10, 0, 59, 0, 0, time.UTC),
			want: []digestPeriodKey{
				{kind: DailyPeriod, localDate: "2026-03-08"},
				{kind: WeeklyPeriod, localDate: "2026-03-02"},
			},
		},
		{
			name: "Tuesday at 08:00 includes the daily and the preceding week",
			now:  time.Date(2026, 3, 10, 1, 0, 0, 0, time.UTC),
			want: []digestPeriodKey{
				{kind: DailyPeriod, localDate: "2026-03-09"},
				{kind: WeeklyPeriod, localDate: "2026-03-02"},
			},
		},
		{
			name: "Sunday keeps the preceding week due until Monday 08:00",
			now:  time.Date(2026, 3, 8, 0, 59, 0, 0, time.UTC),
			want: []digestPeriodKey{
				{kind: DailyPeriod, localDate: "2026-03-06"},
				{kind: WeeklyPeriod, localDate: "2026-02-23"},
			},
		},
		{
			name: "Monday before 08:00 does not include the new week yet",
			now:  time.Date(2026, 3, 9, 0, 59, 0, 0, time.UTC),
			want: []digestPeriodKey{{kind: DailyPeriod, localDate: "2026-03-07"}},
		},
		{
			name: "Monday at 08:00 includes Sunday and the preceding Monday-Sunday week",
			now:  time.Date(2026, 3, 9, 1, 0, 0, 0, time.UTC),
			want: []digestPeriodKey{
				{kind: DailyPeriod, localDate: "2026-03-08"},
				{kind: WeeklyPeriod, localDate: "2026-03-02"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertDigestPeriods(t, dueDigestPeriods(tc.now, location), tc.want)
		})
	}
}

func TestAC1DueDigestPeriodsUseDSTDailyAndWeeklyEnvelopes(t *testing.T) {
	tests := []struct {
		name       string
		now        time.Time
		dailyDate  string
		weeklyDate string
		dailySpan  time.Duration
		weeklySpan time.Duration
	}{
		{
			name:       "spring forward Monday",
			now:        time.Date(2026, 3, 9, 12, 0, 0, 0, time.UTC),
			dailyDate:  "2026-03-08",
			weeklyDate: "2026-03-02",
			dailySpan:  23 * time.Hour,
			weeklySpan: 167 * time.Hour,
		},
		{
			name:       "fall back Monday",
			now:        time.Date(2026, 11, 2, 13, 0, 0, 0, time.UTC),
			dailyDate:  "2026-11-01",
			weeklyDate: "2026-10-26",
			dailySpan:  25 * time.Hour,
			weeklySpan: 169 * time.Hour,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			location, err := time.LoadLocation("America/New_York")
			if err != nil {
				t.Fatal(err)
			}
			keys := dueDigestPeriods(tc.now, location)
			assertDigestPeriods(t, keys, []digestPeriodKey{
				{kind: DailyPeriod, localDate: tc.dailyDate},
				{kind: WeeklyPeriod, localDate: tc.weeklyDate},
			})
			assertPeriodSpan(t, keys[0], location, tc.dailySpan)
			assertPeriodSpan(t, keys[1], location, tc.weeklySpan)
		})
	}
}

func TestAC1NextDigestRunUsesOwnerLocalEightAcrossDST(t *testing.T) {
	tests := []struct {
		name string
		zone string
		now  time.Time
		want time.Time
	}{
		{
			name: "later today",
			zone: "Asia/Ho_Chi_Minh",
			now:  time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC),
			want: time.Date(2026, 3, 9, 1, 0, 0, 0, time.UTC),
		},
		{
			name: "exactly 08:00 waits for tomorrow",
			zone: "Asia/Ho_Chi_Minh",
			now:  time.Date(2026, 3, 9, 1, 0, 0, 0, time.UTC),
			want: time.Date(2026, 3, 10, 1, 0, 0, 0, time.UTC),
		},
		{
			name: "spring forward day",
			zone: "America/New_York",
			now:  time.Date(2026, 3, 7, 13, 0, 0, 0, time.UTC),
			want: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC),
		},
		{
			name: "fall back day",
			zone: "America/New_York",
			now:  time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC),
			want: time.Date(2026, 11, 1, 13, 0, 0, 0, time.UTC),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			location, err := time.LoadLocation(tc.zone)
			if err != nil {
				t.Fatal(err)
			}
			if got := nextDigestRun(tc.now, location); !got.Equal(tc.want) {
				t.Errorf("nextDigestRun() = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestDigestTimezoneFallsBackToUTC(t *testing.T) {
	for _, zone := range []string{"", "browser", "Local", "Mars/Olympus"} {
		if got := digestTimezone(zone); got != "UTC" {
			t.Errorf("digestTimezone(%q) = %q, want UTC", zone, got)
		}
	}
	if got := digestTimezone("Asia/Ho_Chi_Minh"); got != "Asia/Ho_Chi_Minh" {
		t.Errorf("digestTimezone() = %q, want the IANA zone", got)
	}
}

func assertDigestPeriods(t *testing.T, got, want []digestPeriodKey) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("due periods = %+v, want %+v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("due period %d = %+v, want %+v", index, got[index], want[index])
		}
	}
}

func assertPeriodSpan(t *testing.T, key digestPeriodKey, location *time.Location, want time.Duration) {
	t.Helper()
	period, err := NewPeriod(key.kind, key.localDate, location.String())
	if err != nil {
		t.Fatal(err)
	}
	if span := period.To().Sub(period.From()); span != want {
		t.Errorf("%s period span = %s, want %s", key.kind, span, want)
	}
}
