package report

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const maxDashboardLinkRunes = 512

// ErrInvalidDashboardURL marks permanent configuration input that cannot be
// repaired by retrying the same claimed digest period.
var ErrInvalidDashboardURL = errors.New("invalid dashboard URL")

// DigestMessage contains the safe dynamic fields used by daily and weekly notification templates.
type DigestMessage struct {
	Period          string
	Summary         string
	TelegramSummary string
	Link            string
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
		message.TelegramSummary = message.Summary
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
	message.TelegramSummary = formatTelegramDigestSummary(summary.Nodes)
	return message, nil
}

func formatTelegramDigestSummary(nodes []NodeSummary) string {
	var successes, failures int64
	for _, node := range nodes {
		successes += int64(node.Successes)
		failures += int64(node.Failures)
	}
	nodeLabel, testLabel := "nodes", "tests"
	if len(nodes) == 1 {
		nodeLabel = "node"
	}
	if successes+failures == 1 {
		testLabel = "test"
	}
	return fmt.Sprintf("%d %s, %d %s: %d successful, %d failed. Per-node counts and averages continue in the dashboard.",
		len(nodes), nodeLabel, successes+failures, testLabel, successes, failures)
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
		return "", fmt.Errorf("%w: contains surrounding whitespace", ErrInvalidDashboardURL)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", ErrInvalidDashboardURL
	}
	scheme := strings.ToLower(u.Scheme)
	unsafe := (scheme != "https" && scheme != "http") || u.Hostname() == "" || u.User != nil || u.Opaque != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#")
	if unsafe {
		return "", fmt.Errorf("%w: must be an absolute HTTP(S) base URL without credentials, query, or fragment", ErrInvalidDashboardURL)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if u.Path == "" {
		u.Path = "/"
	} else {
		u.Path += "/"
	}
	u.RawPath = ""
	u.Fragment = "/"
	link := u.String()
	if utf8.RuneCountInString(link) > maxDashboardLinkRunes {
		return "", fmt.Errorf("%w: exceeds the %d-character dashboard link limit", ErrInvalidDashboardURL, maxDashboardLinkRunes)
	}
	return link, nil
}

// IsDashboardLinkFor reports whether link is the generated dashboard route for baseURL.
func IsDashboardLinkFor(baseURL, link string) bool {
	expected, err := safeDashboardLink(baseURL)
	return err == nil && expected == link
}
