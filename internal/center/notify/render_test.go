package notify

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

type renderDB struct {
	event   []any
	tmpl    string
	tmplErr error
}

func TestAC1DigestDispatchBoundsTelegramAndKeepsCompleteEmailDetail(t *testing.T) {
	id := "digest-large"
	fullSummary := strings.Repeat("Router <long-name> — 3 tests: 2 successful, 1 failed; avg down 125.25 Mbps, avg up 22.50 Mbps\n", 150)
	telegramSummary := "150 nodes, 450 tests: 300 successful, 150 failed. Per-node counts and averages continue in the dashboard."
	payload, err := json.Marshal(map[string]string{
		"period": "Daily period: 2026-11-17 (UTC)", "summary": fullSummary,
		"telegram_summary": telegramSummary, "link": "https://speedtest.example.test/#/",
	})
	if err != nil {
		t.Fatal(err)
	}
	db := &renderDB{event: []any{"Daily summary", "Command center", time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC), "digest.daily", payload}}
	store := &Store{}
	telegram, err := store.renderDelivery(context.Background(), db, Delivery{EventID: &id, Channel: "telegram"}, "https://speedtest.example.test")
	if err != nil {
		t.Fatal("render oversized Telegram digest:", err)
	}
	if got := telegramTextUnits(telegram); got > 4096 {
		t.Fatalf("Telegram message has %d rendered characters, want at most 4096", got)
	}
	if !strings.Contains(telegram, telegramSummary) || !strings.Contains(telegram, "https://speedtest.example.test/#/") {
		t.Fatalf("bounded Telegram summary lacks aggregate or dashboard continuation: %q", telegram)
	}
	if strings.Contains(telegram, "Router &lt;long-name&gt;") {
		t.Fatal("Telegram digest unexpectedly included per-node details")
	}
	mail, err := store.renderDelivery(context.Background(), db, Delivery{EventID: &id, Channel: "email"}, "https://speedtest.example.test")
	if err != nil {
		t.Fatal("render full e-mail digest:", err)
	}
	if !strings.Contains(mail, fullSummary) || utf8.RuneCountInString(mail) <= 4096 {
		t.Fatal("e-mail digest did not retain the complete per-node details")
	}

	db.tmpl = strings.Repeat("{summary}", 60)
	boundedFallback, err := store.renderDelivery(context.Background(), db, Delivery{EventID: &id, Channel: "telegram"}, "https://speedtest.example.test")
	if err != nil {
		t.Fatal("render oversized custom Telegram template:", err)
	}
	if utf8.RuneCountInString(boundedFallback) > 4096 ||
		!strings.Contains(boundedFallback, telegramSummary) ||
		!strings.Contains(boundedFallback, "Full per-node details and averages continue in the dashboard") {
		t.Fatalf("oversized custom template did not use a bounded continuation: %q", boundedFallback)
	}
}

func TestAC2EmptyDigestDispatchDoesNotUseUnsafeSettingsLinkFallback(t *testing.T) {
	id := "digest-empty"
	payload, err := json.Marshal(map[string]string{
		"period":  "Daily period: 2026-11-17 (UTC)",
		"summary": "No speed test results were recorded for this period.",
		"link":    "", "center": "https://owner:secret@wrong.example/#/alerts",
	})
	if err != nil {
		t.Fatal(err)
	}
	db := &renderDB{event: []any{"Daily summary", "Command center", time.Unix(1, 0).UTC(), "digest.daily", payload}}
	text, err := (&Store{}).renderDelivery(context.Background(), db, Delivery{EventID: &id, Channel: "email"}, "https://owner:secret@wrong.example")
	if err != nil {
		t.Fatal("render empty digest:", err)
	}
	if !strings.Contains(text, "No speed test results were recorded for this period.") {
		t.Fatalf("empty digest message was lost: %q", text)
	}
	if strings.Contains(text, "owner:secret") || strings.Contains(text, "wrong.example") || strings.Contains(text, "/#/alerts") {
		t.Fatalf("empty digest exposed unsafe or generic dashboard link: %q", text)
	}
}

func TestAC2DigestDispatchDropsDashboardLinkForDifferentCenterURL(t *testing.T) {
	id := "digest-stale-link"
	payload, err := json.Marshal(map[string]string{
		"period":  "Daily period: 2026-11-17 (UTC)",
		"summary": "1 node, 1 test: 1 successful, 0 failed.",
		"link":    "https://old-center.example/#/",
	})
	if err != nil {
		t.Fatal(err)
	}
	db := &renderDB{event: []any{"Daily summary", "Command center", time.Unix(1, 0).UTC(), "digest.daily", payload}}
	text, err := (&Store{}).renderDelivery(context.Background(), db, Delivery{EventID: &id, Channel: "email"}, "https://new-center.example")
	if err != nil {
		t.Fatal("render digest with stale center link:", err)
	}
	if strings.Contains(text, "old-center.example") || strings.Contains(text, "new-center.example") {
		t.Fatalf("digest included a link that did not match current settings: %q", text)
	}
}

