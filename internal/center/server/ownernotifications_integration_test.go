//go:build integration

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jooservices/speedtest/internal/center/auth"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/notify"
	"github.com/jooservices/speedtest/internal/center/settings"
)

const (
	deliveryFilterNodeID = "10000000-0000-4000-8000-000000000124"
	deliveryFilterEvent1 = "a0000000-0000-4000-8000-000000000124"
	deliveryFilterEvent2 = "a0000000-0000-4000-8000-000000000125"
	deliveryFilterEvent3 = "a0000000-0000-4000-8000-000000000126"
	deliveryFilterEvent4 = "a0000000-0000-4000-8000-000000000127"
	deliveryFilterSecret = "https://hooks.example.test/PRIVATE-TOKEN-124"
)

type deliveryFilterPage struct {
	Data []notify.Delivery `json:"data"`
	Meta struct {
		Pagination struct {
			CurrentPage int `json:"current_page"`
			PerPage     int `json:"per_page"`
			Total       int `json:"total"`
			LastPage    int `json:"last_page"`
		} `json:"pagination"`
	} `json:"meta"`
}

func TestNotificationDeliveryFiltersIntegrationAC1AC2(t *testing.T) {
	pool, ctx, _ := openServerIntegrationPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)

	hash, err := auth.HashPassword("integration test owner password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,email,password_hash) VALUES ('00000000-0000-4000-8000-000000000901','owner@example.test',$1)`, hash); err != nil {
		t.Fatal(err)
	}
	authStore, err := auth.NewPostgresStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	authService, err := auth.NewService(authStore, auth.Config{SessionTTL: time.Hour, LoginMaxAttempts: 5, LoginWindow: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(Options{Auth: authService, OwnerNotifications: &notify.Store{DB: pool}})
	session, _ := loginIntegrationOwner(t, handler)

	seedNotificationDeliveryFilters(t, pool, ctx)

	get := func(query string, authenticate bool) (*httptest.ResponseRecorder, deliveryFilterPage) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "http://center.test/api/notifications/deliveries"+query, nil)
		if authenticate {
			request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		var page deliveryFilterPage
		if response.Code == http.StatusOK {
			if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
				t.Fatalf("decode %s: %v", query, err)
			}
		}
		return response, page
	}

	assertDeliveryFilterPage(t, get, "?event="+deliveryFilterEvent1+"&status=sent&channel=email", []string{"f0000000-0000-4000-8000-000000000001"}, 1, 1, 1)
	assertDeliveryFilterPage(t, get, "?status=sent&channel=email", []string{"f0000000-0000-4000-8000-000000000007", "f0000000-0000-4000-8000-000000000001"}, 1, 2, 1)
	assertDeliveryFilterPage(t, get, "?status=sent&channel=email&size=1&page=1", []string{"f0000000-0000-4000-8000-000000000007"}, 1, 2, 2)
	assertDeliveryFilterPage(t, get, "?status=sent&channel=email&size=1&page=2", []string{"f0000000-0000-4000-8000-000000000001"}, 2, 2, 2)
	assertDeliveryFilterPage(t, get, "?event="+deliveryFilterEvent1+"&size=2&page=1", []string{"f0000000-0000-4000-8000-000000000004", "f0000000-0000-4000-8000-000000000003"}, 1, 4, 2)
	assertDeliveryFilterPage(t, get, "?event="+deliveryFilterEvent1+"&size=2&page=2", []string{"f0000000-0000-4000-8000-000000000002", "f0000000-0000-4000-8000-000000000001"}, 2, 4, 2)
	assertDeliveryFilterPage(t, get, "?status=sent&size=2&page=1", []string{"f0000000-0000-4000-8000-000000000009", "f0000000-0000-4000-8000-000000000007"}, 1, 4, 2)
	assertDeliveryFilterPage(t, get, "?status=sent&size=2&page=2", []string{"f0000000-0000-4000-8000-000000000002", "f0000000-0000-4000-8000-000000000001"}, 2, 4, 2)
	assertDeliveryFilterPage(t, get, "?channel=telegram&size=1&page=1", []string{"f0000000-0000-4000-8000-000000000008"}, 1, 2, 2)
	assertDeliveryFilterPage(t, get, "?channel=telegram&size=1&page=2", []string{"f0000000-0000-4000-8000-000000000002"}, 2, 2, 2)
	assertDeliveryFilterPage(t, get, "?channel=telegram", []string{"f0000000-0000-4000-8000-000000000008", "f0000000-0000-4000-8000-000000000002"}, 1, 2, 1)
	assertDeliveryFilterPage(t, get, "?status=failed", []string{"f0000000-0000-4000-8000-000000000008", "f0000000-0000-4000-8000-000000000004", "f0000000-0000-4000-8000-000000000005"}, 1, 3, 1)
	assertDeliveryFilterPage(t, get, "?event="+deliveryFilterEvent2+"&status=failed", []string{"f0000000-0000-4000-8000-000000000005"}, 1, 1, 1)
	assertDeliveryFilterPage(t, get, "?event="+deliveryFilterEvent1+"&status=sent&channel=telegram&size=1&page=1", []string{"f0000000-0000-4000-8000-000000000002"}, 1, 1, 1)
	assertDeliveryFilterPage(t, get, "?event="+deliveryFilterEvent1+"&status=sent&channel=telegram&size=1&page=2", nil, 2, 1, 1)

	if response, _ := get("?status=sent&channel=email", false); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated delivery filter = %d", response.Code)
	}
	for _, query := range []string{
		"?status=nope",
		"?status=",
		"?status=sent&status=failed",
		"?channel=nope",
		"?channel=",
		"?channel=email&channel=telegram",
		"?event=",
		"?event=not-a-uuid",
		"?event=" + deliveryFilterEvent1 + "&event=" + deliveryFilterEvent1,
		"?status=%ZZ",
		"?channel=%ZZ",
		"?event=%ZZ",
		"?status=sent&status=%ZZ",
		"?channel=email&channel=%ZZ",
		"?page=%ZZ",
	} {
		if response, _ := get(query, true); response.Code != http.StatusUnprocessableEntity {
			t.Errorf("invalid filter %s = %d", query, response.Code)
		}
		if response, _ := get(query, false); response.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated invalid filter %s = %d", query, response.Code)
		}
	}

	response, _ := get("?event="+deliveryFilterEvent1+"&status=sent&channel=email", true)
	body := response.Body.String()
	for _, leaked := range []string{deliveryFilterSecret, "internal-only-marker", "privateEndpoint", "next_attempt_at", "payload"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("delivery response exposes %q: %s", leaked, body)
		}
	}
	t.Log("PASS: authenticated delivery list applies event/status/channel filters, pages newest first with totals; invalid and unauthenticated calls are refused and the response is secret-free")
}

