package report

import (
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"
)

// DigestMessage contains the safe dynamic fields used by daily and weekly notification templates.
type DigestMessage struct {
	Period  string
	Summary string
	Link    string
}

// FormatDigest builds an owner-local period label and one line per summarized node.
// A populated digest requires a validated absolute dashboard URL; an empty period remains useful without one.
func FormatDigest(summary PeriodSummary, dashboardBaseURL string) (DigestMessage, error) {
	period, err := digestPeriodLabel(summary)
	if err != nil {
		return DigestMessage{}, err
	}
	message := DigestMessage{Period: period}
	if len(summary.Nodes) == 0 {
		message.Summary = "No speed test results were recorded for this period."
		return message, nil
	}
	message.Link, err = safeDashboardLink(dashboardBaseURL)
	if err != nil {
		return DigestMessage{}, err
	}
	lines := make([]string, 0, len(summary.Nodes))
	for _, node := range summary.Nodes {
		lines = append(lines, formatDigestNode(node))
	}
	message.Summary = strings.Join(lines, "\n")
	return message, nil
}

func digestPeriodLabel(summary PeriodSummary) (string, error) {
	start, err := time.Parse(digestDateLayout, summary.LocalDate)
	if err != nil || start.Format(digestDateLayout) != summary.LocalDate {
		return "", fmt.Errorf("invalid digest period date")
	}
	period, err := NewPeriod(summary.Kind, summary.LocalDate, summary.Timezone)
	if err != nil {
		return "", err
	}
	if period.Kind() == DailyPeriod {
		return fmt.Sprintf("Daily period: %s (%s)", period.LocalDate(), period.Timezone()), nil
	}
	end := start.AddDate(0, 0, 6).Format(digestDateLayout)
	return fmt.Sprintf("Weekly period: %s through %s (%s)", period.LocalDate(), end, period.Timezone()), nil
}

func formatDigestNode(node NodeSummary) string {
	name := strings.Join(strings.Fields(node.Node), " ")
	if name == "" {
		name = "Unnamed node"
	}
	tests := node.Successes + node.Failures
	countLabel := "tests"
	if tests == 1 {
		countLabel = "test"
	}
	return fmt.Sprintf("%s — %d %s: %d successful, %d failed; avg down %s, avg up %s",
		name, tests, countLabel, node.Successes, node.Failures, formatDigestAverage(node.AvgDown), formatDigestAverage(node.AvgUp))
}

func formatDigestAverage(value *float64) string {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) {
		return "n/a"
	}
	return fmt.Sprintf("%.2f Mbps", *value)
}

func safeDashboardLink(raw string) (string, error) {
	if raw != strings.TrimSpace(raw) {
		return "", fmt.Errorf("dashboard URL contains surrounding whitespace")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid dashboard URL")
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "https" && scheme != "http") || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("dashboard URL must be an absolute HTTP(S) base URL without credentials, query, or fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if u.Path == "" {
		u.Path = "/"
	} else {
		u.Path += "/"
	}
	u.RawPath = ""
	u.Fragment = "/"
	return u.String(), nil
}
