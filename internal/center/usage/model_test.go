package usage

import (
	"math"
	"testing"
	"time"
)

func TestQueryNormalize(t *testing.T) {
	now := time.Date(2026, 11, 9, 8, 0, 0, 0, time.FixedZone("ICT", 7*60*60))
	got, err := (Query{}).Normalize(now)
	if err != nil || got.Group != "day" || got.To.Location() != time.UTC || got.To.Hour() != 1 || got.To.Sub(got.From) != 30*24*time.Hour {
		t.Fatalf("default query = %+v, %v", got, err)
	}
	valid := Query{From: now.Add(-time.Hour), To: now, Group: "week", Engine: "librespeed"}
	if _, err := valid.Normalize(now); err != nil {
		t.Fatalf("valid query: %v", err)
	}
	for name, query := range map[string]Query{
		"group":       {From: now.Add(-time.Hour), To: now, Group: "hour"},
		"engine":      {From: now.Add(-time.Hour), To: now, Engine: "other"},
		"empty range": {From: now, To: now},
		"reverse":     {From: now, To: now.Add(-time.Hour)},
		"too wide":    {From: now.Add(-MaxRange - time.Second), To: now},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := query.Normalize(now); err == nil {
				t.Fatal("invalid query accepted")
			}
		})
	}
}

func TestMonthStartAndProjection(t *testing.T) {
	now := time.Date(2026, 2, 14, 12, 0, 0, 0, time.FixedZone("east", 9*60*60))
	start := MonthStart(now)
	if want := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC); !start.Equal(want) {
		t.Fatalf("month start = %s, want %s", start, want)
	}
	if got := ProjectMonthEnd(130, time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)); got != 3640 {
		t.Fatalf("projected bytes = %d", got)
	}
	if ProjectMonthEnd(10, start) != 0 || ProjectMonthEnd(0, now) != 0 || ProjectMonthEnd(math.MaxInt64, now) != math.MaxInt64 {
		t.Fatal("projection boundary handling failed")
	}
}

func TestRecommendationsAndBudgetValidation(t *testing.T) {
	if got := RecommendedInterval(1024, 30*24*1024); got == nil || *got != 60 {
		t.Fatalf("recommended interval = %v, want 60", got)
	}
	for _, tc := range []struct {
		average, budget float64
		want            int
	}{{1, 1e15, 5}} {
		got := RecommendedInterval(tc.average, int64(tc.budget))
		if got == nil || *got != tc.want {
			t.Fatalf("recommendation (%v,%v) = %v, want %d", tc.average, tc.budget, got, tc.want)
		}
	}
	if RecommendedInterval(1e12, 1) != nil || recommendationStatus(1e12, 1, nil) != "infeasible" {
		t.Fatal("an impossible monthly budget must not receive a false feasible interval")
	}
	if recommendationStatus(0, 1, nil) != "unavailable" || recommendationStatus(1, 1000, RecommendedInterval(1, 1000)) != "feasible" {
		t.Fatal("recommendation status boundary failed")
	}
	for _, average := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		if RecommendedInterval(average, 100) != nil {
			t.Fatalf("invalid average %v received a recommendation", average)
		}
	}
	if RecommendedInterval(100, 0) != nil {
		t.Fatal("zero budget received a recommendation")
	}
	for _, budget := range []Budget{{MonthlyBytes: 10, Action: "pause"}, {MonthlyBytes: 10, Action: "widen", WidenMultiplier: 2}} {
		if err := ValidateBudget(budget); err != nil {
			t.Fatalf("valid budget %+v: %v", budget, err)
		}
	}
	for _, budget := range []Budget{{Action: "pause"}, {MonthlyBytes: 1, Action: "unknown"}, {MonthlyBytes: 1, Action: "widen", WidenMultiplier: 1}, {MonthlyBytes: 1, Action: "widen", WidenMultiplier: 11}, {MonthlyBytes: 1, Action: "pause", WidenMultiplier: 11}} {
		if err := ValidateBudget(budget); err == nil {
			t.Fatalf("invalid budget accepted: %+v", budget)
		}
	}
}

func TestByteHelpersAndComparison(t *testing.T) {
	if addBytes(2, 3) != 5 || addBytes(math.MaxInt64, 1) != math.MaxInt64 {
		t.Fatal("byte addition failed")
	}
	got := comparison(150, 100, 200)
	if got.ChangePercent == nil || *got.ChangePercent != 100 || got.CurrentMonthToDate != 150 {
		t.Fatalf("comparison = %+v", got)
	}
	if comparison(0, 0, 0).ChangePercent != nil {
		t.Fatal("zero comparison should not divide by zero")
	}
	if got := projectBytes(Bytes{Down: 2, Up: 2, Total: 4}, time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)); got.Down != 60 || got.Up != 60 || got.Total != 120 {
		t.Fatalf("projected directions = %+v", got)
	}
}

func TestStoreClockAndEmptyFilters(t *testing.T) {
	start := time.Now().UTC()
	if got := (&Store{}).now(); got.Before(start) {
		t.Fatalf("default store clock = %s, before %s", got, start)
	}
	where, args := usageFilters(Query{}, false)
	if where != "" || len(args) != 0 {
		t.Fatalf("empty usage filters = %q, %v", where, args)
	}
}
