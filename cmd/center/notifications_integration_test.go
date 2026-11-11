//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/shared/config"
	"github.com/jooservices/speedtest/internal/shared/testenv"
)

// This cross-component scenario runs the real center with real session middleware,
// encrypted settings, PostgreSQL outbox, background loop and an HTTP destination.
func TestNotificationCenterEndToEndIntegration(t *testing.T) {
	values, err := godotenv.Read("../../.env")
	if err != nil {
		t.Fatal("worktree .env required")
	}
	testenv.Apply(t, values)
	t.Setenv("APP_ENV", "test")
	t.Setenv("SPEEDTEST_AGENT_REQUESTS_PER_MINUTE", "6000")
	t.Setenv("APP_KEY", "base64:"+base64.StdEncoding.EncodeToString([]byte(strings.Repeat("n", 32))))
	t.Setenv("SPEEDTEST_PUBLIC_URL", "")
	t.Setenv("SPEEDTEST_RELEASE_DIR", "")
	t.Setenv("SPEEDTEST_GEO_IPAPI_ENABLED", "false")
	t.Setenv("SPEEDTEST_GEO_IPINFO_ENABLED", "false")
	t.Setenv("SPEEDTEST_GEO_DBIP_AUTO_UPDATE", "false")
	cfg, err := config.LoadCenter()
	if err != nil || db.CheckTestDatabase(cfg.Database.Name) != nil {
		t.Fatal("verified worktree configuration required")
	}
	p, err := db.Open(t.Context(), cfg.Database)
	if err != nil {
		t.Fatal("worktree database")
	}
	t.Cleanup(p.Close)
	dbtest.Reset(t, p)
	t.Cleanup(func() { dbtest.Reset(t, p) })
	dbtest.MigrateUp(t, p)
	if err := runSeedAdmin(t.Context(), nil, io.Discard); err != nil {
		t.Fatal("seed test owner", err)
	}
	var calls atomic.Int32
	messages := make(chan string, 4)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 8192))
		messages <- string(raw)
		calls.Add(1)
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			_, _ = io.WriteString(w, `{"ok":true}`)
		} else {
			w.WriteHeader(204)
		}
	}))
	defer remote.Close()
	t.Setenv("SPEEDTEST_TELEGRAM_URL", remote.URL)
	address := freeTCPAddress(t)
	t.Setenv("SPEEDTEST_HTTP_ADDR", address)
	t.Setenv("SPEEDTEST_UI_DIR", "")
	var logs bytes.Buffer
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runWithDatabase(ctx, nil, slog.New(slog.NewJSONHandler(&logs, nil)), io.Discard) }()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Error("center shutdown", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("center shutdown timeout")
		}
	})
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 3 * time.Second}
	base := "http://" + address
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get(base + "/readyz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("center did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Unauthenticated reads must not expose delivery history.
	unauth, err := client.Get(base + "/api/notifications/deliveries")
	if err != nil {
		t.Fatal(err)
	}
	_ = unauth.Body.Close()
	if unauth.StatusCode != 401 {
		t.Fatal("delivery session bypass")
	}
	csrf := ""
	call := func(method, path, body string, write bool) (int, http.Header, map[string]any) {
		t.Helper()
		req, err := http.NewRequest(method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if write {
			req.Header.Set("Origin", base)
			req.Header.Set("X-CSRF-Token", csrf)
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal("HTTP request failed")
		}
		defer func() { _ = resp.Body.Close() }()
		var envelope map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
			t.Fatal("JSON response")
		}
		raw, _ := json.Marshal(envelope)
		if strings.Contains(string(raw), "private-webhook-token") || strings.Contains(string(raw), "private-telegram-token") || strings.Contains(string(raw), "private-slack-token") {
			t.Fatal("credential leaked in API")
		}
		return resp.StatusCode, resp.Header, envelope
	}
	status, _, body := call("GET", "/api/auth/me", "", false)
	if status != 200 {
		t.Fatal("session bootstrap")
	}
	csrf = body["data"].(map[string]any)["csrfToken"].(string)
	status, _, body = call("POST", "/api/auth/login", `{"email":"jooservices@gmail.com","password":"password"}`, true)
	if status != 200 {
		t.Fatal("owner login", status)
	}
	csrf = body["data"].(map[string]any)["csrfToken"].(string)
	status, _, _ = call("POST", "/api/notifications/test/webhook", "", true)
	if status != 422 {
		t.Fatal("unconfigured channel", status)
	}
	// Save through the public API, including write-only URL encryption.
	status, headers, body := call("GET", "/api/settings", "", false)
	if status != 200 {
		t.Fatal("settings read")
	}
	data := body["data"].(map[string]any)
	for _, k := range []string{"geoKeyConfigured", "enrollKeyConfigured"} {
		delete(data, k)
	}
	channels := data["notify"].(map[string]any)
	for _, v := range channels {
		delete(v.(map[string]any), "configured")
	}
	channels["webhook"] = map[string]any{"on": true, "to": "", "secret": remote.URL + "/private-webhook-token"}
	channels["slack"] = map[string]any{"on": true, "to": "", "secret": remote.URL + "/private-slack-token"}
	channels["telegram"] = map[string]any{"on": true, "to": "123", "secret": "private-telegram-token"}
	channels["email"] = map[string]any{"on": true, "to": "notification-test@example.com"}
	data["centerUrl"] = "https://dashboard.example"
	raw, _ := json.Marshal(data)
	req, _ := http.NewRequest("PUT", base+"/api/settings", bytes.NewReader(raw))
	req.Header.Set("Origin", base)
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", headers.Get("ETag"))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || strings.Contains(string(saved), "private-webhook-token") {
		t.Fatal("save encrypted settings", resp.StatusCode)
	}
	status, _, _ = call("POST", "/api/notifications/test/webhook", "", false)
	if status != 403 {
		t.Fatal("CSRF bypass", status)
	}
	status, headers, body = call("POST", "/api/notifications/test/webhook", "", true)
	if status != 202 || headers.Get("Location") == "" || len(body["meta"].(map[string]any)) != 0 {
		t.Fatal("queued test", status)
	}
	// A valid session and CSRF token still cannot authorize a foreign Origin.
	foreign, _ := http.NewRequest("POST", base+"/api/notifications/test/webhook", nil)
	foreign.Header.Set("Origin", "https://foreign.example")
	foreign.Header.Set("X-CSRF-Token", csrf)
	rejected, err := client.Do(foreign)
	if err != nil {
		t.Fatal(err)
	}
	_ = rejected.Body.Close()
	if rejected.StatusCode != 403 {
		t.Fatal("Origin bypass")
	}
	location := headers.Get("Location")
	deadline = time.Now().Add(5 * time.Second)
	for {
		status, _, body = call("GET", location, "", false)
		if status == 200 && body["data"].(map[string]any)["status"] == "sent" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background test send did not complete")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Fatal("test send count")
	}
	select {
	case message := <-messages:
		if !strings.Contains(message, "Test notification") || !strings.Contains(message, "Since") || !strings.Contains(message, "https://dashboard.example/#/alerts") {
			t.Fatal("test template")
		}
	default:
		t.Fatal("test message not received")
	}
	// All four test-channel flows pass through the real center and retry worker.
	for _, channel := range []string{"telegram", "slack", "email"} {
		status, headers, _ = call("POST", "/api/notifications/test/"+channel, "", true)
		if status != 202 {
			t.Fatal("test channel enqueue", channel, status)
		}
		deadline = time.Now().Add(5 * time.Second)
		for {
			status, _, body = call("GET", headers.Get("Location"), "", false)
			if status == 200 && body["data"].(map[string]any)["status"] == "sent" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("test channel delivery", channel)
			}
			time.Sleep(100 * time.Millisecond)
		}
		if channel != "email" {
			select {
			case message := <-messages:
				if !strings.Contains(message, "Test notification") {
					t.Fatal("channel template", channel)
				}
			default:
				t.Fatal("channel not received", channel)
			}
		}
	}
	// An alert outbox event must reach the same sender automatically.
	_, err = p.Exec(t.Context(), `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES('10000000-0000-4000-8000-000000000002','Fiber fixture',100,50);
 INSERT INTO alert_events(id,node_id,severity,ts,title) VALUES('20000000-0000-4000-8000-000000000002','10000000-0000-4000-8000-000000000002','warn',now(),'Download below threshold');
 INSERT INTO alert_notification_outbox(event_id,channels) VALUES('20000000-0000-4000-8000-000000000002',ARRAY['webhook'])`)
	if err != nil {
		t.Fatal("alert fixture")
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		status, _, body = call("GET", "/api/notifications/deliveries?event=20000000-0000-4000-8000-000000000002", "", false)
		items, _ := body["data"].([]any)
		if status == 200 && len(items) == 1 && items[0].(map[string]any)["status"] == "sent" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("outbox did not deliver")
		}
		time.Sleep(100 * time.Millisecond)
	}
	select {
	case message := <-messages:
		if !strings.Contains(message, "Download below threshold") || !strings.Contains(message, "Fiber fixture") {
			t.Fatal("alert template")
		}
	default:
		t.Fatal("alert not received")
	}
	if calls.Load() != 4 {
		t.Fatal("alert send count")
	}
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown")
	}
	// Cleanup has already observed shutdown; replace channel result for its check.
	done <- nil
	if strings.Contains(logs.String(), "private-webhook-token") || strings.Contains(logs.String(), "private-telegram-token") || strings.Contains(logs.String(), "private-slack-token") || strings.Contains(logs.String(), remote.URL) {
		t.Fatal("secret in logs")
	}
	t.Log("PASS: real-center login -> Settings encryption -> all four test channels 202/Location -> worker -> delivery poll; alert outbox -> webhook; CSRF and credential redaction")
}
