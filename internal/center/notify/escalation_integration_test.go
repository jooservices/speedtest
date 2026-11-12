//go:build integration

package notify

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jooservices/speedtest/internal/center/settings"
)

type escalationSettings struct{}

func (escalationSettings) NotificationConfig(_ context.Context, _ pgx.Tx, channel string) (settings.Settings, string, error) {
	view := settings.Settings{Notify: settings.NotificationSettings{
		Telegram: settings.ChannelConfig{On: true, To: "chat"},
		Email:    settings.ChannelConfig{On: true, To: "ops@example.test"},
	}}
	secret := ""
	if channel == "telegram" {
		secret = "telegram-token"
	}
	return view, secret, nil
}

type failedTelegramSender struct{ calls int }

func (s *failedTelegramSender) Send(_ context.Context, channel, _, _, _ string) error {
	if channel == "telegram" {
		s.calls++
		return errors.New("provider unavailable")
	}
	return nil
}

func TestRepeatedDeliveryFailureEscalatesThroughAnotherChannel(t *testing.T) {
	p, ctx := database(t)
	const nodeID = "10000000-0000-4000-8000-000000000081"
	const eventID = "10000000-0000-4000-8000-000000000082"
	now := time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC)
	if _, err := p.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES($1,'Fiber',100,50)`, nodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO alert_events(id,node_id,severity,ts,title,detail,acked,event_key,payload)
		VALUES($1,$2,'info',$3,'Test success','Sample',true,'test.success','{"node":"Fiber"}')`, eventID, nodeID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO notification_deliveries(id,event_id,channel,created_at,next_attempt_at) VALUES($1,$2,'telegram',$3,$3)`, "10000000-0000-4000-8000-000000000083", eventID, now); err != nil {
		t.Fatal(err)
	}
	sender := &failedTelegramSender{}
	store := &Store{DB: p, Settings: escalationSettings{}, Sender: sender, Now: func() time.Time { return now }}
	for range 3 {
		if did, err := store.Dispatch(ctx); err != nil || !did {
			t.Fatalf("dispatch = %v %v", did, err)
		}
		if _, err := p.Exec(ctx, `UPDATE notification_deliveries SET next_attempt_at=$2 WHERE id=$1`, "10000000-0000-4000-8000-000000000083", now); err != nil {
			t.Fatal(err)
		}
	}
	var channel string
	if err := p.QueryRow(ctx, `SELECT o.channels[1] FROM alert_notification_outbox o JOIN alert_events e ON e.id=o.event_id WHERE e.event_key='platform.delivery_failing'`).Scan(&channel); err != nil || channel != "email" {
		t.Fatalf("escalation route = %q %v", channel, err)
	}
	var attempts int
	if err := p.QueryRow(ctx, `SELECT attempts FROM notification_deliveries WHERE id=$1`, "10000000-0000-4000-8000-000000000083").Scan(&attempts); err != nil || attempts != 3 || sender.calls != 3 {
		t.Fatalf("failed attempts = %d, provider calls = %d, error = %v", attempts, sender.calls, err)
	}
}
