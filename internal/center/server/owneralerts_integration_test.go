//go:build integration

package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/agentapi"
	"github.com/jooservices/speedtest/internal/center/alerts"
	"github.com/jooservices/speedtest/internal/center/auth"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/nodes"
)

func TestAlertHTTPWorktreeDatabaseIdentityIntegration(t *testing.T) {
	openServerIntegrationPool(t)
}

func TestAlertRuleToAgentEventHTTPFlowIntegration(t *testing.T) {
	pool, ctx, appKey := openServerIntegrationPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)

	passwordHash, err := auth.HashPassword("integration test owner password")
	if err != nil {
		t.Fatal("prepare integration owner:", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,email,password_hash) VALUES ('00000000-0000-4000-8000-000000000901','owner@example.test',$1)`, passwordHash); err != nil {
		t.Fatal("create integration owner:", err)
	}
	authStore, err := auth.NewPostgresStore(pool)
	if err != nil {
		t.Fatal("create auth store:", err)
	}
	authService, err := auth.NewService(authStore, auth.Config{SessionTTL: time.Hour, LoginMaxAttempts: 5, LoginWindow: time.Minute})
	if err != nil {
		t.Fatal("create auth service:", err)
	}
	nodeService := nodes.NewService(pool, appKey)
	nodeID, token := enrollIngestionNode(t, ctx, nodeService, "alert-http-edge")
	alertStore := &alerts.Store{DB: pool, AppKey: appKey}
	ingestion := &agentapi.Postgres{Pool: pool, Hook: alertStore}
	handler := NewHandler(Options{Auth: authService, NodeCredentials: nodeService, AgentAPI: ingestion, OwnerAlerts: alertStore, AgentRateLimit: 10000})
	session, csrf := loginIntegrationOwner(t, handler)

	create := httptest.NewRequest(http.MethodPost, "http://center.test/api/alerts/rules", strings.NewReader(`{"name":"Failed runs","metric":"failed","op":"=","value":1,"forTests":2,"nodes":"all","severity":"crit","channels":["email"],"enabled":true}`))
	ownerHTTPWrite(create, session, csrf)
	create.Header.Set("Idempotency-Key", "alert-http-rule-create")
	createResponse := httptest.NewRecorder()
	handler.ServeHTTP(createResponse, create)
	if createResponse.Code != http.StatusCreated {
		t.Fatalf("create alert rule status = %d: %s", createResponse.Code, createResponse.Body.String())
	}
	ruleID := responseString(t, createResponse.Body.Bytes(), "data", "id")

	failedResult := func(id, ts string) string {
		return fmt.Sprintf(`{"id":%q,"ts":%q,"status":"failed","down":null,"up":null,"ping":null,"jitter":null,"dLat":null,"uLat":null,"loss":null,"serverId":null,"ip":null,"threads":1,"bytesDown":0,"bytesUp":0,"duration":1,"error":"engine unavailable","manual":false,"raw":{},"loadedDown":null,"loadedUp":null,"server":null,"engineISP":null,"engineResultId":null,"engineResultUrl":null,"engineIp":null,"warnings":[]}`, id, ts)
	}
	for i, ts := range []string{"2026-11-03T10:00:00Z", "2026-11-03T10:30:00Z"} {
		id := fmt.Sprintf("60000000-0000-4000-8000-%012x", i+1)
		batch := `{"results":[` + failedResult(id, ts) + `]}`
		outcomes(t, ingestRequest(t, handler, token, http.MethodPost, "/api/agent/results", batch), http.StatusCreated)
	}

	list := httptest.NewRequest(http.MethodGet, "http://center.test/api/alerts/events?state=open", nil)
	list.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
	listResponse := httptest.NewRecorder()
	handler.ServeHTTP(listResponse, list)
	var eventList struct {
		Data []alerts.Event `json:"data"`
	}
	if listResponse.Code != http.StatusOK || json.Unmarshal(listResponse.Body.Bytes(), &eventList) != nil || len(eventList.Data) != 1 {
		t.Fatalf("open alert activity = %d %s", listResponse.Code, listResponse.Body.String())
	}
	event := eventList.Data[0]
	if event.RuleID == nil || *event.RuleID != ruleID || event.NodeID != nodeID || event.Acked || event.ResolvedAt != nil {
		t.Fatalf("opened alert event = %+v", event)
	}

	getEvent := httptest.NewRequest(http.MethodGet, "http://center.test/api/alerts/events/"+event.ID, nil)
	getEvent.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, getEvent)
	if getResponse.Code != http.StatusOK || getResponse.Header().Get("ETag") == "" {
		t.Fatalf("event detail = %d etag=%q %s", getResponse.Code, getResponse.Header().Get("ETag"), getResponse.Body.String())
	}
	ack := httptest.NewRequest(http.MethodPut, "http://center.test/api/alerts/events/"+event.ID+"/ack", strings.NewReader(`{"acked":true}`))
	ownerHTTPWrite(ack, session, csrf)
	ack.Header.Set("If-Match", getResponse.Header().Get("ETag"))
	ackResponse := httptest.NewRecorder()
	handler.ServeHTTP(ackResponse, ack)
	var acked struct {
		Data alerts.Event `json:"data"`
	}
	if ackResponse.Code != http.StatusOK || ackResponse.Header().Get("ETag") == getResponse.Header().Get("ETag") || json.Unmarshal(ackResponse.Body.Bytes(), &acked) != nil || !acked.Data.Acked {
		t.Fatalf("ack event = %d etag=%q %s", ackResponse.Code, ackResponse.Header().Get("ETag"), ackResponse.Body.String())
	}

	openAgain := httptest.NewRequest(http.MethodGet, "http://center.test/api/alerts/events?state=open", nil)
	openAgain.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
	openAgainResponse := httptest.NewRecorder()
	handler.ServeHTTP(openAgainResponse, openAgain)
	var cleared struct {
		Meta struct {
			Pagination struct {
				Total int `json:"total"`
			} `json:"pagination"`
		} `json:"meta"`
	}
	if openAgainResponse.Code != http.StatusOK || json.Unmarshal(openAgainResponse.Body.Bytes(), &cleared) != nil || cleared.Meta.Pagination.Total != 0 {
		t.Fatalf("acknowledged alert remains open: %d %s", openAgainResponse.Code, openAgainResponse.Body.String())
	}
	t.Log("PASS: owner session creates a rule; authenticated agent failures open one durable event; owner acknowledges it with ETag and open activity clears")
}
