package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jooservices/speedtest/internal/center/settings"
)

func TestHTTPSenders(t *testing.T) {
	for _, channel := range []string{"telegram", "slack", "webhook"} {
		t.Run(channel, func(t *testing.T) {
			for _, result := range []struct {
				status int
				body   string
				ok     bool
			}{{200, `{"ok":true}`, true}, {204, ``, channel != "telegram"}, {200, `{"ok":false}`, channel != "telegram"}, {200, `invalid`, channel != "telegram"}, {302, `secret`, false}, {429, `secret`, false}, {500, `secret`, false}} {
				var got map[string]string
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" {
						t.Error("HTTP request contract")
					}
					if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
						t.Error(err)
					}
					if channel == "telegram" && r.URL.Path != "/botprivate-token/sendMessage" {
						t.Error("Telegram route")
					}
					w.Header().Set("Location", "http://should-never-follow.invalid")
					w.WriteHeader(result.status)
					_, _ = io.WriteString(w, result.body)
				}))
				transport := &Transport{TelegramURL: srv.URL, Client: srv.Client()}
				secret := srv.URL
				if channel == "telegram" {
					secret = "private-token"
				}
				err := transport.Send(t.Context(), channel, "123", secret, "test message")
				srv.Close()
				if (err == nil) != result.ok {
					t.Fatalf("status %d body %s: err=%v", result.status, result.body, err)
				}
				if got["text"] != "test message" {
					t.Fatal("message payload")
				}
				if channel == "telegram" && got["chat_id"] != "123" {
					t.Fatal("Telegram chat")
				}
				if err != nil && err.Error() != "notification transport failed" {
					t.Fatal("unsafe transport error")
				}
			}
		})
	}
}
func TestHTTPFailureAndCancellation(t *testing.T) {
	tr := &Transport{}
	for _, channel := range []string{"unknown", "telegram", "webhook"} {
		if !errors.Is(tr.Send(t.Context(), channel, "", "invalid\nsecret", "message"), ErrSend) {
			t.Fatal("invalid transport")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if tr.Send(ctx, "webhook", "", "http://127.0.0.1:1/secret", "message") == nil {
		t.Fatal("cancelled request")
	}
	tr.TelegramURL = "://"
	if tr.Send(t.Context(), "telegram", "", "secret", "message") == nil {
		t.Fatal("malformed URL")
	}
}

func TestMessageAndBackoff(t *testing.T) {
	since := time.Date(2026, 11, 3, 10, 0, 0, 0, time.FixedZone("local", 7*3600))
	got := Message("Speed\nlow", "Node\rOne", since, "https://dashboard.example/")
	if got != "Speed low — Node One\nSince 2026-11-03T03:00:00Z\nhttps://dashboard.example/#/alerts" {
		t.Fatal(got)
	}
	// An optional detail (the old and the new address of an IP change) sits under the title; blank or multi-line details are tidied.
	detailed := Message("Public IP changed", "Home", since, "https://dashboard.example/", "Changed from 203.0.113.1 to 203.0.113.2\n(Hanoi)")
	if detailed != "Public IP changed — Home\nChanged from 203.0.113.1 to 203.0.113.2 (Hanoi)\nSince 2026-11-03T03:00:00Z\nhttps://dashboard.example/#/alerts" {
		t.Fatal(detailed)
	}
	if Message("x", "n", since, "https://d/", "  ") != Message("x", "n", since, "https://d/") {
		t.Fatal("a blank detail must not add a line")
	}
	for n, want := range map[int]time.Duration{-1: time.Minute, 0: time.Minute, 1: time.Minute, 2: 2 * time.Minute, 6: 32 * time.Minute, 7: time.Hour, 8: time.Hour, 100: time.Hour} {
		if Backoff(n) != want {
			t.Errorf("backoff(%d)=%v", n, Backoff(n))
		}
	}
}

func TestSMTPRejectsInvalidAddresses(t *testing.T) {
	for _, tr := range []Transport{{SMTPFrom: "bad"}, {SMTPFrom: "sender@example.com"}, {SMTPFrom: "sender@example.com", SMTPAddr: "bad"}, {SMTPFrom: "sender@example.com", SMTPAddr: "127.0.0.1:1"}} {
		if tr.Send(t.Context(), "email", "receiver@example.com", "", "hello") == nil {
			t.Fatal("invalid SMTP config")
		}
	}
	tr := &Transport{SMTPFrom: "sender@example.com", SMTPAddr: "127.0.0.1:1"}
	if tr.Send(t.Context(), "email", "bad\r\nBcc: victim@example.com", "", "message") == nil {
		t.Fatal("SMTP header injection")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if tr.Send(ctx, "email", "receiver@example.com", "", "hello") == nil {
		t.Fatal("SMTP cancellation")
	}
}

// The SMTP protocol is exercised locally without a real SMTP account. Mailpit
// additionally verifies the shared service in the integration suite.
func TestSMTPProtocol(t *testing.T) {
	for _, fail := range []string{"", "greeting", "mail", "rcpt", "data", "body", "quit", "auth", "tls"} {
		t.Run(fail, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = listener.Close() }()
			done := make(chan string, 1)
			go smtpFixture(listener, fail, done)
			tr := &Transport{SMTPAddr: listener.Addr().String(), SMTPFrom: "sender@example.com"}
			password := ""
			if fail == "auth" {
				password = "private"
			}
			err = tr.Send(t.Context(), "email", "receiver@example.com", password, "hello\nworld")
			if (err == nil) != (fail == "") {
				t.Fatalf("SMTP result=%v", err)
			}
			select {
			case body := <-done:
				if fail == "" && !strings.Contains(body, "hello\r\nworld") {
					t.Fatal("SMTP body")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("SMTP server stalled")
			}
		})
	}
}

type preferenceStub struct {
	view              settings.Settings
	secret            string
	getErr, secretErr error
}

func (p preferenceStub) Get(context.Context) (settings.Settings, string, error) {
	return p.view, "", p.getErr
}
func (p preferenceStub) NotificationSecret(context.Context, string) (string, error) {
	return p.secret, p.secretErr
}
func TestConfiguration(t *testing.T) {
	for _, channel := range []string{"email", "telegram", "slack", "webhook", "invalid"} {
		v := settings.Settings{Notify: settings.NotificationSettings{Email: settings.ChannelConfig{On: true, To: "a@example.com"}, Telegram: settings.ChannelConfig{On: true, To: "123"}, Slack: settings.ChannelConfig{On: true}, Webhook: settings.ChannelConfig{On: true}}}
		for _, p := range []preferenceStub{{view: v, secret: "private"}, {view: v}, {view: v, getErr: errors.New("read")}, {view: v, secretErr: errors.New("decrypt")}, {}} {
			s := &Store{Settings: p}
			_, _, _, err := s.configuration(t.Context(), channel)
			expected := channel != "invalid" && p.getErr == nil && p.secretErr == nil && (p.secret != "" || channel == "email") && p.view.Notify.Email.On
			if (err == nil) != expected {
				t.Fatalf("configuration %s: %v", channel, err)
			}
		}
	}
	now := time.Now()
	s := &Store{Now: func() time.Time { return now }}
	if !s.now().Equal(now) {
		t.Fatal("fake clock")
	}
	if (&Store{}).now().IsZero() {
		t.Fatal("clock")
	}
	if _, err := (&Store{Settings: preferenceStub{}}).Test(t.Context(), "bad"); err == nil {
		t.Fatal("invalid test channel")
	}
}

func (p preferenceStub) NotificationConfig(_ context.Context, _ pgx.Tx, _ string) (settings.Settings, string, error) {
	if p.getErr != nil {
		return p.view, "", p.getErr
	}
	return p.view, p.secret, p.secretErr
}
