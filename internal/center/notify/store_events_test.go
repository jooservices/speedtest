package notify

import (
	"context"
	"errors"
	"testing"
)

func TestStoreNotificationCatalogAdapters(t *testing.T) {
	ctx := context.Background()
	db := newMemEventDB()
	store := &Store{EventsDB: db}

	list, err := store.ListNotificationEvents(ctx)
	if err != nil || len(list) == 0 {
		t.Fatalf("ListNotificationEvents: %v len=%d", err, len(list))
	}
	got, err := store.GetNotificationEvent(ctx, "test.success")
	if err != nil || got.Key != "test.success" {
		t.Fatalf("GetNotificationEvent: %+v %v", got, err)
	}
	off := false
	updated, err := store.UpdateNotificationEvent(ctx, "test.success", EventPatch{Enabled: &off})
	if err != nil || updated.Enabled {
		t.Fatalf("UpdateNotificationEvent: %+v %v", updated, err)
	}
	tpl, err := store.GetNotificationTemplate(ctx, "test.success", "telegram")
	if err != nil || tpl.Body == "" {
		t.Fatalf("GetNotificationTemplate: %+v %v", tpl, err)
	}
	saved, err := store.PutNotificationTemplate(ctx, "test.success", "telegram", "Hi {node}")
	if err != nil || saved.Body != "Hi {node}" || saved.IsDefault {
		t.Fatalf("PutNotificationTemplate: %+v %v", saved, err)
	}
	preview, err := store.PreviewNotificationTemplate(ctx, "test.success", "telegram", "Hi {node}")
	if err != nil || preview.Body == "" {
		t.Fatalf("PreviewNotificationTemplate: %+v %v", preview, err)
	}
	reset, err := store.ResetNotificationTemplate(ctx, "test.success", "telegram")
	if err != nil || !reset.IsDefault {
		t.Fatalf("ResetNotificationTemplate: %+v %v", reset, err)
	}
	if err := store.SetNotificationNodeEnabled(ctx, "test.success", "n1", false); err != nil {
		t.Fatalf("SetNotificationNodeEnabled: %v", err)
	}

	db.fail = errors.New("boom")
	if _, err := store.ListNotificationEvents(ctx); err == nil {
		t.Fatal("expected ListNotificationEvents failure")
	}
	if store.eventAPI().DB != db {
		t.Fatal("EventsDB should win over pool DB")
	}
	if (&Store{}).eventAPI() == nil {
		t.Fatal("eventAPI must return an EventStore when EventsDB is unset")
	}
}
