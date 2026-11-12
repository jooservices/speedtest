package stats

import (
	"context"
	"math"
	"testing"
)

func TestSummarizeValuesMatchesPreviewInterpolation(t *testing.T) {
	got := summarizeValues([]float64{50, 10, 40, 20, 30})
	if got.N != 5 || got.Mean == nil || got.Median == nil || got.P50 == nil || got.Min == nil || got.Max == nil || got.P5 == nil || got.P95 == nil || got.P99 == nil || got.Std == nil {
		t.Fatalf("summary = %+v, want all five samples and populated metrics", got)
	}
	for name, pair := range map[string][2]float64{
		"mean": {*got.Mean, 30}, "median": {*got.Median, 30}, "p50": {*got.P50, 30}, "min": {*got.Min, 10}, "max": {*got.Max, 50},
		"p5": {*got.P5, 12}, "p95": {*got.P95, 48}, "p99": {*got.P99, 49.6}, "std": {*got.Std, math.Sqrt(200)},
	} {
		if math.Abs(pair[0]-pair[1]) > 1e-12 {
			t.Errorf("%s = %.12f, want %.12f", name, pair[0], pair[1])
		}
	}
	if got.P50 != got.Median {
		t.Fatal("p50 and median must share the same interpolated value")
	}
}

func TestSummarizeValuesEmptyAndSingleSample(t *testing.T) {
	empty := summarizeValues(nil)
	if empty.N != 0 || empty.Mean != nil || empty.P5 != nil || empty.P50 != nil || empty.P99 != nil || empty.Std != nil {
		t.Fatalf("empty summary = %+v, want zero count and null metrics", empty)
	}
	one := summarizeValues([]float64{0})
	if one.N != 1 || one.Mean == nil || *one.Mean != 0 || one.Median == nil || *one.Median != 0 || one.P50 == nil || *one.P50 != 0 || one.P99 == nil || *one.P99 != 0 || one.Std == nil || *one.Std != 0 {
		t.Fatalf("single-sample summary = %+v, want zero values and population stddev 0", one)
	}
}

func TestMetricColumnsAndUnits(t *testing.T) {
	for _, tc := range []struct {
		metric Metric
		column string
		unit   string
	}{
		{Download, "down_mbps", "Mbps"},
		{Upload, "up_mbps", "Mbps"},
		{Ping, "ping_ms", "ms"},
		{Jitter, "jitter_ms", "ms"},
		{Loss, "loss_percent", "%"},
	} {
		column, err := tc.metric.column()
		if err != nil || column != tc.column || tc.metric.unit() != tc.unit {
			t.Errorf("metric %q: column=%q err=%v unit=%q", tc.metric, column, err, tc.metric.unit())
		}
	}
	if _, err := Metric("unexpected").column(); err == nil {
		t.Fatal("unsupported metric accepted")
	}
}

func TestSuccessRateExcludesRateLimitedFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		count   int
		ok      int
		limited int
		want    *float64
	}{
		{name: "rate limits excluded", count: 5, ok: 3, limited: 1, want: number(75)},
		{name: "all rate limited", count: 2, limited: 2},
		{name: "no rate limits", count: 4, ok: 3, want: number(75)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := NodeSummary{Count: tc.count, OK: tc.ok, RateLimited: tc.limited}
			got.setSuccessRate()
			if tc.want == nil {
				if got.SuccessRate != nil {
					t.Fatalf("success rate = %v, want nil", *got.SuccessRate)
				}
				return
			}
			if got.SuccessRate == nil || *got.SuccessRate != *tc.want {
				t.Fatalf("success rate = %v, want %v", got.SuccessRate, *tc.want)
			}
		})
	}
}

