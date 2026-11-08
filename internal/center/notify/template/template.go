// Package template renders notification message bodies from catalog defaults
// and owner overrides without executing code or expanding unknown placeholders.
package template

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/jooservices/speedtest/internal/center/catalog"
)

var (
	// ErrUnknownPlaceholder is returned when a template references a token the event does not allow.
	ErrUnknownPlaceholder = errors.New("unknown placeholder")
	// ErrInvalidTemplate is returned for empty or oversized bodies.
	ErrInvalidTemplate = errors.New("invalid template")
	tokenPattern       = regexp.MustCompile(`\{([a-z0-9_]+)\}`)
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
		"count": "3", "link": "https://speedtest.example/#/alerts", "center": "https://speedtest.example",
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
	for _, name := range PlaceholdersIn(body) {
		if !catalog.ValidEventPlaceholder(eventKey, name) {
			return fmt.Errorf("%w: {%s}", ErrUnknownPlaceholder, name)
		}
	}
	return nil
}

// PlaceholdersIn lists unique placeholder names in appearance order.
func PlaceholdersIn(body string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range tokenPattern.FindAllStringSubmatch(body, -1) {
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// Render substitutes placeholders. Missing values become empty strings.
// Channel selects formatting: telegram escapes HTML specials in values;
// webhook keeps the JSON template body as text after substitution.
func Render(channel, body string, data Data) (string, error) {
	if strings.TrimSpace(body) == "" {
		return "", ErrInvalidTemplate
	}
	out := tokenPattern.ReplaceAllStringFunc(body, func(tok string) string {
		name := tok[1 : len(tok)-1]
		val := ""
		if data != nil {
			val = data[name]
		}
		if channel == "telegram" {
			return escapeHTML(val)
		}
		return val
	})
	if channel == "webhook" {
		if !json.Valid([]byte(out)) {
			// Owner templates may be plain text; wrap as {"text":"..."}.
			raw, err := json.Marshal(map[string]string{"text": out})
			if err != nil {
				return "", err
			}
			return string(raw), nil
		}
	}
	return out, nil
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
