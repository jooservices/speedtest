// Package catalog defines the notification and alert validation contract.
package catalog

import "slices"

// Channel describes destination validation and delivery prerequisites.
type Channel struct {
	Name           string
	Destination    string
	SecretRequired bool
}

var channels = []Channel{
	{"email", "email", false},
	{"telegram", "telegram", true},
	{"slack", "webhook", true},
	{"webhook", "webhook", true},
}

// Metric describes threshold restrictions and deliberate UI omissions.
type Metric struct {
	Name      string
	Max       float64 // Zero means no upper bound.
	Event     bool    // Events require operator =, value 1 and one test.
	UIOmitted bool
}

var metrics = []Metric{
	{"down_pct", 0, false, false}, {"up_pct", 0, false, false},
	{"ping", 0, false, false}, {"jitter", 0, false, false},
	{"loss", 100, false, false}, {"failed", 0, false, false},
	{"ip_changed", 0, true, false}, {"offline", 0, true, true},
}

// Operator includes the UI's deliberate subset of comparison operators.
type Operator struct {
	Name      string
	UIOmitted bool
}

var operators = []Operator{{"<", false}, {">", false}, {"<=", true}, {">=", true}, {"=", true}}

// Channels returns a copy so callers cannot mutate the shared contract.
func Channels() []Channel { return slices.Clone(channels) }

// Metrics returns a copy of the metric contract.
func Metrics() []Metric { return slices.Clone(metrics) }

// Operators returns a copy of the operator contract.
func Operators() []Operator { return slices.Clone(operators) }

// LookupChannel finds a supported notification channel.
func LookupChannel(name string) (Channel, bool) {
	for _, ch := range channels {
		if ch.Name == name {
			return ch, true
		}
	}
	return Channel{}, false
}

// ValidOperator reports whether a comparison operator is supported.
func ValidOperator(name string) bool {
	for _, op := range operators {
		if op.Name == name {
			return true
		}
	}
	return false
}

// ValidThreshold checks metric-specific restrictions after numeric validation.
func ValidThreshold(name, op string, value float64, tests int) bool {
	for _, m := range metrics {
		if m.Name == name {
			return (m.Max == 0 || value <= m.Max) && (!m.Event || (op == "=" && value == 1 && tests == 1))
		}
	}
	return false
}
