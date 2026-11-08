package template

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestValidateAndPlaceholders(t *testing.T) {
	if err := Validate("test.success", "Hello {node} {download}"); err != nil {
		t.Fatal(err)
	}
	if err := Validate("test.success", "Bad {secret}"); !errors.Is(err, ErrUnknownPlaceholder) {
		t.Fatalf("unknown: %v", err)
	}
	if err := Validate("missing", "Hi {node}"); !errors.Is(err, ErrInvalidTemplate) {
		t.Fatalf("missing event: %v", err)
	}
	if err := Validate("test.success", ""); !errors.Is(err, ErrInvalidTemplate) {
		t.Fatalf("empty: %v", err)
	}
	if err := Validate("test.success", strings.Repeat("x", maxBodyRunes+1)); !errors.Is(err, ErrInvalidTemplate) {
		t.Fatalf("oversized: %v", err)
	}
	got := PlaceholdersIn("{node} and {node} then {link}")
	if len(got) != 2 || got[0] != "node" || got[1] != "link" {
		t.Fatalf("placeholders = %v", got)
	}
}

func TestRenderChannels(t *testing.T) {
	data := Data{"node": "A <B>", "event": "Warn", "severity": "warn", "severity_icon": SeverityIcon("warn"), "since": "t", "link": "https://x"}
	body := "{severity_icon} {event} — {node}\n{link}"
	plain, err := Render("email", body, data)
	if err != nil || !strings.Contains(plain, "A <B>") || !strings.Contains(plain, "⚠️") {
		t.Fatalf("email = %q %v", plain, err)
	}
	html, err := Render("telegram", body, data)
	if err != nil || !strings.Contains(html, "A &lt;B&gt;") || strings.Contains(html, "A <B>") {
		t.Fatalf("telegram = %q %v", html, err)
	}
	slack, err := Render("slack", body, data)
	if err != nil || slack == "" {
		t.Fatalf("slack = %q %v", slack, err)
	}
	raw, err := Render("webhook", `{"text":"{event} on {node}"}`, data)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]string
	if err := json.Unmarshal([]byte(raw), &obj); err != nil || obj["text"] == "" {
		t.Fatalf("webhook json = %q %v", raw, err)
	}
	wrapped, err := Render("webhook", "plain {node}", data)
	if err != nil || !json.Valid([]byte(wrapped)) {
		t.Fatalf("webhook wrap = %q %v", wrapped, err)
	}
	if _, err := Render("email", "   ", nil); !errors.Is(err, ErrInvalidTemplate) {
		t.Fatalf("blank: %v", err)
	}
}

func TestSampleDefaultsAndIcons(t *testing.T) {
	s := Sample("test.success")
	if s["node"] == "" || s["download_ok"] == "" || s["severity_icon"] == "" {
		t.Fatalf("sample incomplete: %#v", s)
	}
	if SeverityIcon("crit") != "🚨" || SeverityIcon("warn") != "⚠️" || SeverityIcon("info") != "ℹ️" {
		t.Fatal("icons")
	}
	body, ok := DefaultBody("test.success", "telegram")
	if !ok || !strings.Contains(body, "{download}") {
		t.Fatalf("default = %q %v", body, ok)
	}
	if _, ok := DefaultBody("missing", "email"); ok {
		t.Fatal("missing default")
	}
	chs := FormatChannels()
	if len(chs) != 4 {
		t.Fatalf("channels = %v", chs)
	}
}
