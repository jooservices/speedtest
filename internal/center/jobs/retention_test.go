package jobs

import (
	"testing"
	"time"
)

func TestNextRunIsStrictlyInTheFutureAcrossBoundaries(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for in, want := range map[string]string{
		"2026-11-03T02:00:00Z":      "2026-11-03T03:15:00Z",
		"2026-11-03T03:15:00Z":      "2026-11-04T03:15:00Z",
		"2026-11-03T03:14:59Z":      "2026-11-03T03:15:00Z",
		"2026-11-03T23:59:59Z":      "2026-11-04T03:15:00Z",
		"2026-12-31T20:00:00Z":      "2027-01-01T03:15:00Z",
		"2026-11-03T10:00:00+07:00": "2026-11-03T03:15:00Z",
	} {
		if got := NextRun(at(in), RunHour, RunMinute); !got.Equal(at(want)) {
			t.Errorf("NextRun(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestDefaultsAndClock(t *testing.T) {
	r := &Retention{}
	if r.now().IsZero() || r.logger() == nil {
		t.Fatal("zero-value Retention must work")
	}
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if !(&Retention{Now: func() time.Time { return fixed }}).now().Equal(fixed) {
		t.Fatal("Now override")
	}
}
