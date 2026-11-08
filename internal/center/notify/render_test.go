package notify

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type renderDB struct {
	event []any
	tmpl  string
}

func (r *renderDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	if strings.Contains(sql, "notification_templates") {
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
	body, ok := store.templateBody(context.Background(), empty, "not.an.event", "email")
	if ok || body != "" {
		t.Fatalf("missing body %q %v", body, ok)
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
	def, ok := store.templateBody(context.Background(), empty, "test.success", "telegram")
	if !ok || def == "" {
		t.Fatalf("default body %q %v", def, ok)
	}
	fallback, ok := store.templateBody(context.Background(), empty, "test.success", "nope")
	if !ok || fallback == "" {
		t.Fatalf("email fallback %q %v", fallback, ok)
	}
}
