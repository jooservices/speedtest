// Package template renders notification message bodies from catalog defaults
// and owner overrides without executing code or expanding unknown placeholders.
package template

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jooservices/speedtest/internal/center/catalog"
)

var (
	// ErrUnknownPlaceholder is returned when a template references a token the event does not allow.
	ErrUnknownPlaceholder = errors.New("unknown placeholder")
	// ErrInvalidTemplate is returned for empty or oversized bodies.
	ErrInvalidTemplate = errors.New("invalid template")
)

const maxBodyRunes = 4000

// Data supplies placeholder values for rendering and preview.
type Data map[string]string

// Sample returns deterministic preview values for one catalog event.
func Sample(eventKey string) Data {
	e, ok := catalog.LookupEvent(eventKey)
	title := "Sample event"
	if ok {
		title = e.Title
	}
	return Data{
		"node": "Viettel Fast 50", "label": "home", "event": title, "severity": e.Severity,
		"severity_icon": SeverityIcon(e.Severity), "value": "180", "expected": "300", "pct": "60",
		"threshold": "65", "download": "180 Mbps", "upload": "90 Mbps", "ping": "12", "jitter": "3",
		"loss": "0.2", "download_limit": "65% of expected", "upload_limit": "40% of expected",
		"ping_limit": "30", "jitter_limit": "10", "loss_limit": "2",
		"download_ok": "no", "upload_ok": "yes", "ping_ok": "yes", "jitter_ok": "yes", "loss_ok": "yes",
		"server": "HCMC-1", "isp": "Viettel", "ip": "203.0.113.10", "old_ip": "198.51.100.8",
		"city": "Ho Chi Minh City", "since": "2026-11-08T01:00:00Z", "duration": "45m",
		"count": "3", "failure_reason": "speed test timed out", "link": "https://speedtest.example/#/alerts", "center": "https://speedtest.example",
	}
}

// SeverityIcon is the emoji shown at the start of Telegram and plain templates.
func SeverityIcon(severity string) string {
	switch severity {
	case "crit":
		return "🚨"
	case "warn":
		return "⚠️"
	default:
		return "ℹ️"
	}
}

// Validate checks that body is non-empty, bounded, and only uses allowed placeholders.
func Validate(eventKey, body string) error {
	if strings.TrimSpace(body) == "" || len([]rune(body)) > maxBodyRunes {
		return ErrInvalidTemplate
	}
	if !catalog.ValidEventKey(eventKey) {
		return ErrInvalidTemplate
	}
	names, valid := placeholderNames(body)
	if !valid {
		return ErrInvalidTemplate
	}
	for _, name := range names {
		if !catalog.ValidEventPlaceholder(eventKey, name) {
			return fmt.Errorf("%w: {%s}", ErrUnknownPlaceholder, name)
		}
	}
	return nil
}

// ValidateForChannel applies JSON structure rules to webhook templates as well as placeholder validation.
func ValidateForChannel(eventKey, channel, body string) error {
	if err := Validate(eventKey, body); err != nil {
		return err
	}
	if channel == "webhook" && !validWebhookTemplate(body) {
		return ErrInvalidTemplate
	}
	return nil
}

// PlaceholdersIn lists unique placeholder names in appearance order.
func PlaceholdersIn(body string) []string {
	seen := map[string]bool{}
	var out []string
	spans, valid := placeholderSpans(body)
	if !valid {
		return out
	}
	for _, span := range spans {
		name := span.name
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

type placeholderSpan struct {
	start int
	end   int
	name  string
}

func placeholderNames(body string) ([]string, bool) {
	spans, valid := placeholderSpans(body)
	names := make([]string, 0, len(spans))
	for _, span := range spans {
		names = append(names, span.name)
	}
	return names, valid
}

func placeholderSpans(body string) ([]placeholderSpan, bool) {
	var stack []int
	var spans []placeholderSpan
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '{':
			stack = append(stack, i)
		case '}':
			if len(stack) == 0 {
				return nil, false
			}
			start := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			content := body[start+1 : i]
			if placeholderCandidate(content) {
				spans = append(spans, placeholderSpan{start: start, end: i + 1, name: content})
			}
		}
	}
	if len(stack) > 0 {
		return nil, false
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	return spans, true
}

func placeholderCandidate(content string) bool {
	content = strings.TrimSpace(content)
	return content != "" && !strings.ContainsAny(content, `":,[]`)
}

// Render substitutes placeholders. Missing values become empty strings.
// Channel selects formatting: Telegram values are HTML-escaped and webhook
// values are JSON-string-escaped before they enter the configured payload.
func Render(channel, body string, data Data) (string, error) {
	if strings.TrimSpace(body) == "" {
		return "", ErrInvalidTemplate
	}
	spans, valid := placeholderSpans(body)
	if !valid {
		return "", ErrInvalidTemplate
	}
	webhookJSON := channel == "webhook" && validWebhookTemplate(body)
	var out strings.Builder
	last := 0
	for _, span := range spans {
		out.WriteString(body[last:span.start])
		val := ""
		if data != nil {
			val = data[span.name]
		}
		if channel == "telegram" {
			val = escapeHTML(val)
		} else if webhookJSON {
			val = escapeJSONString(val)
		}
		out.WriteString(val)
		last = span.end
	}
	out.WriteString(body[last:])
	renderedBody := out.String()
	if channel == "webhook" {
		if !json.Valid([]byte(renderedBody)) {
			// Owner templates may be plain text; wrap as {"text":"..."}.
			raw, err := json.Marshal(map[string]string{"text": renderedBody})
			if err != nil {
				return "", err
			}
			return string(raw), nil
		}
	}
	return renderedBody, nil
}

func validWebhookTemplate(body string) bool {
	spans, valid := placeholderSpans(body)
	if !valid {
		return false
	}
	var probe strings.Builder
	last := 0
	for _, span := range spans {
		probe.WriteString(body[last:span.start])
		probe.WriteString("sample")
		last = span.end
	}
	probe.WriteString(body[last:])
	return json.Valid([]byte(probe.String()))
}

// DefaultBody returns the catalog default for event/channel.
func DefaultBody(eventKey, channel string) (string, bool) {
	e, ok := catalog.LookupEvent(eventKey)
	if !ok {
		return "", false
	}
	body, ok := e.Templates[channel]
	return body, ok
}

// FormatChannels lists supported template channels in stable order.
func FormatChannels() []string {
	names := make([]string, 0, len(catalog.Channels()))
	for _, ch := range catalog.Channels() {
		names = append(names, ch.Name)
	}
	sort.Strings(names)
	return names
}

func escapeHTML(s string) string {
	r := strings.NewReplacer(`&`, "&amp;", `<`, "&lt;", `>`, "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

func escapeJSONString(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw[1 : len(raw)-1])
}