func TestCombinedSeriesMatchesPreviewSumOfAvailableNodeBuckets(t *testing.T) {
	first := "node-a"
	second := "node-b"
	a := []Point{{Value: number(100), Count: 2}, {Value: number(300), Count: 1}, {Value: nil, Count: 0}, {Value: nil, Count: 0}}
	b := []Point{{Value: number(50), Count: 1}, {Value: nil, Count: 0}, {Value: number(25), Count: 1}, {Value: nil, Count: 0}}
	got := combinedSeries([]Series{{NodeID: &first, Points: a}, {NodeID: &second, Points: b}}, Download)
	if got.NodeID != nil || got.Name != "Combined total" || len(got.Points) != 4 {
		t.Fatalf("combined series = %+v", got)
	}
	if got.Points[0].Value == nil || *got.Points[0].Value != 150 || got.Points[0].Count != 3 {
		t.Errorf("complete bucket = %+v, want sum 150 from 3 source readings", got.Points[0])
	}
	if got.Points[1].Value == nil || *got.Points[1].Value != 300 || got.Points[1].Count != 1 {
		t.Errorf("one-node bucket = %+v, want the available node total 300", got.Points[1])
	}
	if got.Points[2].Value == nil || *got.Points[2].Value != 25 || got.Points[2].Count != 1 {
		t.Errorf("other one-node bucket = %+v, want the available node total 25", got.Points[2])
	}
	if got.Points[3].Value != nil || got.Points[3].Count != 0 {
		t.Errorf("empty bucket = %+v, want null and count 0", got.Points[3])
	}
	if got.Summary.N != 3 || got.Summary.Mean == nil || math.Abs(*got.Summary.Mean-(475.0/3)) > 1e-12 {
		t.Errorf("combined summary = %+v, want all three displayed bucket totals", got.Summary)
	}
}

func TestNodeIDsDistinguishesUnfilteredAndEmptySelection(t *testing.T) {
	if got := (Query{}).nodeIDs(); got != nil {
		t.Fatalf("unfiltered node IDs = %#v, want nil", got)
	}
	if got := (Query{NodesSet: true}).nodeIDs(); len(got) != 0 {
		t.Fatalf("explicit empty node IDs = %#v, want an empty slice", got)
	}
}

func TestStoreShortCircuitsEmptyNodeSelectionAndRejectsInvalidQueries(t *testing.T) {
	store := &Store{}
	ctx := context.Background()
	empty := Query{NodesSet: true, Metric: Download, Bucket: "day"}
	if got, err := store.Summary(ctx, empty); err != nil || len(got) != 0 {
		t.Fatalf("empty summary = %v, err=%v", got, err)
	}
	if got, err := store.Trend(ctx, empty); err != nil || len(got) != 0 {
		t.Fatalf("empty trend = %v, err=%v", got, err)
	}
	if got, err := store.HourOfDay(ctx, empty); err != nil || len(got) != 0 {
		t.Fatalf("empty hour-of-day = %v, err=%v", got, err)
	}
	if got, err := store.Distribution(ctx, empty); err != nil || len(got) != 0 {
		t.Fatalf("empty distribution = %v, err=%v", got, err)
	}

	invalid := Query{Metric: Metric("unknown"), Bucket: "day"}
	if _, err := store.Trend(ctx, invalid); err == nil {
		t.Fatal("trend accepted an unsupported metric")
	}
	invalid.Metric = Download
	invalid.Bucket = "week"
	if _, err := store.Trend(ctx, invalid); err == nil {
		t.Fatal("trend accepted an unsupported bucket")
	}
	invalid.Bucket = "day"
	invalid.Metric = Ping
	invalid.Combined = true
	if _, err := store.Trend(ctx, invalid); err == nil {
		t.Fatal("combined trend accepted a non-throughput metric")
	}
	invalid.Metric = Metric("unknown")
	if _, err := store.HourOfDay(ctx, invalid); err == nil {
		t.Fatal("hour-of-day accepted an unsupported metric")
	}
	if _, err := store.Heatmap(ctx, invalid, "node"); err == nil {
		t.Fatal("heatmap accepted an unsupported metric")
	}
	if _, err := store.Distribution(ctx, invalid); err == nil {
		t.Fatal("distribution accepted an unsupported metric")
	}
}
