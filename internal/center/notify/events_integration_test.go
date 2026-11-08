//go:build integration

package notify

import (
	"errors"
	"testing"

	"github.com/jooservices/speedtest/internal/center/notify/template"
)

func TestEventStoreCatalogOverridesAndTemplates(t *testing.T) {
	p, ctx := database(t)
	store := &EventStore{DB: p}

	list, err := store.ListEvents(ctx)
	if err != nil || len(list) < 30 {
		t.Fatalf("list = %d %v", len(list), err)
	}
	view, err := store.GetEvent(ctx, "test.success")
	if err != nil || !view.Enabled || view.Customized {
		t.Fatalf("default view = %+v %v", view, err)
	}
	off := false
	warn := "warn"
	updated, err := store.UpdateEvent(ctx, "test.success", EventPatch{Enabled: &off, Severity: &warn, Channels: []string{"email", "telegram"}})
	if err != nil || updated.Enabled || updated.Severity != "warn" || !updated.Customized || len(updated.Channels) != 2 {
		t.Fatalf("update = %+v %v", updated, err)
	}
	if _, err := store.UpdateEvent(ctx, "missing", EventPatch{}); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("missing update: %v", err)
	}
	if _, err := store.UpdateEvent(ctx, "test.success", EventPatch{Channels: []string{"nope"}}); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("bad channels: %v", err)
	}

	tpl, err := store.GetTemplate(ctx, "test.success", "telegram")
	if err != nil || !tpl.IsDefault || tpl.Body == "" {
		t.Fatalf("default template = %+v %v", tpl, err)
	}
	custom := "Hello {node} — download {download} ping {ping}"
	saved, err := store.PutTemplate(ctx, "test.success", "telegram", custom)
	if err != nil || saved.IsDefault || saved.Body != custom {
		t.Fatalf("put = %+v %v", saved, err)
	}
	if _, err := store.PutTemplate(ctx, "test.success", "telegram", "bad {secret}"); !errors.Is(err, template.ErrUnknownPlaceholder) {
		t.Fatalf("unknown placeholder: %v", err)
	}
	preview, err := store.PreviewTemplate(ctx, "test.success", "telegram", "")
	if err != nil || preview.Body == "" || preview.Sample["node"] == "" {
		t.Fatalf("preview = %+v %v", preview, err)
	}
	reset, err := store.ResetTemplate(ctx, "test.success", "telegram")
	if err != nil || !reset.IsDefault {
		t.Fatalf("reset = %+v %v", reset, err)
	}

	execute(t, p, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES('10000000-0000-4000-8000-000000000001','Fiber',100,50)`)
	on, err := store.NodeEnabled(ctx, "test.success", "10000000-0000-4000-8000-000000000001")
	if err != nil || on {
		// globally disabled above
		if err != nil {
			t.Fatal(err)
		}
		if on {
			t.Fatal("expected global off")
		}
	}
	en := true
	if _, err := store.UpdateEvent(ctx, "test.success", EventPatch{Enabled: &en}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetNodeEnabled(ctx, "test.success", "10000000-0000-4000-8000-000000000001", false); err != nil {
		t.Fatal(err)
	}
	on, err = store.NodeEnabled(ctx, "test.success", "10000000-0000-4000-8000-000000000001")
	if err != nil || on {
		t.Fatalf("node override = %v %v", on, err)
	}
	if err := store.SetNodeEnabled(ctx, "node.offline", "10000000-0000-4000-8000-000000000001", false); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("non-scoped node setting: %v", err)
	}
	body, sev, channels, err := store.ResolveBody(ctx, "quality.download_low", "email")
	if err != nil || body == "" || sev == "" || len(channels) == 0 {
		t.Fatalf("resolve = %q %q %v %v", body, sev, channels, err)
	}
}
