package notify

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jooservices/speedtest/internal/center/notify/template"
	"github.com/jooservices/speedtest/internal/center/report"
)

type renderDB struct {
	event    []any
	eventErr error
	tmpl     string
	tmplErr  error
}

func assertMessageContains(t *testing.T, message string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(message, want) {
			t.Errorf("message does not contain %q: %q", want, message)
		}
	}
}

func TestAC1DigestDispatchIncludesPerNodeValuesAndUsesCurrentSafeURL(t *testing.T) {
	id := "digest-short"
	summary := "Router <long-name> — 3 tests: 2 successful, 1 failed; avg down 125.25 Mbps, avg up 22.50 Mbps"
	payload, err := json.Marshal(map[string]string{
		"period": "Daily period: 2026-11-17 (UTC)", "summary": summary,
		"telegram_summary": summary, "telegram_part": "1", "telegram_parts": "1",
		"link": "https://old-center.example/#/",
	})
	if err != nil {
		t.Fatal(err)
	}
	db := &renderDB{event: []any{"Daily summary", "Command center", time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC), "digest.daily", payload}}
	store := &Store{}
	telegram, err := store.renderDelivery(context.Background(), db, Delivery{EventID: &id, Channel: "telegram"}, "https://new-center.example")
	if err != nil {
		t.Fatal("render per-node Telegram digest:", err)
	}
	if got := telegramTextUnits(telegram); got > maxTelegramMessageUnits {
		t.Fatalf("Telegram message has %d UTF-16 units, want at most %d", got, maxTelegramMessageUnits)
	}
	assertMessageContains(t, telegram, "Part 1 of 1", "Router &lt;long-name&gt;", "2 successful, 1 failed",
		"avg down 125.25 Mbps", "avg up 22.50 Mbps", "https://new-center.example/#/")
	if strings.Contains(telegram, "old-center.example") || strings.Contains(telegram, "continue in the dashboard") {
		t.Fatalf("Telegram message contains a stale link or false continuation claim: %q", telegram)
	}
	mail, err := store.renderDelivery(context.Background(), db, Delivery{EventID: &id, Channel: "email"}, "https://new-center.example")
	if err != nil {
		t.Fatal("render e-mail digest:", err)
	}
	if !strings.Contains(mail, summary) || !strings.Contains(mail, "https://new-center.example/#/") || strings.Contains(mail, "old-center.example") {
		t.Fatal("e-mail digest did not retain details or use the current safe link")
	}
}

func TestAC1DigestTelegramCustomTemplatesKeepDetailsAndPartNumberBounded(t *testing.T) {
	id := "digest-custom"
	summary := "Node 😀 & router — 2 tests: 1 successful, 1 failed; avg down 12.25 Mbps, avg up 8.50 Mbps"
	payload, err := json.Marshal(map[string]string{
		"period": "Daily period: 2026-11-17 (UTC)", "summary": summary,
		"telegram_summary": summary, "telegram_part": "3", "telegram_parts": "9",
		"link": "https://speedtest.example.test/#/",
	})
	if err != nil {
		t.Fatal(err)
	}
	db := &renderDB{event: []any{"Daily summary", "Command center", time.Unix(1, 0).UTC(), "digest.daily", payload}}
	store := &Store{}
	for name, body := range map[string]string{
		"template without summary":              "Digest ready.",
		"template expands summary past limit":   strings.Repeat("{summary}", 60),
		"supplementary characters exceed limit": strings.Repeat("😀", 2500),
	} {
		t.Run(name, func(t *testing.T) {
			db.tmpl = body
			message, err := store.renderDelivery(context.Background(), db, Delivery{EventID: &id, Channel: "telegram"}, "https://speedtest.example.test")
			if err != nil {
				t.Fatal("render custom Telegram template:", err)
			}
			if units := telegramTextUnits(message); units > maxTelegramMessageUnits {
				t.Fatalf("Telegram message has %d UTF-16 units", units)
			}
			for _, want := range []string{"Part 3 of 9", "Node 😀 &amp; router", "1 successful, 1 failed", "avg down 12.25 Mbps", "avg up 8.50 Mbps", "https://speedtest.example.test/#/"} {
				if !strings.Contains(message, want) {
					t.Errorf("Telegram message does not contain %q: %q", want, message)
				}
			}
			if strings.Contains(message, "continue in the dashboard") {
				t.Fatalf("Telegram message makes a false continuation claim: %q", message)
			}
		})
	}
}

