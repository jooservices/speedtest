package report

import (
	"math"
	"strings"
	"testing"
)

func TestAC1FormatDailyDigestIncludesNodeCountsAveragesAndSafeDashboardLink(t *testing.T) {
	down, up := 125.25, 22.5
	got, err := FormatDigest(PeriodSummary{
		Kind: DailyPeriod, LocalDate: "2026-11-17", Timezone: "Asia/Ho_Chi_Minh",
		Nodes: []NodeSummary{
			{Node: "Node <alpha>\nforged", Successes: 3, Failures: 1, AvgDown: &down, AvgUp: &up},
			{Node: "Beta", Successes: 2, AvgDown: &down},
		},
	}, "https://speedtest.example.test/")
	if err != nil {
		t.Fatal(err)
	}
	if got.Period != "Daily period: 2026-11-17 (Asia/Ho_Chi_Minh)" {
		t.Errorf("period = %q", got.Period)
	}
	for _, want := range []string{
		"Node <alpha> forged — 4 tests: 3 successful, 1 failed; avg down 125.25 Mbps, avg up 22.50 Mbps",
		"Beta — 2 tests: 2 successful, 0 failed; avg down 125.25 Mbps, avg up n/a",
	} {
		if !strings.Contains(got.Summary, want) {
			t.Errorf("summary %q does not contain %q", got.Summary, want)
		}
	}
	if strings.Contains(got.Summary, "\nforged") {
		t.Fatal("node name introduced a new message line")
	}
	if got.Link != "https://speedtest.example.test/#/dashboard" {
		t.Errorf("dashboard link = %q", got.Link)
	}
}

func TestAC1FormatWeeklyEmptyDigestIsClearWithoutDashboardURL(t *testing.T) {
	got, err := FormatDigest(PeriodSummary{
		Kind: WeeklyPeriod, LocalDate: "2026-11-09", Timezone: "UTC", Nodes: []NodeSummary{},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Period != "Weekly period: 2026-11-09 through 2026-11-15 (UTC)" {
		t.Errorf("period = %q", got.Period)
	}
	if got.Summary != "No speed test results were recorded for this period." {
		t.Errorf("empty summary = %q", got.Summary)
	}
	if got.Link != "" {
		t.Errorf("empty period link = %q, want no link", got.Link)
	}
}

func TestAC1FormatDigestRejectsInvalidPeriodAndUnsafeDashboardURL(t *testing.T) {
	valid := PeriodSummary{Kind: DailyPeriod, LocalDate: "2026-11-17", Timezone: "UTC", Nodes: []NodeSummary{{Node: "node", Successes: 1}}}
	for _, test := range []struct {
		name string
		data PeriodSummary
		url  string
	}{
		{name: "unsupported period", data: PeriodSummary{Kind: "monthly", LocalDate: "2026-11-17", Timezone: "UTC", Nodes: []NodeSummary{{}}}, url: "https://speedtest.example.test"},
		{name: "invalid date", data: PeriodSummary{Kind: DailyPeriod, LocalDate: "not-a-date", Timezone: "UTC", Nodes: []NodeSummary{{}}}, url: "https://speedtest.example.test"},
		{name: "weekly date must be Monday", data: PeriodSummary{Kind: WeeklyPeriod, LocalDate: "2026-11-17", Timezone: "UTC", Nodes: []NodeSummary{{}}}, url: "https://speedtest.example.test"},
		{name: "missing URL", data: valid, url: ""},
		{name: "malformed URL", data: valid, url: "https://%"},
		{name: "surrounding whitespace", data: valid, url: " https://speedtest.example.test"},
		{name: "unsafe scheme", data: valid, url: "javascript:alert(1)"},
		{name: "missing host", data: valid, url: "https:///dashboard"},
		{name: "URL credentials", data: valid, url: "https://owner:secret@speedtest.example.test"},
		{name: "query string", data: valid, url: "https://speedtest.example.test?next=other"},
		{name: "fragment", data: valid, url: "https://speedtest.example.test/#/alerts"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := FormatDigest(test.data, test.url); err == nil {
				t.Fatal("FormatDigest() accepted invalid input")
			}
		})
	}
}

func TestAC1DigestAverageFormattingHandlesMissingAndNonFiniteValues(t *testing.T) {
	nan, infinity, average := math.NaN(), math.Inf(1), 0.0
	got, err := FormatDigest(PeriodSummary{
		Kind: DailyPeriod, LocalDate: "2026-11-17", Timezone: "UTC",
		Nodes: []NodeSummary{
			{Node: "missing", AvgDown: nil, AvgUp: &nan},
			{Node: "infinite", AvgDown: &infinity, AvgUp: &average},
			{Node: "", AvgDown: &average, AvgUp: &average},
		},
	}, "https://speedtest.example.test")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"avg down n/a, avg up n/a", "avg down n/a, avg up 0.00 Mbps", "Unnamed node"} {
		if !strings.Contains(got.Summary, want) {
			t.Errorf("summary %q does not contain %q", got.Summary, want)
		}
	}
}

func TestAC1DashboardLinkUsesConfiguredHTTPBasePath(t *testing.T) {
	got, err := safeDashboardLink("http://speedtest.example.test/center/")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://speedtest.example.test/center/#/dashboard" {
		t.Errorf("dashboard link = %q", got)
	}
}