func assertDeliveryFilterPage(t *testing.T, get func(string, bool) (*httptest.ResponseRecorder, deliveryFilterPage), query string, wantIDs []string, wantPage, wantTotal, wantLast int) {
	t.Helper()
	response, page := get(query, true)
	if response.Code != http.StatusOK {
		t.Fatalf("%s = %d %s", query, response.Code, response.Body.String())
	}
	if page.Meta.Pagination.CurrentPage != wantPage || page.Meta.Pagination.Total != wantTotal || page.Meta.Pagination.LastPage != wantLast || page.Meta.Pagination.PerPage < 1 {
		t.Fatalf("%s pagination = %+v", query, page.Meta.Pagination)
	}
	if len(page.Data) != len(wantIDs) {
		t.Fatalf("%s returned %d deliveries, want %d", query, len(page.Data), len(wantIDs))
	}
	for i, id := range wantIDs {
		if page.Data[i].ID != id {
			t.Fatalf("%s delivery %d = %s, want %s", query, i, page.Data[i].ID, id)
		}
	}
}

func seedNotificationDeliveryFilters(t *testing.T, pool *pgxpool.Pool, ctx context.Context) {
	t.Helper()
	execute := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	execute(`INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES($1,'delivery-filter-edge',100,50)`, deliveryFilterNodeID)
	for _, event := range []string{deliveryFilterEvent1, deliveryFilterEvent2, deliveryFilterEvent3, deliveryFilterEvent4} {
		execute(`INSERT INTO alert_events(id,node_id,severity,ts,title) VALUES($1,$2,'warn','2026-11-01T09:00:00Z','Delivery filter fixture')`, event, deliveryFilterNodeID)
	}
	deliveries := []struct {
		id, event, channel, status, created, errText string
		attempts                                     int
	}{
		{"f0000000-0000-4000-8000-000000000001", deliveryFilterEvent1, "email", "sent", "2026-11-01T10:00:00Z", "", 1},
		{"f0000000-0000-4000-8000-000000000002", deliveryFilterEvent1, "telegram", "sent", "2026-11-01T12:00:00Z", "", 2},
		{"f0000000-0000-4000-8000-000000000003", deliveryFilterEvent1, "webhook", "pending", "2026-11-01T14:00:00Z", "", 0},
		{"f0000000-0000-4000-8000-000000000004", deliveryFilterEvent1, "slack", "failed", "2026-11-01T16:00:00Z", "notification attempt failed", 1},
		{"f0000000-0000-4000-8000-000000000005", deliveryFilterEvent2, "email", "failed", "2026-11-01T11:00:00Z", "notification attempt failed", 1},
		{"f0000000-0000-4000-8000-000000000006", deliveryFilterEvent2, "slack", "pending", "2026-11-01T15:00:00Z", "", 0},
		{"f0000000-0000-4000-8000-000000000007", deliveryFilterEvent3, "email", "sent", "2026-11-01T13:00:00Z", "", 1},
		{"f0000000-0000-4000-8000-000000000008", deliveryFilterEvent3, "telegram", "failed", "2026-11-01T17:00:00Z", "notification attempt failed", 3},
		{"f0000000-0000-4000-8000-000000000009", deliveryFilterEvent4, "webhook", "sent", "2026-11-01T18:00:00Z", "", 1},
	}
	execute(`UPDATE alert_events SET payload=$2::jsonb WHERE id=$1::uuid`, deliveryFilterEvent1, `{"privateEndpoint":"internal-only-marker"}`)
	for _, d := range deliveries {
		execute(`INSERT INTO notification_deliveries(id,event_id,channel,status,attempts,created_at,last_attempt,error,next_attempt_at)
VALUES($1,$2::uuid,$3,$4,$5,$6::timestamptz,CASE WHEN $5>0 THEN $6::timestamptz ELSE NULL END,NULLIF($7,'')::varchar,$6::timestamptz)`,
			d.id, d.event, d.channel, d.status, d.attempts, d.created, d.errText)
	}
	cipher, err := settings.NewCipherFromKey([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt([]byte(deliveryFilterSecret))
	if err != nil {
		t.Fatal(err)
	}
	execute(`INSERT INTO settings(key,secret_ciphertext) VALUES('notify.webhook.secret',$1)`, encrypted)
}
