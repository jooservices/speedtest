package alerts

import (
	"math"
	"reflect"
	"testing"

	"github.com/jooservices/speedtest/internal/center/agentapi"
)

func validInput() RuleInput {
	return RuleInput{Name: "  Slow download ", Metric: "down_pct", Op: "<", Value: 80, ForTests: 3,
		AllNodes: true, Severity: "warn", Channels: []string{"email"}, Enabled: true}
}

func TestNormalizeRule(t *testing.T) {
	tests := []struct {
		name string
		edit func(*RuleInput)
		want bool
	}{
		{name: "valid all nodes", want: true},
		{name: "trim name", edit: func(r *RuleInput) { r.Name = "  Name  " }, want: true},
		{name: "empty name", edit: func(r *RuleInput) { r.Name = "  " }},
		{name: "long name", edit: func(r *RuleInput) { r.Name = string(make([]byte, 201)) }},
		{name: "unknown metric", edit: func(r *RuleInput) { r.Metric = "other" }},
		{name: "unknown operator", edit: func(r *RuleInput) { r.Op = "!=" }},
		{name: "negative value", edit: func(r *RuleInput) { r.Value = -1 }},
		{name: "nan value", edit: func(r *RuleInput) { r.Value = math.NaN() }},
		{name: "infinite value", edit: func(r *RuleInput) { r.Value = math.Inf(1) }},
		{name: "loss over 100", edit: func(r *RuleInput) { r.Metric, r.Value = "loss", 101 }},
		{name: "tests below range", edit: func(r *RuleInput) { r.ForTests = 0 }},
		{name: "tests above range", edit: func(r *RuleInput) { r.ForTests = 1001 }},
		{name: "bad severity", edit: func(r *RuleInput) { r.Severity = "critical" }},
		{name: "no channels", edit: func(r *RuleInput) { r.Channels = nil }},
		{name: "too many channels", edit: func(r *RuleInput) { r.Channels = []string{"email", "telegram", "slack", "webhook", "email"} }},
		{name: "bad channel", edit: func(r *RuleInput) { r.Channels = []string{"smtp"} }},
		{name: "duplicate channel", edit: func(r *RuleInput) { r.Channels = []string{"email", "email"} }},
		{name: "scope all with IDs", edit: func(r *RuleInput) { r.NodeIDs = []string{"00000000-0000-4000-8000-000000000001"} }},
		{name: "empty explicit scope", edit: func(r *RuleInput) { r.AllNodes = false }},
		{name: "invalid node ID", edit: func(r *RuleInput) { r.AllNodes = false; r.NodeIDs = []string{"bad"} }},
		{name: "duplicate node IDs", edit: func(r *RuleInput) {
			r.AllNodes = false
			r.NodeIDs = []string{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000001"}
		}},
		{name: "normalize and sort IDs", edit: func(r *RuleInput) {
			r.AllNodes = false
			r.NodeIDs = []string{"00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000001"}
		}, want: true},
		{name: "IP change fixed threshold", edit: func(r *RuleInput) { r.Metric, r.Op, r.Value, r.ForTests = "ip_changed", "=", 1, 1 }, want: true},
		{name: "offline fixed threshold", edit: func(r *RuleInput) { r.Metric, r.Op, r.Value, r.ForTests = "offline", "=", 1, 1 }, want: true},
		{name: "IP change cannot use threshold", edit: func(r *RuleInput) { r.Metric, r.Op, r.Value, r.ForTests = "ip_changed", ">", 2, 2 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := validInput()
			if tc.edit != nil {
				tc.edit(&in)
			}
			got, err := NormalizeRule(in)
			if (err == nil) != tc.want {
				t.Fatalf("NormalizeRule() error = %v, want success %v", err, tc.want)
			}
			if !tc.want {
				return
			}
			if got.Name == "  Slow download " {
				t.Fatal("name was not normalized")
			}
			if tc.name == "normalize and sort IDs" && !reflect.DeepEqual(got.NodeIDs, []string{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002"}) {
				t.Fatalf("node IDs not sorted: %v", got.NodeIDs)
			}
		})
	}
}

func TestAdvanceStreak(t *testing.T) {
	tests := []struct {
		name        string
		count       int
		active, hit bool
		threshold   int
		want        streakTransition
	}{
		{name: "first hit", hit: true, threshold: 2, want: streakTransition{count: 1}},
		{name: "opens at threshold", count: 1, hit: true, threshold: 2, want: streakTransition{count: 2, active: true, opened: true}},
		{name: "stays open", count: 2, active: true, hit: true, threshold: 2, want: streakTransition{count: 2, active: true}},
		{name: "miss clears and resolves", count: 2, active: true, threshold: 2, want: streakTransition{resolved: true}},
		{name: "miss before opening", count: 1, threshold: 3, want: streakTransition{recovered: true}},
		{name: "offline one test", hit: true, threshold: 1, want: streakTransition{count: 1, active: true, opened: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := advanceStreak(tc.count, tc.active, tc.hit, tc.threshold); got != tc.want {
				t.Fatalf("advanceStreak() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestMetricMatches(t *testing.T) {
	down, up, ping, jitter, loss := 50.0, 20.0, 100.0, 5.0, 10.0
	result := agentapi.Result{Status: "ok", Down: &down, Up: &up, Ping: &ping, Jitter: &jitter, Loss: &loss}
	tests := []struct {
		name    string
		rule    Rule
		result  agentapi.Result
		expDown float64
		expUp   float64
		want    bool
	}{
		{name: "download percentage", rule: Rule{Metric: "down_pct", Op: "=", Value: 50}, result: result, expDown: 100, want: true},
		{name: "upload percentage", rule: Rule{Metric: "up_pct", Op: ">=", Value: 40}, result: result, expUp: 50, want: true},
		{name: "latency", rule: Rule{Metric: "ping", Op: ">", Value: 99}, result: result, want: true},
		{name: "jitter", rule: Rule{Metric: "jitter", Op: "<", Value: 6}, result: result, want: true},
		{name: "packet loss", rule: Rule{Metric: "loss", Op: "<=", Value: 10}, result: result, want: true},
		{name: "failed", rule: Rule{Metric: "failed"}, result: agentapi.Result{Status: "failed"}, want: true},
		{name: "rate limited ignored", rule: Rule{Metric: "failed"}, result: agentapi.Result{Status: "failed", FailureClass: "rate_limited"}},
		{name: "failed resets numeric", rule: Rule{Metric: "ping", Op: ">", Value: 1}, result: agentapi.Result{Status: "failed", Ping: &ping}},
		{name: "missing metric", rule: Rule{Metric: "jitter", Op: ">", Value: 1}, result: agentapi.Result{Status: "ok"}},
		{name: "zero expected speed", rule: Rule{Metric: "down_pct", Op: "<", Value: 1}, result: result},
		{name: "unknown metric", rule: Rule{Metric: "offline", Op: "=", Value: 1}, result: result},
		{name: "unknown operator", rule: Rule{Metric: "ping", Op: "!=", Value: 1}, result: result},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := metricMatches(tc.rule, tc.result, tc.expDown, tc.expUp); got != tc.want {
				t.Fatalf("metricMatches() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMetricObservationFailedResultLeavesQualityStreakUnobserved(t *testing.T) {
	_, observed := metricObservation(Rule{Metric: "ping", Op: ">", Value: 30}, agentapi.Result{Status: "failed"}, 0, 0)
	if observed {
		t.Fatal("a failed test without a quality measurement must not resolve the alert")
	}
	_, observed = metricObservation(Rule{Metric: "ping", Op: ">", Value: 30}, agentapi.Result{Status: "ok"}, 0, 0)
	if observed {
		t.Fatal("a successful test without the metric must leave the streak unchanged")
	}
}

func TestRuleText(t *testing.T) {
	tests := []struct{ metric, want string }{
		{"failed", "3 failed tests in a row"},
		{"down_pct", "Download < 50% of expected for 3 tests in a row"},
		{"up_pct", "Upload >= 50% of expected for 3 tests in a row"},
		{"ping", "Latency > 50 ms for 3 tests in a row"},
		{"jitter", "Jitter <= 50 ms for 3 tests in a row"},
		{"loss", "Packet loss = 50% for 3 tests in a row"},
	}
	for _, tc := range tests {
		t.Run(tc.metric, func(t *testing.T) {
			rule := Rule{Metric: tc.metric, Op: map[string]string{"down_pct": "<", "up_pct": ">=", "ping": ">", "jitter": "<=", "loss": "="}[tc.metric], Value: 50, ForTests: 3}
			if got := ruleText(rule); got != tc.want {
				t.Fatalf("ruleText() = %q, want %q", got, tc.want)
			}
		})
	}
}
