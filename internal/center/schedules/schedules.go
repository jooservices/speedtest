// Package schedules holds the pure scheduling rules of the command center: slot arithmetic, quiet
// hours, staggering and clash detection. It has no database or HTTP code.
package schedules

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"
)

const (
	// MinInterval and MaxInterval bound a node's test interval in minutes.
	MinInterval = 5
	MaxInterval = 1440
	// OverlapMinutes is how long a test is assumed to occupy the link when looking for clashes.
	OverlapMinutes = 5
	// MaxUpcoming is the longest list of upcoming runs returned for a node.
	MaxUpcoming = 10
	// horizon bounds the search for the next slot outside quiet hours.
	horizon = 14 * 24 * time.Hour
)

var clock = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

// Slot is a node schedule: tests start at Unix epoch minute Offset + k*Interval.
type Slot struct {
	Interval int
	Offset   int
}

// Clamp returns a valid slot: the interval inside its bounds and the offset below the interval.
func Clamp(interval, offset int) Slot {
	if interval < MinInterval {
		interval = MinInterval
	}
	if interval > MaxInterval {
		interval = MaxInterval
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= interval {
		offset = interval - 1
	}
	return Slot{Interval: interval, Offset: offset}
}

// Validate reports an invalid interval or an offset that is not below the interval.
func (s Slot) Validate() error {
	switch {
	case s.Interval < MinInterval || s.Interval > MaxInterval:
		return fmt.Errorf("interval must be between %d and %d minutes", MinInterval, MaxInterval)
	case s.Offset < 0 || s.Offset >= s.Interval:
		return errors.New("offset must be at least 0 and less than the interval")
	}
	return nil
}

// Quiet is a daily window, in its own time zone, in which scheduled tests are skipped. It may cross midnight.
type Quiet struct {
	Start, End, Timezone string
}

// Enabled reports whether a window is configured.
func (q Quiet) Enabled() bool { return q.Start != "" && q.End != "" }

// Validate checks that both ends are set or empty, differ, are HH:mm and that the zone is known.
func (q Quiet) Validate() error {
	if (q.Start == "") != (q.End == "") {
		return errors.New("quietStart and quietEnd must both be empty or both be set")
	}
	if q.Start != "" && (!clock.MatchString(q.Start) || !clock.MatchString(q.End)) {
		return errors.New("quiet hours must be HH:mm")
	}
	if q.Start != "" && q.Start == q.End {
		return errors.New("quietStart and quietEnd must differ")
	}
	if q.Timezone != "" {
		if _, err := time.LoadLocation(q.Timezone); err != nil {
			return errors.New("timezone must be an IANA time-zone name")
		}
	}
	return nil
}

func minutesOfDay(hhmm string) int {
	return int(hhmm[0]-'0')*600 + int(hhmm[1]-'0')*60 + int(hhmm[3]-'0')*10 + int(hhmm[4]-'0')
}

// Contains reports whether t falls inside the quiet window (start inclusive, end exclusive).
func (q Quiet) Contains(t time.Time) bool {
	if !q.Enabled() {
		return false
	}
	loc := time.UTC
	if q.Timezone != "" {
		if l, err := time.LoadLocation(q.Timezone); err == nil {
			loc = l
		}
	}
	local := t.In(loc)
	now := local.Hour()*60 + local.Minute()
	start, end := minutesOfDay(q.Start), minutesOfDay(q.End)
	if start < end {
		return now >= start && now < end
	}
	return now >= start || now < end
}

// first returns the first slot minute (Unix epoch minutes) strictly after the given instant.
func first(s Slot, after time.Time) int64 {
	m := after.Unix()/60 + 1
	if after.Unix() < 0 {
		m = 0
	}
	iv := int64(s.Interval)
	return m + ((int64(s.Offset)-m%iv)%iv+iv)%iv
}

// NextRuns returns up to n slot times after the given instant that are not in quiet hours.
func NextRuns(s Slot, q Quiet, after time.Time, n int) []time.Time {
	runs := []time.Time{}
	if s.Validate() != nil || n <= 0 {
		return runs
	}
	end := after.Add(horizon)
	for m := first(s, after); len(runs) < n; m += int64(s.Interval) {
		t := time.Unix(m*60, 0).UTC()
		if t.After(end) {
			break
		}
		if !q.Contains(t) {
			runs = append(runs, t)
		}
	}
	return runs
}

// RunsPerDay counts the tests that start in the 24 hours after the given instant, outside quiet hours.
func RunsPerDay(s Slot, q Quiet, from time.Time) int {
	if s.Validate() != nil {
		return 0
	}
	count := 0
	end := from.Add(24 * time.Hour)
	// The window includes the current minute, so look from one minute earlier.
	for m := first(s, from.Add(-time.Minute)); ; m += int64(s.Interval) {
		t := time.Unix(m*60, 0).UTC()
		if !t.Before(end) {
			return count
		}
		if !q.Contains(t) {
			count++
		}
	}
}

// Node is a node's schedule as seen by the coordination rules.
type Node struct {
	ID       string
	Interval int
	Offset   int
	Enabled  bool
}

// Stagger assigns evenly spread offsets: nodes with the same interval, ordered by ID, are spaced by
// interval/count minutes. The result is deterministic and every offset is below its interval.
func Stagger(nodes []Node) map[string]int {
	groups := map[int][]string{}
	for _, n := range nodes {
		groups[n.Interval] = append(groups[n.Interval], n.ID)
	}
	out := make(map[string]int, len(nodes))
	for interval, ids := range groups {
		sort.Strings(ids)
		for i, id := range ids {
			out[id] = i * interval / len(ids)
		}
	}
	return out
}

// Clash is a pair of enabled nodes whose tests can overlap.
type Clash struct {
	A, B   string
	Detail string
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// overlaps reports whether two schedules can ever start within OverlapMinutes of each other. Slots of
// A and B differ by Offset difference modulo gcd(intervals), so the closest pair is found without enumerating runs.
func overlaps(a, b Node) (bool, int) {
	g := gcd(a.Interval, b.Interval)
	r := ((a.Offset-b.Offset)%g + g) % g
	dist := r
	if g-r < dist {
		dist = g - r
	}
	return dist < OverlapMinutes, dist
}

// Clashes lists pairs of enabled nodes that can run at the same time. With maxConcurrent above one, a pair
// is reported only when the nodes overlapping either of them could not all run within the limit.
func Clashes(nodes []Node, maxConcurrent int) []Clash {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	var active []Node
	for _, n := range nodes {
		if n.Enabled {
			active = append(active, n)
		}
	}
	sort.Slice(active, func(i, j int) bool { return active[i].ID < active[j].ID })
	degree := map[string]int{}
	type pair struct {
		a, b Node
		dist int
	}
	var pairs []pair
	for i := range active {
		for j := i + 1; j < len(active); j++ {
			if ok, dist := overlaps(active[i], active[j]); ok {
				pairs = append(pairs, pair{active[i], active[j], dist})
				degree[active[i].ID]++
				degree[active[j].ID]++
			}
		}
	}
	out := []Clash{}
	for _, p := range pairs {
		group := degree[p.a.ID] + 1
		if other := degree[p.b.ID] + 1; other > group {
			group = other
		}
		if group > maxConcurrent {
			out = append(out, Clash{A: p.a.ID, B: p.b.ID, Detail: fmt.Sprintf("tests start %d min apart, less than the %d min a test is assumed to take", p.dist, OverlapMinutes)})
		}
	}
	return out
}
