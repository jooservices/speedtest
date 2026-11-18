package report

import (
	"errors"
	"fmt"
	"time"
)

// ErrInvalidPeriod reports an invalid period kind, local date, or IANA time zone.
var ErrInvalidPeriod = errors.New("invalid report period")

// PeriodKind identifies the local calendar period used by a node summary.
type PeriodKind string

const (
	DailyPeriod  PeriodKind = "daily"
	WeeklyPeriod PeriodKind = "weekly"
)

// Period identifies an owner-local calendar period and its exact UTC membership intervals.
type Period struct {
	kind      PeriodKind
	localDate string
	timezone  string
	from      time.Time
	to        time.Time
	intervals []utcInterval
}

type utcInterval struct {
	from time.Time
	to   time.Time
}

const timezoneSearchPadding = 36 * time.Hour

// NewPeriod builds a daily period or a Monday-first Monday-to-Sunday week from an ISO local date.
func NewPeriod(kind PeriodKind, localDate, timezone string) (Period, error) {
	days, ok := periodDays(kind)
	if !ok {
		return Period{}, fmt.Errorf("%w: unsupported period kind", ErrInvalidPeriod)
	}

	date, err := time.Parse("2006-01-02", localDate)
	if err != nil || date.Format("2006-01-02") != localDate {
		return Period{}, fmt.Errorf("%w: local date must be YYYY-MM-DD", ErrInvalidPeriod)
	}
	if kind == WeeklyPeriod && date.Weekday() != time.Monday {
		return Period{}, fmt.Errorf("%w: weekly periods must start on Monday", ErrInvalidPeriod)
	}

	location, err := periodLocation(timezone)
	if err != nil {
		return Period{}, err
	}
	intervals := periodIntervals(date, days, location)
	if len(intervals) == 0 {
		return Period{}, fmt.Errorf("%w: local date does not exist in time zone", ErrInvalidPeriod)
	}

	return Period{
		kind:      kind,
		localDate: localDate,
		timezone:  timezone,
		from:      intervals[0].from,
		to:        intervals[len(intervals)-1].to,
		intervals: intervals,
	}, nil
}

func periodIntervals(date time.Time, days int, location *time.Location) []utcInterval {
	localEnd := date.AddDate(0, 0, days)
	searchStart := date.Add(-timezoneSearchPadding)
	searchEnd := localEnd.Add(timezoneSearchPadding)
	intervals := make([]utcInterval, 0, 2)
	for cursor := searchStart; cursor.Before(searchEnd); {
		localCursor := cursor.In(location)
		_, offset := localCursor.Zone()
		_, zoneEnd := localCursor.ZoneBounds()
		if zoneEnd.IsZero() || zoneEnd.After(searchEnd) {
			zoneEnd = searchEnd
		}

		utcStart := later(cursor, date.Add(-time.Duration(offset)*time.Second))
		utcEnd := earlier(zoneEnd, localEnd.Add(-time.Duration(offset)*time.Second))
		if utcStart.Before(utcEnd) {
			intervals = appendUTCInterval(intervals, utcStart, utcEnd)
		}
		cursor = zoneEnd
	}
	return intervals
}

func appendUTCInterval(intervals []utcInterval, from, to time.Time) []utcInterval {
	if len(intervals) > 0 && intervals[len(intervals)-1].to.Equal(from) {
		intervals[len(intervals)-1].to = to.UTC()
		return intervals
	}
	return append(intervals, utcInterval{from: from.UTC(), to: to.UTC()})
}

func later(left, right time.Time) time.Time {
	if left.After(right) {
		return left
	}
	return right
}

func earlier(left, right time.Time) time.Time {
	if left.Before(right) {
		return left
	}
	return right
}

func periodDays(kind PeriodKind) (int, bool) {
	switch kind {
	case DailyPeriod:
		return 1, true
	case WeeklyPeriod:
		return 7, true
	default:
		return 0, false
	}
}

func periodLocation(timezone string) (*time.Location, error) {
	if timezone == "" || timezone == "Local" {
		return nil, fmt.Errorf("%w: time zone must be an IANA name", ErrInvalidPeriod)
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid IANA time zone", ErrInvalidPeriod)
	}
	return location, nil
}

// Kind returns whether this period is daily or weekly.
func (p Period) Kind() PeriodKind { return p.kind }

// LocalDate returns the local date that starts this period.
func (p Period) LocalDate() string { return p.localDate }

// Timezone returns the IANA time zone used to construct this period.
func (p Period) Timezone() string { return p.timezone }

// From returns the inclusive start of the UTC envelope containing this period's intervals.
func (p Period) From() time.Time { return p.from }

// To returns the exclusive end of the UTC envelope containing this period's intervals.
func (p Period) To() time.Time { return p.to }
