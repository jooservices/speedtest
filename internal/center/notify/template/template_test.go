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

func TestValidateForChannel(t *testing.T) {
	if err := ValidateForChannel("test.success", "webhook", `{"node":"{node}","download":"{download}"}`); err != nil {
		t.Fatalf("valid JSON webhook template: %v", err)
	}
	if err := ValidateForChannel("test.success", "webhook", `download for {node}`); !errors.Is(err, ErrInvalidTemplate) {
		t.Fatalf("plain text webhook accepted: %v", err)
	}
	if err := ValidateForChannel("test.success", "telegram", "Hello {node}"); err != nil {
		t.Fatalf("plain Telegram template: %v", err)
	}
}

func TestValidateRejectsMalformedAndCaseVariantPlaceholders(t *testing.T) {
	for _, body := range []string{"{Node}", "{unknown-name}", "{bad name}", "{{node}}", "{node"} {
		if err := Validate("test.success", body); err == nil {
			t.Errorf("malformed/unknown placeholder %q was accepted", body)
		}
	}
	if err := ValidateForChannel("test.success", "webhook", `{"event":"{event}","node":"{node}"}`); err != nil {
		t.Fatalf("valid webhook object structure rejected: %v", err)
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
	special := `node "quoted" \ slash` + "\nnext line"
	encoded, err := Render("webhook", `{"event":"{event}","node":"{node}"}`, Data{"event": "changed", "node": special})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]string
	if err := json.Unmarshal([]byte(encoded), &decoded); err != nil || decoded["node"] != special {
		t.Fatalf("webhook interpolation = %q, decoded=%q, err=%v", encoded, decoded["node"], err)
	}
	plainEncoded, err := Render("webhook", "Event for {node}", Data{"node": special})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(plainEncoded), &decoded); err != nil || decoded["text"] != "Event for "+special {
		t.Fatalf("webhook text interpolation = %q, decoded=%q, err=%v", plainEncoded, decoded["text"], err)
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
