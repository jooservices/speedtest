package stats

import (
	"testing"
	"time"
)

func TestBucketWidthKeepsPointsWithinTheLimit(t *testing.T) {
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		span                time.Duration
		max                 int
		wantWidth           float64
		wantBuckets, boundN int
	}{
		{24 * time.Hour, 48, 1800, 48, 48},
		{7 * 24 * time.Hour, 300, 2016, 300, 300},
		{30 * 24 * time.Hour, 300, 8640, 300, 300},
		{366 * 24 * time.Hour, 300, 105408, 300, 300},
		{90 * time.Second, 300, 1, 90, 300},
		{time.Hour, 7, 515, 7, 7},
	} {
		width, buckets := BucketWidth(from, from.Add(tc.span), tc.max)
		if width != tc.wantWidth || buckets != tc.wantBuckets || buckets > tc.boundN {
			t.Errorf("%v/%d: width %v buckets %d, want %v/%d", tc.span, tc.max, width, buckets, tc.wantWidth, tc.wantBuckets)
		}
		if float64(buckets)*width < tc.span.Seconds() {
			t.Errorf("%v/%d: the buckets do not cover the window", tc.span, tc.max)
		}
	}
	if _, buckets := BucketWidth(from, from, 10); buckets != 0 {
		t.Fatal("an empty window has no buckets")
	}
	if w, b := BucketWidth(from, from.Add(time.Hour), 0); b != DefaultMaxPoints || w < 1 {
		t.Fatalf("zero max points falls back to the default: %v %d", w, b)
	}
}

func TestSeriesExpressionAndValidation(t *testing.T) {
	if expr, err := seriesExpression(Ping, false); err != nil || expr != "r.ping_ms" {
		t.Fatalf("ping = %q %v", expr, err)
	}
	for _, m := range []Metric{Download, Upload} {
		if expr, err := seriesExpression(m, true); err != nil || expr == "" {
			t.Fatalf("%s percent = %q %v", m, expr, err)
		}
	}
	if _, err := seriesExpression(Ping, true); err == nil {
		t.Fatal("percent is only for down and up")
	}
	if _, err := seriesExpression("bogus", false); err == nil {
		t.Fatal("unknown metric accepted")
	}
	s := &Store{}
	for _, n := range []int{0, 301, -1} {
		if _, err := s.Series(t.Context(), Query{Metric: Download}, false, n); err == nil {
			t.Fatalf("maxPoints %d accepted", n)
		}
	}
	if items, err := s.Series(t.Context(), Query{Metric: Download, NodesSet: true}, false, 10); err != nil || len(items) != 0 {
		t.Fatalf("empty selection: %v %v", items, err)
	}
	from := time.Now()
	if items, err := s.Series(t.Context(), Query{Metric: Download, From: from, To: from}, false, 10); err != nil || len(items) != 0 {
		t.Fatalf("empty window: %v %v", items, err)
	}
}
