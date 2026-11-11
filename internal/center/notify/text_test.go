package notify

import (
	"context"
	"errors"
	"testing"

	"github.com/jooservices/speedtest/internal/center/settings"
)

type recordingSender struct {
	channel, to, secret, message string
	err                          error
}

func (r *recordingSender) Send(_ context.Context, channel, to, secret, message string) error {
	r.channel, r.to, r.secret, r.message = channel, to, secret, message
	return r.err
}

func TestSendText(t *testing.T) {
	view := settings.Settings{CenterURL: "https://speedtest.example", Notify: settings.NotificationSettings{Email: settings.ChannelConfig{On: true, To: "ops@example.com"}}}
	sender := &recordingSender{}
	s := &Store{Settings: preferenceStub{view: view}, Sender: sender}
	if err := s.SendText(t.Context(), "email", func(url string) string { return "report " + url }); err != nil {
		t.Fatal(err)
	}
	if sender.channel != "email" || sender.to != "ops@example.com" || sender.message != "report https://speedtest.example" {
		t.Fatalf("%#v", sender)
	}
	sender.err = errors.New("smtp")
	if err := s.SendText(t.Context(), "email", func(string) string { return "x" }); err == nil {
		t.Fatal("send error swallowed")
	}
	off := &Store{Settings: preferenceStub{}, Sender: sender}
	if err := off.SendText(t.Context(), "email", func(string) string { return "x" }); !errors.Is(err, ErrUnconfigured) {
		t.Fatalf("got %v", err)
	}
}
