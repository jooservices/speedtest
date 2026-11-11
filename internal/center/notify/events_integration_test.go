//go:build integration

package notify

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/audit"
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

func TestEventStoreConcurrentFieldPatchesPreserveBothChanges(t *testing.T) {
	p, ctx := database(t)
	store := &EventStore{DB: p}
	warn := "warn"
	channels := []string{"email", "telegram"}
	var wait sync.WaitGroup
	errs := make(chan error, 2)
	wait.Add(2)
	go func() {
		defer wait.Done()
		_, err := store.UpdateEvent(ctx, "test.success", EventPatch{Severity: &warn})
		errs <- err
	}()
	go func() {
		defer wait.Done()
		_, err := store.UpdateEvent(ctx, "test.success", EventPatch{Channels: channels})
		errs <- err
	}()
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	view, err := store.GetEvent(ctx, "test.success")
	if err != nil || view.Severity != warn || len(view.Channels) != 2 || view.Channels[0] != "email" || view.Channels[1] != "telegram" {
		t.Fatalf("concurrent patches lost a field: %+v %v", view, err)
	}
}

func TestEventAndAuditCommitTogether(t *testing.T) {
	p, ctx := database(t)
	store := &Store{DB: p}
	off := false
	if _, err := store.UpdateNotificationEventAudited(ctx, "test.success", EventPatch{Enabled: &off}, audit.Entry{Actor: "owner@example.com"}); err == nil {
		t.Fatal("event setting committed without a durable audit entry")
	}
	event, err := store.GetNotificationEvent(ctx, "test.success")
	if err != nil || !event.Enabled || event.Customized {
		t.Fatalf("failed audit left an event change behind: %+v %v", event, err)
	}
	entry := audit.Entry{Actor: "owner@example.com", Action: "notification.event.update", TargetType: "notification_event", TargetID: "test.success"}
	if _, err := store.UpdateNotificationEventAudited(ctx, "test.success", EventPatch{Enabled: &off}, entry); err != nil {
		t.Fatal("audited update failed:", err)
	}
	var count int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action='notification.event.update' AND target_id='test.success'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("audit row = %d %v", count, err)
	}
}

func TestCatalogPublisherSupportsSystemAndNodeEvents(t *testing.T) {
	p, ctx := database(t)
	store := &Store{DB: p}
	if _, err := p.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES('10000000-0000-4000-8000-000000000071','Fiber',100,50)`); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishEvent(ctx, "platform.export_ready", "", map[string]string{"link": "https://center/export"}); err != nil {
		t.Fatal("publish system event:", err)
	}
	if err := store.PublishEvent(ctx, "network.isp_changed", "10000000-0000-4000-8000-000000000071", map[string]string{"isp": "Viettel"}); err != nil {
		t.Fatal("publish node event:", err)
	}
	var systemNode *string
	if err := p.QueryRow(ctx, `SELECT node_id::text FROM alert_events WHERE event_key='platform.export_ready'`).Scan(&systemNode); err != nil || systemNode != nil {
		t.Fatalf("system event node = %v %v", systemNode, err)
	}
	var deliveries int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM alert_notification_outbox o JOIN alert_events e ON e.id=o.event_id WHERE e.event_key IN ('platform.export_ready','network.isp_changed')`).Scan(&deliveries); err != nil || deliveries != 2 {
		t.Fatalf("outbox handoff count = %d %v", deliveries, err)
	}
	if err := store.PublishEvent(ctx, "network.server_differs", "10000000-0000-4000-8000-000000000071", map[string]string{"server": "HCMC-1"}); err != nil {
		t.Fatal("publish disabled catalog event:", err)
	}
	if err := p.QueryRow(ctx, `SELECT count(*) FROM alert_events e LEFT JOIN alert_notification_outbox o ON o.event_id=e.id WHERE e.event_key='network.server_differs' AND o.event_id IS NULL`).Scan(&deliveries); err != nil || deliveries != 1 {
		t.Fatalf("disabled catalog event was not retained without dispatch: %d %v", deliveries, err)
	}
}

func TestCatalogPublisherCooldownIsPerEventAndNode(t *testing.T) {
	p, ctx := database(t)
	for index, id := range []string{"10000000-0000-4000-8000-000000000081", "10000000-0000-4000-8000-000000000082"} {
		if _, err := p.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES($1,$2,100,50)`, id, fmt.Sprintf("Fiber %d", index+1)); err != nil {
			t.Fatal(err)
		}
	}
	cooldown, repeat := 300, 60
	if _, err := (&EventStore{DB: p}).UpdateEvent(ctx, "network.isp_changed", EventPatch{CooldownSeconds: &cooldown, RepeatSeconds: &repeat}); err != nil {
		t.Fatal("save event delivery policy:", err)
	}
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	store := &Store{DB: p, Now: func() time.Time { return now }}
	if err := store.PublishEvent(ctx, "network.isp_changed", "10000000-0000-4000-8000-000000000081", map[string]string{"isp": "Alpha"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err := store.PublishEvent(ctx, "network.isp_changed", "10000000-0000-4000-8000-000000000081", map[string]string{"isp": "Beta"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishEvent(ctx, "network.isp_changed", "10000000-0000-4000-8000-000000000082", map[string]string{"isp": "Alpha"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(4 * time.Minute)
	if err := store.PublishEvent(ctx, "network.isp_changed", "10000000-0000-4000-8000-000000000081", map[string]string{"isp": "Gamma"}); err != nil {
		t.Fatal(err)
	}
	var events, queued int
	if err := p.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE o.event_id IS NOT NULL)
		FROM alert_events e LEFT JOIN alert_notification_outbox o ON o.event_id=e.id WHERE e.event_key='network.isp_changed'`).Scan(&events, &queued); err != nil {
		t.Fatal(err)
	}
	if events != 4 || queued != 3 {
		t.Fatalf("cooldown events/queued = %d/%d, want 4/3", events, queued)
	}
}

func TestAuthenticationEventProducersUseScopedThresholds(t *testing.T) {
	p, ctx := database(t)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	store := &Store{DB: p, Now: func() time.Time { return now }}
	threshold := 3.0
	if _, err := (&EventStore{DB: p}).UpdateEvent(ctx, "security.login_burst", EventPatch{Thresholds: map[string]float64{"attempts": threshold}}); err != nil {
		t.Fatal("set failed-login threshold:", err)
	}
	userKey := "10000000-0000-4000-8000-000000000083"
	if err := store.RecordSignIn(ctx, userKey, "203.0.113.81"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSignIn(ctx, userKey, "203.0.113.81"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSignIn(ctx, userKey, "198.51.100.81"); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := store.RecordLoginFailure(ctx, "192.0.2.81"); err != nil {
			t.Fatal(err)
		}
	}
	var newSignIns, bursts, failures, locations int
	if err := p.QueryRow(ctx, `SELECT count(*) FILTER (WHERE event_key='security.signin_new'),
		count(*) FILTER (WHERE event_key='security.login_burst') FROM alert_events`).Scan(&newSignIns, &bursts); err != nil {
		t.Fatal(err)
	}
	if err := p.QueryRow(ctx, `SELECT count(*) FROM notification_login_failures`).Scan(&failures); err != nil {
		t.Fatal(err)
	}
	if err := p.QueryRow(ctx, `SELECT count(*) FROM notification_signin_locations`).Scan(&locations); err != nil {
		t.Fatal(err)
	}
	if newSignIns != 2 || bursts != 1 || failures != 3 || locations != 2 {
		t.Fatalf("signin/burst/failure/location rows = %d/%d/%d/%d", newSignIns, bursts, failures, locations)
	}
}
