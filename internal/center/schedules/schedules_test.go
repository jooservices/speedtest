package schedules

import (
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestClampAndValidate(t *testing.T) {
	for _, tc := range []struct{ in, off, wantIn, wantOff int }{
		{30, 10, 30, 10}, {30, 45, 30, 29}, {2, 0, 5, 0}, {9999, 5000, 1440, 1439}, {10, -3, 10, 0},
	} {
		got := Clamp(tc.in, tc.off)
		if got.Interval != tc.wantIn || got.Offset != tc.wantOff || got.Validate() != nil {
			t.Errorf("Clamp(%d,%d) = %+v", tc.in, tc.off, got)
		}
	}
	for _, s := range []Slot{{4, 0}, {1441, 0}, {30, 30}, {30, -1}} {
		if s.Validate() == nil {
			t.Errorf("%+v accepted", s)
		}
	}
}

func TestQuietValidate(t *testing.T) {
	ok := []Quiet{{}, {"22:00", "06:00", "Asia/Ho_Chi_Minh"}, {"", "", "UTC"}, {"00:00", "23:59", ""}}
	bad := []Quiet{{"22:00", "", "UTC"}, {"", "06:00", "UTC"}, {"22:00", "22:00", "UTC"}, {"24:00", "06:00", "UTC"}, {"9:00", "10:00", "UTC"}, {"22:00", "06:00", "Mars/Base"}}
	for _, q := range ok {
		if err := q.Validate(); err != nil {
			t.Errorf("%+v rejected: %v", q, err)
		}
	}
	for _, q := range bad {
		if q.Validate() == nil {
			t.Errorf("%+v accepted", q)
		}
	}
}

func TestQuietContains(t *testing.T) {
	night := Quiet{"22:00", "06:00", "Asia/Ho_Chi_Minh"} // UTC+7
	day := Quiet{"09:00", "17:00", "UTC"}
	for _, tc := range []struct {
		q    Quiet
		t    string
		want bool
	}{
		{night, "2026-11-03T15:00:00Z", true},  // 22:00 local, start inclusive
		{night, "2026-11-03T14:59:00Z", false}, // 21:59 local
		{night, "2026-11-02T22:59:00Z", true},  // 05:59 local next day
		{night, "2026-11-02T23:00:00Z", false}, // 06:00 local, end exclusive
		{day, "2026-11-03T09:00:00Z", true}, {day, "2026-11-03T17:00:00Z", false}, {day, "2026-11-03T08:59:00Z", false},
		{Quiet{}, "2026-11-03T03:00:00Z", false},
		{Quiet{"22:00", "06:00", "Bad/Zone"}, "2026-11-03T23:00:00Z", true}, // unknown zone falls back to UTC
	} {
		if got := tc.q.Contains(at(tc.t)); got != tc.want {
			t.Errorf("%+v at %s = %v, want %v", tc.q, tc.t, got, tc.want)
		}
	}
}

func TestNextRunsAcrossHourDayAndYearBoundaries(t *testing.T) {
	// Epoch-aligned slots: interval 30 offset 10 start at minutes :10 and :40.
	got := NextRuns(Slot{30, 10}, Quiet{}, at("2026-11-03T10:45:30Z"), 3)
	want := []string{"2026-11-03T11:10:00Z", "2026-11-03T11:40:00Z", "2026-11-03T12:10:00Z"}
	for i, w := range want {
		if i >= len(got) || !got[i].Equal(at(w)) {
			t.Fatalf("hour boundary: got %v, want %v", got, want)
		}
	}
	day := NextRuns(Slot{60, 0}, Quiet{}, at("2026-12-31T23:30:00Z"), 2)
	if !day[0].Equal(at("2027-01-01T00:00:00Z")) || !day[1].Equal(at("2027-01-01T01:00:00Z")) {
		t.Fatalf("year boundary: %v", day)
	}
	exact := NextRuns(Slot{30, 0}, Quiet{}, at("2026-11-03T10:00:00Z"), 1)
	if !exact[0].Equal(at("2026-11-03T10:30:00Z")) {
		t.Fatalf("a slot at the reference instant is not 'next': %v", exact)
	}
	seconds := NextRuns(Slot{30, 0}, Quiet{}, at("2026-11-03T09:59:59Z"), 1)
	if !seconds[0].Equal(at("2026-11-03T10:00:00Z")) {
		t.Fatalf("one second before a slot: %v", seconds)
	}
	long := NextRuns(Slot{1440, 600}, Quiet{}, at("2026-11-03T10:00:00Z"), 2) // daily at 10:00 UTC
	if !long[0].Equal(at("2026-11-04T10:00:00Z")) || !long[1].Equal(at("2026-11-05T10:00:00Z")) {
		t.Fatalf("daily: %v", long)
	}
	if len(NextRuns(Slot{30, 30}, Quiet{}, at("2026-11-03T10:00:00Z"), 3)) != 0 {
		t.Fatal("an invalid slot must give no runs")
	}
	if len(NextRuns(Slot{30, 0}, Quiet{}, at("2026-11-03T10:00:00Z"), 0)) != 0 {
		t.Fatal("n = 0")
	}
}

func TestNextRunsSkipQuietHours(t *testing.T) {
	q := Quiet{"22:00", "06:00", "UTC"}
	got := NextRuns(Slot{60, 0}, q, at("2026-11-03T20:30:00Z"), 3)
	want := []string{"2026-11-03T21:00:00Z", "2026-11-04T06:00:00Z", "2026-11-04T07:00:00Z"}
	for i, w := range want {
		if i >= len(got) || !got[i].Equal(at(w)) {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	// A window from 00:00 to 23:59 leaves only the 23:59 minute open.
	almost := Quiet{"00:00", "23:59", "UTC"}
	if runs := NextRuns(Slot{1440, 1439}, almost, at("2026-11-03T00:00:00Z"), 1); len(runs) != 1 || runs[0].Format(time.RFC3339) != "2026-11-03T23:59:00Z" {
		t.Fatalf("23:59 slot = %v", runs)
	}
	if runs := NextRuns(Slot{60, 0}, almost, at("2026-11-03T00:00:00Z"), 2); len(runs) != 0 {
		t.Fatalf("hourly slots are all quiet: %v", runs)
	}
}

func TestRunsPerDay(t *testing.T) {
	from := at("2026-11-03T00:00:00Z")
	if got := RunsPerDay(Slot{30, 0}, Quiet{}, from); got != 48 {
		t.Fatalf("30 min = %d", got)
	}
	if got := RunsPerDay(Slot{1440, 600}, Quiet{}, from); got != 1 {
		t.Fatalf("daily = %d", got)
	}
	if got := RunsPerDay(Slot{60, 0}, Quiet{"22:00", "06:00", "UTC"}, from); got != 16 {
		t.Fatalf("hourly with 8 quiet hours = %d, want 16", got)
	}
	if got := RunsPerDay(Slot{3, 0}, Quiet{}, from); got != 0 {
		t.Fatalf("invalid slot = %d", got)
	}
}

func TestStaggerSpreadsEvenlyAndDeterministically(t *testing.T) {
	nodes := []Node{{"c", 30, 0, true}, {"a", 30, 0, true}, {"b", 30, 0, true}, {"d", 30, 0, true}, {"x", 60, 0, true}, {"y", 60, 0, false}}
	got := Stagger(nodes)
	if got["a"] != 0 || got["b"] != 7 || got["c"] != 15 || got["d"] != 22 || got["x"] != 0 || got["y"] != 30 {
		t.Fatalf("stagger = %v", got)
	}
	again := Stagger([]Node{nodes[5], nodes[4], nodes[3], nodes[2], nodes[1], nodes[0]})
	for id, off := range got {
		if again[id] != off {
			t.Fatalf("not deterministic for %s", id)
		}
	}
	one := Stagger([]Node{{"solo", 45, 20, true}})
	if one["solo"] != 0 {
		t.Fatalf("single node = %v", one)
	}
	// 3 nodes on 5 minutes: offsets 0,1,3 are all below the interval.
	for _, off := range Stagger([]Node{{"a", 5, 0, true}, {"b", 5, 0, true}, {"c", 5, 0, true}}) {
		if off < 0 || off >= 5 {
			t.Fatalf("offset %d outside interval", off)
		}
	}
}

func TestClashes(t *testing.T) {
	same := []Node{{"a", 30, 0, true}, {"b", 30, 3, true}, {"c", 30, 15, true}}
	got := Clashes(same, 1)
	if len(got) != 1 || got[0].A != "a" || got[0].B != "b" {
		t.Fatalf("clashes = %+v", got)
	}
	if len(Clashes(same, 2)) != 0 {
		t.Fatalf("two nodes may overlap when two tests are allowed: %+v", Clashes(same, 2))
	}
	if got := Clashes([]Node{{"a", 30, 0, true}, {"b", 30, 5, true}}, 1); len(got) != 0 {
		t.Fatalf("5 minutes apart is not an overlap: %+v", got)
	}
	// Different intervals: 30 and 45 share gcd 15, so offsets 0 and 2 come within 2 minutes.
	if got := Clashes([]Node{{"a", 30, 0, true}, {"b", 45, 2, true}}, 1); len(got) != 1 {
		t.Fatalf("mixed intervals = %+v", got)
	}
	// Coprime intervals always meet.
	if got := Clashes([]Node{{"a", 7, 0, true}, {"b", 11, 3, true}}, 1); len(got) != 1 {
		t.Fatalf("coprime intervals = %+v", got)
	}
	// Wrap-around: offsets 29 and 1 on a 30 minute interval are two minutes apart.
	if got := Clashes([]Node{{"a", 30, 29, true}, {"b", 30, 1, true}}, 1); len(got) != 1 {
		t.Fatalf("wrap around = %+v", got)
	}
	if got := Clashes([]Node{{"a", 30, 0, true}, {"b", 30, 0, false}}, 1); len(got) != 0 {
		t.Fatalf("a disabled node never clashes: %+v", got)
	}
	three := []Node{{"a", 30, 0, true}, {"b", 30, 1, true}, {"c", 30, 2, true}}
	if got := Clashes(three, 2); len(got) != 3 {
		t.Fatalf("three nodes with two slots all clash: %+v", got)
	}
	if got := Clashes(three, 3); len(got) != 0 {
		t.Fatalf("three nodes with three slots do not clash: %+v", got)
	}
	if got := Clashes(same, 0); len(got) != 1 {
		t.Fatal("a limit below one is treated as one")
	}
	staggered := Stagger([]Node{{"a", 30, 0, true}, {"b", 30, 0, true}, {"c", 30, 0, true}})
	var after []Node
	for id, off := range staggered {
		after = append(after, Node{id, 30, off, true})
	}
	if got := Clashes(after, 1); len(got) != 0 {
		t.Fatalf("staggering must remove clashes: %+v", got)
	}
}
