// Package alerts evaluates alert rules incrementally and serves their durable events.
package alerts

import (
	"errors"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

var (
	ErrNotFound = errors.New("alert resource not found")
	ErrStale    = errors.New("alert entity tag is stale")
	ErrConflict = errors.New("alert state conflict")
	ErrInvalid  = errors.New("invalid alert input")
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// RuleInput is the complete owner-editable configuration for one rule.
// AllNodes selects every node; otherwise NodeIDs is a non-empty explicit scope.
type RuleInput struct {
	Name     string
	Metric   string
	Op       string
	Value    float64
	ForTests int
	AllNodes bool
	NodeIDs  []string
	Severity string
	Channels []string
	Enabled  bool
}

// Rule is the public rule representation. Nodes is either "all" or a UUID list.
type Rule struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Metric   string   `json:"metric"`
	Op       string   `json:"op"`
	Value    float64  `json:"value"`
	ForTests int      `json:"forTests"`
	Nodes    any      `json:"nodes"`
	Severity string   `json:"severity"`
	Channels []string `json:"channels"`
	Enabled  bool     `json:"enabled"`
	Revision int64    `json:"-"`
	NodeIDs  []string `json:"-"`
	AllNodes bool     `json:"-"`
}

// Event is an alert's durable history row.
type Event struct {
	ID         string  `json:"id"`
	RuleID     *string `json:"ruleId"`
	NodeID     string  `json:"nodeId"`
	Severity   string  `json:"severity"`
	TS         string  `json:"ts"`
	ResolvedAt *string `json:"resolvedAt"`
	Title      string  `json:"title"`
	Detail     string  `json:"detail"`
	Acked      bool    `json:"acked"`
}

// EventFilter selects alert activity without changing its stable ordering.
type EventFilter struct {
	State    string
	Severity string
	NodeID   string
}

var metrics = map[string]bool{
	"down_pct": true, "up_pct": true, "ping": true, "jitter": true,
	"loss": true, "failed": true, "ip_changed": true, "offline": true,
}

var operators = map[string]bool{"<": true, ">": true, "<=": true, ">=": true, "=": true}
var severities = map[string]bool{"info": true, "warn": true, "crit": true}
var channels = map[string]bool{"email": true, "telegram": true, "slack": true, "webhook": true}

// NormalizeRule validates the OpenAPI rule constraints and returns stable IDs
// and whitespace without changing the requested rule semantics.
func NormalizeRule(in RuleInput) (RuleInput, error) {
	in.Name = strings.TrimSpace(in.Name)
	if utf8.RuneCountInString(in.Name) < 1 || utf8.RuneCountInString(in.Name) > 200 ||
		!metrics[in.Metric] || !operators[in.Op] || math.IsNaN(in.Value) || math.IsInf(in.Value, 0) || in.Value < 0 ||
		in.ForTests < 1 || in.ForTests > 1000 || !severities[in.Severity] || len(in.Channels) < 1 || len(in.Channels) > 4 {
		return RuleInput{}, ErrInvalid
	}
	if in.Metric == "loss" && in.Value > 100 {
		return RuleInput{}, ErrInvalid
	}
	if (in.Metric == "ip_changed" || in.Metric == "offline") && (in.Op != "=" || in.Value != 1 || in.ForTests != 1) {
		return RuleInput{}, ErrInvalid
	}
	if in.AllNodes {
		if len(in.NodeIDs) != 0 {
			return RuleInput{}, ErrInvalid
		}
	} else {
		if len(in.NodeIDs) < 1 || len(in.NodeIDs) > 100 {
			return RuleInput{}, ErrInvalid
		}
		seen := make(map[string]bool, len(in.NodeIDs))
		for i, id := range in.NodeIDs {
			id = strings.ToLower(strings.TrimSpace(id))
			if !uuidPattern.MatchString(id) || seen[id] {
				return RuleInput{}, ErrInvalid
			}
			seen[id] = true
			in.NodeIDs[i] = id
		}
		sort.Strings(in.NodeIDs)
	}
	seenChannels := make(map[string]bool, len(in.Channels))
	for _, channel := range in.Channels {
		if !channels[channel] || seenChannels[channel] {
			return RuleInput{}, ErrInvalid
		}
		seenChannels[channel] = true
	}
	in.Channels = append([]string(nil), in.Channels...)
	return in, nil
}

func ruleView(r Rule) Rule {
	if r.AllNodes {
		r.Nodes = "all"
	} else {
		r.Nodes = append([]string(nil), r.NodeIDs...)
	}
	return r
}

type streakTransition struct {
	count    int
	active   bool
	opened   bool
	resolved bool
}

func advanceStreak(count int, active, hit bool, threshold int) streakTransition {
	if !hit {
		return streakTransition{resolved: active}
	}
	if count < threshold {
		count++
	}
	opened := !active && count >= threshold
	return streakTransition{count: count, active: active || opened, opened: opened}
}

func compare(value float64, op string, threshold float64) bool {
	switch op {
	case "<":
		return value < threshold
	case ">":
		return value > threshold
	case "<=":
		return value <= threshold
	case ">=":
		return value >= threshold
	case "=":
		return value == threshold
	default:
		return false
	}
}

func ruleText(r Rule) string {
	if r.Metric == "failed" {
		return strconv.Itoa(r.ForTests) + " failed tests in a row"
	}
	labels := map[string]string{"down_pct": "Download", "up_pct": "Upload", "ping": "Latency", "jitter": "Jitter", "loss": "Packet loss"}
	units := map[string]string{"down_pct": "% of expected", "up_pct": "% of expected", "ping": " ms", "jitter": " ms", "loss": "%"}
	return labels[r.Metric] + " " + r.Op + " " + strconv.FormatFloat(r.Value, 'f', -1, 64) + units[r.Metric] + " for " + strconv.Itoa(r.ForTests) + " tests in a row"
}