func TestAC1DigestTelegramLimitCountsSupplementaryCharacters(t *testing.T) {
	id := "digest-unicode-template"
	payload, err := json.Marshal(map[string]string{
		"period":           "Daily period: 2026-11-17 (UTC)",
		"summary":          "2 nodes, 2 tests: 2 successful, 0 failed. Per-node counts and averages continue in the dashboard.",
		"telegram_summary": "2 nodes, 2 tests: 2 successful, 0 failed. Per-node counts and averages continue in the dashboard.",
		"link":             "https://speedtest.example.test/#/",
	})
	if err != nil {
		t.Fatal(err)
	}
	db := &renderDB{
		event: []any{"Daily summary", "Command center", time.Unix(1, 0).UTC(), "digest.daily", payload},
		tmpl:  strings.Repeat("😀", 2500),
	}
	message, err := (&Store{}).renderDelivery(context.Background(), db, Delivery{EventID: &id, Channel: "telegram"}, "https://speedtest.example.test")
	if err != nil {
		t.Fatal("render supplementary-character template:", err)
	}
	if telegramTextUnits(message) > 4096 || !strings.Contains(message, "Open dashboard") {
		t.Fatalf("supplementary-character template did not fall back safely: units=%d message=%q", telegramTextUnits(message), message)
	}
}

func (r *renderDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	if strings.Contains(sql, "notification_templates") {
		if r.tmplErr != nil {
			return memRow{err: r.tmplErr}
		}
		if r.tmpl == "" {
			return memRow{err: pgx.ErrNoRows}
		}
		return memRow{vals: []any{r.tmpl}}
	}
	return memRow{vals: r.event}
}

func TestRenderDeliveryUsesCatalogTemplate(t *testing.T) {
	id := "e1"
	db := &renderDB{
		event: []any{"Download low", "Fiber", time.Date(2026, 11, 8, 1, 0, 0, 0, time.UTC), "quality.download_low",
			[]byte(`{"node":"Fiber","event":"Download below limit","severity":"warn","severity_icon":"⚠️","download":"180 Mbps","ping":"12","jitter":"3","loss":"0.1","download_limit":"65% of expected","ping_limit":"30","jitter_limit":"10","loss_limit":"2","download_ok":"no","upload_ok":"yes","ping_ok":"yes","jitter_ok":"yes","loss_ok":"yes","since":"2026-11-08T01:00:00Z","link":"https://c.example/#/alerts"}`)},
	}
	store := &Store{}
	text, err := store.renderDelivery(context.Background(), db, Delivery{EventID: &id, Channel: "telegram"}, "https://c.example/")
	if err != nil || text == "" || !strings.Contains(text, "Fiber") || !strings.Contains(text, "12") {
		t.Fatalf("render = %q %v", text, err)
	}
	db.tmpl = "Custom {node} ping {ping}"
	text, err = store.renderDelivery(context.Background(), db, Delivery{EventID: &id, Channel: "email"}, "https://c.example/")
	if err != nil || text != "Custom Fiber ping 12" {
		t.Fatalf("custom = %q %v", text, err)
	}
	plain, err := store.renderDelivery(context.Background(), db, Delivery{Channel: "email", CreatedAt: time.Unix(0, 0).UTC()}, "https://c.example/")
	if err != nil || plain == "" {
		t.Fatalf("test msg %q %v", plain, err)
	}
	if trimRightSlash("https://x/") != "https://x" {
		t.Fatal("trim")
	}
	empty := &renderDB{}
	body, err := store.templateBody(context.Background(), empty, "not.an.event", "email")
	if err != nil || body != "" {
		t.Fatalf("missing body %q %v", body, err)
	}
	legacy := &renderDB{event: []any{"Title", "Node", time.Unix(1, 0).UTC(), "", []byte(`{}`)}}
	text, err = store.renderDelivery(context.Background(), legacy, Delivery{EventID: &id, Channel: "email"}, "https://c.example/")
	if err != nil || !strings.Contains(text, "Title") {
		t.Fatalf("legacy %q %v", text, err)
	}
	sparse := &renderDB{event: []any{"Title", "Node", time.Unix(1, 0).UTC(), "test.success", []byte(`{}`)}}
	text, err = store.renderDelivery(context.Background(), sparse, Delivery{EventID: &id, Channel: "telegram"}, "https://c.example/")
	if err != nil || !strings.Contains(text, "Node") {
		t.Fatalf("sparse %q %v", text, err)
	}
	def, err := store.templateBody(context.Background(), empty, "test.success", "telegram")
	if err != nil || def == "" {
		t.Fatalf("default body %q %v", def, err)
	}
	fallback, err := store.templateBody(context.Background(), empty, "test.success", "nope")
	if err != nil || fallback == "" {
		t.Fatalf("email fallback %q %v", fallback, err)
	}
	failing := &renderDB{event: db.event, tmplErr: errors.New("temporary database outage")}
	if _, err := store.renderDelivery(context.Background(), failing, Delivery{EventID: &id, Channel: "telegram"}, "https://c.example/"); err == nil || !strings.Contains(err.Error(), "temporary database outage") {
		t.Fatalf("database failure must be retried instead of sending a default: %v", err)
	}
}
