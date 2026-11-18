package report

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
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
			want: []digestPeriodKey{
				{kind: DailyPeriod, localDate: "2026-03-07"},
				{kind: WeeklyPeriod, localDate: "2026-02-23"},
			},
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

func TestAC1DigestScheduleDateRecoveryIsBounded(t *testing.T) {
	tests := []struct {
		name    string
		kind    PeriodKind
		after   string
		through string
		limit   int
		want    []string
	}{
		{
			name:    "daily catch-up stops at the batch limit",
			kind:    DailyPeriod,
			after:   "2026-03-01",
			through: "2026-03-10",
			limit:   3,
			want:    []string{"2026-03-02", "2026-03-03", "2026-03-04"},
		},
		{
			name:    "weekly catch-up advances by seven calendar days",
			kind:    WeeklyPeriod,
			after:   "2026-03-02",
			through: "2026-03-23",
			limit:   8,
			want:    []string{"2026-03-09", "2026-03-16", "2026-03-23"},
		},
		{
			name:    "no new due dates returns an empty batch",
			kind:    DailyPeriod,
			after:   "2026-03-10",
			through: "2026-03-10",
			limit:   3,
			want:    []string{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := digestDatesAfter(tc.kind, tc.after, tc.through, tc.limit)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("scheduled dates=%v, want %v", got, tc.want)
			}
			for index := range tc.want {
				if got[index] != tc.want[index] {
					t.Errorf("scheduled date %d=%s, want %s", index, got[index], tc.want[index])
				}
			}
		})
	}
}

func TestAC1DigestScheduleRejectsInvalidPeriodDatesAndKinds(t *testing.T) {
	if _, err := previousDigestDate(DailyPeriod, "not-a-date"); err == nil {
		t.Fatal("invalid period date must fail")
	}
	if _, err := previousDigestDate(PeriodKind("monthly"), "2026-03-01"); err == nil {
		t.Fatal("unsupported period kind must fail")
	}
	if _, err := digestDatesAfter(DailyPeriod, "bad", "2026-03-01", 2); err == nil {
		t.Fatal("invalid watermark date must fail")
	}
	if _, err := digestDatesAfter(DailyPeriod, "2026-03-01", "bad", 2); err == nil {
		t.Fatal("invalid latest date must fail")
	}
	if _, err := digestDatesAfter(PeriodKind("monthly"), "2026-03-01", "2026-03-31", 2); err == nil {
		t.Fatal("unsupported period kind must fail")
	}
}

func TestDigestLoopWaitUsesEarliestPositiveWakeup(t *testing.T) {
	tests := []struct {
		name  string
		first time.Duration
		other time.Duration
		want  time.Duration
	}{
		{name: "earlier first", first: time.Second, other: 2 * time.Second, want: time.Second},
		{name: "earlier second", first: 2 * time.Second, other: time.Second, want: time.Second},
		{name: "nonpositive first", first: 0, other: time.Second, want: time.Second},
		{name: "nonpositive second", first: time.Second, other: 0, want: time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := minDigestWait(tc.first, tc.other); got != tc.want {
				t.Errorf("minimum digest wait=%s, want %s", got, tc.want)
			}
		})
	}
}

func TestDigestLoopLogsSafeFailureCode(t *testing.T) {
	tests := []struct {
		name    string
		failure error
		zone    func(context.Context) (string, error)
		code    string
	}{
		{
			name:    "enqueue failure",
			failure: withDigestCode("enqueue", errors.New("credential=private-value")),
			code:    "enqueue",
		},
		{
			name: "time zone lookup failure",
			zone: func(context.Context) (string, error) { return "", errors.New("token=private-value") },
			code: "timezone_read",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			scheduler := &DigestScheduler{
				Timezone: tc.zone,
				Log:      slog.New(slog.NewTextHandler(&output, nil)),
			}
			if tc.failure != nil {
				scheduler.loopWait(context.Background(), tc.failure)
			} else {
				scheduler.loopWait(context.Background(), nil)
			}
			logged := output.String()
			if !strings.Contains(logged, "code="+tc.code) {
				t.Fatalf("log %q does not identify code %q", logged, tc.code)
			}
			if strings.Contains(logged, "private-value") {
				t.Fatalf("log exposed raw error content: %q", logged)
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