func TestAC1DigestDispatchRejectsUnsafeCurrentURLForBothChannels(t *testing.T) {
	id := "digest-unsafe-current-url"
	summary := "Router — 1 test: 1 successful, 0 failed; avg down 7.00 Mbps, avg up 3.00 Mbps"
	payload, err := json.Marshal(map[string]string{
		"period": "Daily period: 2026-11-17 (UTC)", "summary": summary,
		"telegram_summary": summary, "telegram_part": "1", "telegram_parts": "1",
		"link": "https://old-center.example/#/",
	})
	if err != nil {
		t.Fatal(err)
	}
	db := &renderDB{event: []any{"Daily summary", "Command center", time.Unix(1, 0).UTC(), "digest.daily", payload}}
	for _, channel := range []string{"telegram", "email"} {
		t.Run(channel, func(t *testing.T) {
			_, err := (&Store{}).renderDelivery(context.Background(), db, Delivery{EventID: &id, Channel: channel}, "https://owner:secret@wrong.example")
			if !errors.Is(err, report.ErrInvalidDashboardURL) {
				t.Fatalf("render digest with unsafe current URL error = %v, want ErrInvalidDashboardURL", err)
			}
		})
	}
}

func TestAC1DigestCustomTemplatesCannotOmitRequiredData(t *testing.T) {
	for _, test := range []struct {
		name    string
		summary string
		link    bool
	}{
		{name: "populated period", summary: "Node — 2 tests: 1 successful, 1 failed; avg down 12 Mbps, avg up 3 Mbps", link: true},
		{name: "empty period", summary: emptyDigestSummary},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, channel := range []string{"telegram", "email"} {
				t.Run(channel, func(t *testing.T) {
					assertCustomDigestTemplateRequiredData(t, channel, test.summary, test.link)
				})
			}
		})
	}
}

