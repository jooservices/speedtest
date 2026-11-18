package report

import (
	"strings"
	"time"
)

const digestHour = 8

const digestDateLayout = "2006-01-02"

// digestPeriodKey is one local calendar period that a digest may claim.
type digestPeriodKey struct {
	kind      PeriodKind
	localDate string
}

// dueDigestPeriods returns the latest daily period and, once Monday 08:00 has
// passed, the preceding Monday-Sunday week. Before 08:00 the previous day is
// still waiting for its own run.
func dueDigestPeriods(now time.Time, location *time.Location) []digestPeriodKey {
	local := now.In(location)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
	daily := day.AddDate(0, 0, -1)
	if local.Hour() < digestHour {
		daily = daily.AddDate(0, 0, -1)
	}
	out := []digestPeriodKey{{kind: DailyPeriod, localDate: daily.Format(digestDateLayout)}}
	weekStart := day.AddDate(0, 0, -daysSinceMonday(day.Weekday()))
	if !now.Before(digestHourOf(weekStart)) {
		out = append(out, digestPeriodKey{kind: WeeklyPeriod, localDate: weekStart.AddDate(0, 0, -7).Format(digestDateLayout)})
	}
	return out
}

func daysSinceMonday(weekday time.Weekday) int {
	return (int(weekday) + 6) % 7
}

func digestHourOf(day time.Time) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), digestHour, 0, 0, 0, day.Location())
}

// nextDigestRun returns the next owner-local 08:00 strictly after now.
func nextDigestRun(now time.Time, location *time.Location) time.Time {
	local := now.In(location)
	next := digestHourOf(time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location))
	if !next.After(now) {
		tomorrow := local.AddDate(0, 0, 1)
		next = digestHourOf(time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 0, 0, 0, 0, location))
	}
	return next
}

// digestTimezone turns the owner display preference into an IANA zone the
// scheduler can use. A browser-only or invalid value falls back to UTC.
func digestTimezone(zone string) string {
	zone = strings.TrimSpace(zone)
	switch zone {
	case "", "browser", "Local":
		return "UTC"
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return "UTC"
	}
	return zone
}
