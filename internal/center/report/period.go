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

// Period is an owner-local calendar period represented by UTC query bounds [From, To).
type Period struct {
	kind      PeriodKind
	localDate string
	timezone  string
	from      time.Time
	to        time.Time
}

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
	year, month, day := date.Date()
	from := time.Date(year, month, day, 0, 0, 0, 0, location)
	if from.In(location).Format("2006-01-02") != localDate {
		return Period{}, fmt.Errorf("%w: local date does not exist in time zone", ErrInvalidPeriod)
	}
	to := from.AddDate(0, 0, days)

	return Period{
		kind:      kind,
		localDate: localDate,
		timezone:  timezone,
		from:      from.UTC(),
		to:        to.UTC(),
	}, nil
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

// From returns the inclusive UTC start bound.
func (p Period) From() time.Time { return p.from }

// To returns the exclusive UTC end bound.
func (p Period) To() time.Time { return p.to }