func assertCustomDigestTemplateRequiredData(t *testing.T, channel, summary string, link bool) {
	t.Helper()
	id := "digest-required-data"
	payload, err := json.Marshal(map[string]string{
		"period": "Daily period: 2026-11-17 (UTC)", "summary": summary,
		"telegram_summary": summary, "telegram_part": "1", "telegram_parts": "1",
		"link": "https://stale.example/#/",
	})
	if err != nil {
		t.Fatal(err)
	}
	db := &renderDB{event: []any{"Daily summary", "Command center", time.Unix(1, 0).UTC(), "digest.daily", payload}, tmpl: "Custom notification"}
	message, err := (&Store{}).renderDelivery(context.Background(), db, Delivery{EventID: &id, Channel: channel}, "https://current.example")
	if err != nil {
		t.Fatal("render custom digest template:", err)
	}
	if !strings.Contains(message, summary) {
		t.Errorf("custom %s template omitted required summary %q: %q", channel, summary, message)
	}
	if got := strings.Count(message, summary); got != 1 {
		t.Errorf("custom %s template rendered summary %d times, want once", channel, got)
	}
	if link {
		if !strings.Contains(message, "https://current.example/#/") || strings.Contains(message, "stale.example") {
			t.Errorf("custom %s template omitted the current safe link or kept the stale link: %q", channel, message)
		}
	} else if strings.Contains(message, "https://") || strings.Contains(message, "/#/") {
		t.Errorf("empty %s digest unexpectedly contains a dashboard link: %q", channel, message)
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

func TestAC2DigestDispatchRegeneratesDashboardLinkForChangedCenterURL(t *testing.T) {
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
	if strings.Contains(text, "old-center.example") || !strings.Contains(text, "new-center.example/#/") {
		t.Fatalf("digest did not use a safe link from current settings: %q", text)
	}
}

func TestAC1DigestTelegramLimitCountsSupplementaryCharacters(t *testing.T) {
	id := "digest-unicode-template"
	payload, err := json.Marshal(map[string]string{
		"period":           "Daily period: 2026-11-17 (UTC)",
		"summary":          "2 nodes — 2 tests: 2 successful, 0 failed; avg down 10 Mbps, avg up 5 Mbps.",
		"telegram_summary": "2 nodes — 2 tests: 2 successful, 0 failed; avg down 10 Mbps, avg up 5 Mbps.",
		"telegram_part":    "1", "telegram_parts": "1",
		"link": "https://speedtest.example.test/#/",
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
	if telegramTextUnits(message) > maxTelegramMessageUnits || !strings.Contains(message, "avg down 10 Mbps") {
		t.Fatalf("supplementary-character template did not fall back with details: units=%d message=%q", telegramTextUnits(message), message)
	}
}

func TestAC1DigestTelegramUsesFullSummaryWithoutChunkFieldAndBoundsLongPeriod(t *testing.T) {
	id := "digest-legacy-payload"
	summary := "Node 😀 — 1 test: 1 successful, 0 failed; avg down 10 Mbps, avg up 5 Mbps"
	payload, err := json.Marshal(map[string]string{
		"period": strings.Repeat("&", 60), "summary": summary,
		"telegram_part": "2", "telegram_parts": "2",
	})
	if err != nil {
		t.Fatal(err)
	}
	db := &renderDB{event: []any{"Daily summary", "Command center", time.Unix(1, 0).UTC(), "digest.daily", payload}}
	message, err := (&Store{}).renderDelivery(context.Background(), db, Delivery{EventID: &id, Channel: "telegram"}, "https://speedtest.example.test")
	if err != nil {
		t.Fatal("render digest without a chunk summary:", err)
	}
	for _, want := range []string{"Part 2 of 2", "Digest period", "Node 😀", "1 successful, 0 failed", "avg down 10 Mbps", "avg up 5 Mbps"} {
		if !strings.Contains(message, want) {
			t.Errorf("Telegram message does not contain %q: %q", want, message)
		}
	}
}

func TestAC1DigestBoundedFallbackKeepsMaximumAcceptedAmpersandLink(t *testing.T) {
	const linkPrefix = "https://speedtest.example.test/"
	const linkSuffix = "/#/"
	ampersands := maxDigestDashboardLinkRunes - len([]rune(linkPrefix)) - len([]rune(linkSuffix))
	link, err := report.DashboardLink(linkPrefix + strings.Repeat("&", ampersands))
	if err != nil {
		t.Fatal("create maximum-length accepted dashboard link:", err)
	}
	if got := len([]rune(link)); got != maxDigestDashboardLinkRunes {
		t.Fatalf("dashboard link has %d runes, want %d", got, maxDigestDashboardLinkRunes)
	}
	data := map[string]string{
		"period":  "Daily period: 2026-11-17 (UTC)",
		"summary": "Node 😀 — 3 tests: 2 successful, 1 failed; avg down 12 Mbps, avg up 4 Mbps",
		"link":    link, "digest_link_safe": "true", "telegram_part": "1", "telegram_parts": "1",
	}
	baseline, err := renderBoundedTelegramDigest(data)
	if err != nil {
		t.Fatal("render bounded digest baseline:", err)
	}
	data["summary"] += strings.Repeat("x", maxTelegramMessageUnits-telegramTextUnits(baseline))
	message, err := renderDeliveryTemplate("telegram", "digest.daily", "Full details continue in the dashboard: {summary}{summary}", data)
	if err != nil {
		t.Fatal("render oversized custom template with bounded fallback:", err)
	}
	if got := telegramTextUnits(message); got != maxTelegramMessageUnits {
		t.Fatalf("bounded message has %d UTF-16 units, want exactly %d", got, maxTelegramMessageUnits)
	}
	if !strings.Contains(message, "Node 😀 — 3 tests: 2 successful, 1 failed; avg down 12 Mbps, avg up 4 Mbps") {
		t.Fatal("bounded fallback omitted per-node detail")
	}
	if strings.Count(message, strings.Repeat("&amp;", ampersands)) != 1 || !strings.Contains(message, "Dashboard: ") {
		t.Fatal("bounded fallback dropped or duplicated the accepted dashboard link")
	}
	if strings.Contains(message, "continue in the dashboard") {
		t.Fatalf("bounded fallback retained a false continuation claim: %q", message)
	}
	assertBoundedDigestRejectsUnsafeAndOversized(t, data)
}

func assertBoundedDigestRejectsUnsafeAndOversized(t *testing.T, data map[string]string) {
	t.Helper()
	data["digest_link_safe"] = "false"
	if _, err := renderBoundedTelegramDigest(data); !errors.Is(err, report.ErrInvalidDashboardURL) {
		t.Fatalf("unsafe bounded link error = %v, want ErrInvalidDashboardURL", err)
	}
	data["summary"] = strings.Repeat("x", maxTelegramMessageUnits+1)
	data["digest_link_safe"] = "true"
	if _, err := renderBoundedTelegramDigest(data); !errors.Is(err, template.ErrInvalidTemplate) {
		t.Fatalf("oversized digest part error = %v, want ErrInvalidTemplate", err)
	}
}

func TestAC1DigestPartHeaderRejectsInvalidNumbering(t *testing.T) {
	for _, data := range []map[string]string{
		{}, {"telegram_part": "x", "telegram_parts": "3"},
		{"telegram_part": "4", "telegram_parts": "3"},
	} {
		if got := digestPartHeader(data); got != "" {
			t.Errorf("invalid part header = %q, want empty", got)
		}
	}
	if got := digestPartHeader(map[string]string{"telegram_part": "2", "telegram_parts": "3"}); got != "Part 2 of 3" {
		t.Errorf("part header = %q, want Part 2 of 3", got)
	}
}

func TestAC1DigestInvalidTemplateAndEventReadFailuresAreReturned(t *testing.T) {
	id := "digest-read-error"
	payload, err := json.Marshal(map[string]string{
		"period": "Daily period", "summary": "Node — 1 test: 1 successful, 0 failed; avg down 1 Mbps, avg up 1 Mbps",
		"telegram_summary": "Node — 1 test: 1 successful, 0 failed; avg down 1 Mbps, avg up 1 Mbps",
		"telegram_part":    "1", "telegram_parts": "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	db := &renderDB{event: []any{"Daily summary", "Command center", time.Unix(1, 0).UTC(), "digest.daily", payload}, tmpl: "invalid {"}
	if _, err := (&Store{}).renderDelivery(context.Background(), db, Delivery{EventID: &id, Channel: "telegram"}, "https://speedtest.example.test"); !errors.Is(err, template.ErrInvalidTemplate) {
		t.Fatalf("invalid digest template error = %v, want ErrInvalidTemplate", err)
	}
	if _, err := renderBoundedBody("invalid {", map[string]string{}); !errors.Is(err, template.ErrInvalidTemplate) {
		t.Fatalf("invalid bounded template error = %v, want ErrInvalidTemplate", err)
	}
	db.tmpl = " "
	if _, err := (&Store{}).renderDelivery(context.Background(), db, Delivery{EventID: &id, Channel: "telegram"}, "https://speedtest.example.test"); !errors.Is(err, template.ErrInvalidTemplate) {
		t.Fatalf("empty saved digest template error = %v, want ErrInvalidTemplate", err)
	}
	db.eventErr = errors.New("event store unavailable")
	if _, err := (&Store{}).renderDelivery(context.Background(), db, Delivery{EventID: &id, Channel: "telegram"}, ""); err == nil || !strings.Contains(err.Error(), "event store unavailable") {
		t.Fatalf("event read error = %v, want propagated storage error", err)
	}
}

func TestAC1DigestRendererUsesDefaultBodyWhenTemplateIsAbsent(t *testing.T) {
	data := map[string]string{
		"period":  "Daily period: 2026-11-17 (UTC)",
		"summary": "Node — 1 test: 1 successful, 0 failed; avg down 1 Mbps, avg up 1 Mbps",
		"link":    "https://speedtest.example.test/#/", "digest_link_safe": "true",
		"telegram_part": "1", "telegram_parts": "1",
	}
	message, err := renderDeliveryTemplate("telegram", "digest.daily", "", data)
	if err != nil {
		t.Fatal("render digest with missing template body:", err)
	}
	if !strings.Contains(message, "Part 1 of 1") || !strings.Contains(message, data["summary"]) {
		t.Fatalf("default digest body omitted part numbering or summary: %q", message)
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
	if r.eventErr != nil {
		return memRow{err: r.eventErr}
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
