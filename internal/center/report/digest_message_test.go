package report

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
)

func TestAC1FormatDailyDigestIncludesNodeCountsAveragesAndDashboardRoute(t *testing.T) {
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
	if got.Link != "https://speedtest.example.test/#/" {
		t.Errorf("dashboard link = %q", got.Link)
	}
}

func TestAC1FormatDigestRetainsFullEmailDetailAndBuildsTelegramAggregate(t *testing.T) {
	const nodeCount = 160
	down, up := 125.25, 22.5
	nodes := make([]NodeSummary, 0, nodeCount)
	for i := range nodeCount {
		nodes = append(nodes, NodeSummary{
			Node:      fmt.Sprintf("router-%03d-%s", i, strings.Repeat("long&name", 8)),
			Successes: 2, Failures: 1, AvgDown: &down, AvgUp: &up,
		})
	}
	message, err := FormatDigest(PeriodSummary{
		Kind: DailyPeriod, LocalDate: "2026-11-17", Timezone: "UTC", Nodes: nodes,
	}, "https://speedtest.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(message.Summary, nodes[0].Node) || !strings.Contains(message.Summary, nodes[len(nodes)-1].Node) {
		t.Fatal("full digest summary omitted per-node details")
	}
	if !strings.Contains(message.Summary, "avg down 125.25 Mbps, avg up 22.50 Mbps") {
		t.Fatal("full digest summary omitted per-node averages")
	}
	if !strings.Contains(message.TelegramSummary, "160 nodes, 480 tests: 320 successful, 160 failed") {
		t.Fatalf("Telegram aggregate = %q", message.TelegramSummary)
	}
	if !strings.Contains(message.TelegramSummary, "Per-node counts and averages continue in the dashboard") {
		t.Fatalf("Telegram continuation is missing: %q", message.TelegramSummary)
	}
	if strings.Contains(message.TelegramSummary, nodes[0].Node) || strings.Contains(message.TelegramSummary, nodes[len(nodes)-1].Node) {
		t.Fatal("Telegram aggregate contains user-controlled node names")
	}
	if len(message.Summary) <= 4096 {
		t.Fatalf("test fixture summary has only %d bytes; want a realistic oversized digest", len(message.Summary))
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
		want error
	}{
		{name: "unsupported period", data: PeriodSummary{Kind: "monthly", LocalDate: "2026-11-17", Timezone: "UTC", Nodes: []NodeSummary{{}}}, url: "https://speedtest.example.test"},
		{name: "invalid date", data: PeriodSummary{Kind: DailyPeriod, LocalDate: "not-a-date", Timezone: "UTC", Nodes: []NodeSummary{{}}}, url: "https://speedtest.example.test"},
		{name: "weekly date must be Monday", data: PeriodSummary{Kind: WeeklyPeriod, LocalDate: "2026-11-17", Timezone: "UTC", Nodes: []NodeSummary{{}}}, url: "https://speedtest.example.test"},
		{name: "missing URL", data: valid, url: "", want: ErrInvalidDashboardURL},
		{name: "malformed URL", data: valid, url: "https://%", want: ErrInvalidDashboardURL},
		{name: "surrounding whitespace", data: valid, url: " https://speedtest.example.test", want: ErrInvalidDashboardURL},
		{name: "unsafe scheme", data: valid, url: "javascript:alert(1)", want: ErrInvalidDashboardURL},
		{name: "missing host", data: valid, url: "https:///dashboard", want: ErrInvalidDashboardURL},
		{name: "URL credentials", data: valid, url: "https://owner:secret@speedtest.example.test", want: ErrInvalidDashboardURL},
		{name: "query string", data: valid, url: "https://speedtest.example.test?next=other", want: ErrInvalidDashboardURL},
		{name: "empty query", data: valid, url: "https://speedtest.example.test?", want: ErrInvalidDashboardURL},
		{name: "fragment", data: valid, url: "https://speedtest.example.test/#/alerts", want: ErrInvalidDashboardURL},
		{name: "empty fragment", data: valid, url: "https://speedtest.example.test/#", want: ErrInvalidDashboardURL},
		{name: "overlong dashboard URL", data: valid, url: "https://speedtest.example.test/" + strings.Repeat("x", 600), want: ErrInvalidDashboardURL},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := FormatDigest(test.data, test.url); err == nil {
				t.Fatal("FormatDigest() accepted invalid input")
			} else if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("FormatDigest() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestAC1DashboardLinkMustMatchConfiguredCenterURL(t *testing.T) {
	for _, test := range []struct {
		baseURL string
		link    string
		want    bool
	}{
		{baseURL: "https://speedtest.example.test", link: "https://speedtest.example.test/#/", want: true},
		{baseURL: "http://speedtest.example.test/center/", link: "http://speedtest.example.test/center/#/", want: true},
		{baseURL: "https://speedtest.example.test", link: "https://speedtest.example.test/#/alerts"},
		{baseURL: "https://speedtest.example.test", link: "https://owner:secret@speedtest.example.test/#/"},
		{baseURL: "https://speedtest.example.test", link: "https://attacker.example/#/"},
		{baseURL: "https://owner:secret@speedtest.example.test", link: "https://owner:secret@speedtest.example.test/#/"},
		{baseURL: "https://speedtest.example.test?next=other", link: "https://speedtest.example.test?next=other/#/"},
	} {
		if got := IsDashboardLinkFor(test.baseURL, test.link); got != test.want {
			t.Errorf("IsDashboardLinkFor(%q, %q) = %t, want %t", test.baseURL, test.link, got, test.want)
		}
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
			{Node: "single", Successes: 1, AvgDown: &average, AvgUp: &average},
		},
	}, "https://speedtest.example.test")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"avg down n/a, avg up n/a", "avg down n/a, avg up 0.00 Mbps", "Unnamed node", "single — 1 test:"} {
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
	if got != "http://speedtest.example.test/center/#/" {
		t.Errorf("dashboard link = %q", got)
	}
}
