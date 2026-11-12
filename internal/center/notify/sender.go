// Package notify delivers durable alert notifications without exposing credentials.
package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"strings"
	"time"
)

// ErrSend is intentionally independent of transport errors, URLs and server bodies.
var ErrSend = errors.New("notification transport failed")

// Sender sends one plain-text message; credentials must never be logged.
type Sender interface {
	Send(context.Context, string, string, string, string) error
}

// Transport uses SMTP and bounded HTTP requests. No redirects are followed.
type Transport struct {
	SMTPAddr, SMTPFrom, SMTPUser string
	TelegramURL                  string
	Client                       *http.Client
}

func (s *Transport) Send(ctx context.Context, channel, to, secret, message string) error {
	if channel == "email" {
		return s.email(ctx, to, secret, message)
	}
	var endpoint string
	var payload any
	switch channel {
	case "telegram":
		base := strings.TrimRight(s.TelegramURL, "/")
		if base == "" {
			return ErrSend
		}
		endpoint = base + "/bot" + url.PathEscape(secret) + "/sendMessage"
		payload = map[string]string{"chat_id": to, "text": message, "parse_mode": "HTML"}
	case "slack":
		endpoint = secret
		payload = map[string]any{"blocks": []any{map[string]any{"type": "section", "text": map[string]string{"type": "mrkdwn", "text": message}}}}
	case "webhook":
		endpoint = secret
		if json.Valid([]byte(message)) {
			payload = json.RawMessage(message)
		} else {
			payload = map[string]string{"text": message}
		}
	default:
		return ErrSend
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return ErrSend
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return ErrSend
	}
	req.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: 10 * time.Second}
	if s.Client != nil {
		client = *s.Client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return ErrSend
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ErrSend
	}
	if channel == "telegram" {
		var result struct {
			OK bool `json:"ok"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&result) != nil || !result.OK {
			return ErrSend
		}
	}
	return nil
}

func (s *Transport) email(ctx context.Context, to, password, message string) error {
	from, err := mail.ParseAddress(s.SMTPFrom)
	if err != nil || from.Address != s.SMTPFrom {
		return ErrSend
	}
	dest, err := mail.ParseAddress(to)
	if err != nil || dest.Address != to || strings.ContainsAny(to+s.SMTPFrom, "\r\n") {
		return ErrSend
	}
	host, _, err := net.SplitHostPort(s.SMTPAddr)
	if err != nil {
		return ErrSend
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", s.SMTPAddr)
	if err != nil {
		return ErrSend
	}
	defer func() { _ = conn.Close() }()
	deadline := time.Now().Add(10 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if conn.SetDeadline(deadline) != nil {
		return ErrSend
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return ErrSend
	}
	defer func() { _ = client.Close() }()
	if ok, _ := client.Extension("STARTTLS"); ok {
		if client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}) != nil {
			return ErrSend
		}
	}
	if password != "" {
		if client.Auth(smtp.PlainAuth("", s.SMTPUser, password, host)) != nil {
			return ErrSend
		}
	}
	if client.Mail(from.Address) != nil || client.Rcpt(dest.Address) != nil {
		return ErrSend
	}
	writer, err := client.Data()
	if err != nil {
		return ErrSend
	}
	_, err = fmt.Fprintf(writer, "From: %s\r\nTo: %s\r\nSubject: Speedtest notification\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s\r\n", from.Address, dest.Address, strings.ReplaceAll(strings.ReplaceAll(message, "\r\n", "\n"), "\n", "\r\n"))
	if err != nil {
		_ = writer.Close()
		return ErrSend
	}
	if writer.Close() != nil || client.Quit() != nil {
		return ErrSend
	}
	return nil
}

// Message keeps the event, node, start time and dashboard together in plain text. An optional detail (for example the old and the
// new address of an IP change) goes on its own line under the title.
func Message(what, node string, since time.Time, centerURL string, detail ...string) string {
	clean := func(v string) string { return strings.Join(strings.Fields(v), " ") }
	extra := ""
	if len(detail) > 0 && clean(detail[0]) != "" {
		extra = clean(detail[0]) + "\n"
	}
	return fmt.Sprintf("%s — %s\n%sSince %s\n%s", clean(what), clean(node), extra, since.UTC().Format(time.RFC3339), strings.TrimRight(centerURL, "/")+"/#/alerts")
}

// Backoff doubles from one minute and caps at one hour.
func Backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 7 {
		return time.Hour
	}
	d := time.Minute * time.Duration(1<<uint(attempt-1))
	if d > time.Hour {
		return time.Hour
	}
	return d
}
