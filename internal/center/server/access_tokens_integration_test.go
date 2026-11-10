//go:build integration

package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jooservices/speedtest/internal/center/agentapi"
	"github.com/jooservices/speedtest/internal/center/alerts"
	"github.com/jooservices/speedtest/internal/center/apitoken"
	"github.com/jooservices/speedtest/internal/center/audit"
	"github.com/jooservices/speedtest/internal/center/auth"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/nodes"
	"github.com/jooservices/speedtest/internal/center/ownerapi"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestAccessTokenMCPHTTPFlowIntegration(t *testing.T) {
	fixture := newAccessTokenFixture(t)
	readToken, readID := fixture.issue(t, "integration-read", apitoken.ReadOnly)
	fixture.verifyStoredDigest(t, readToken, readID)
	fixture.verifyReadREST(t, readToken)
	fixture.verifyReadMCP(t, readToken, readID)
	writeToken, writeID := fixture.issue(t, "integration-write", apitoken.ReadWrite)
	fixture.verifyMCPReadAndWrites(t, writeToken)
	fixture.verifyWriteAuditAndSafeLogs(t, readToken, writeToken)
	fixture.verifyExpiryAndRevoke(t, readToken, readID, writeToken, writeID)
	fixture.verifyTokenAudit(t, "access_token.create", 2)
}

type accessTokenFixture struct {
	pool    *pgxpool.Pool
	ctx     context.Context
	handler http.Handler
	session string
	csrf    string
	logs    *bytes.Buffer
	nodeID  string
	alertID string
}

func newAccessTokenFixture(t *testing.T) *accessTokenFixture {
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
	logs := &bytes.Buffer{}
	owner := &ownerapi.Store{DB: pool}
	alertStore := &alerts.Store{DB: pool, AppKey: appKey}
	nodeID, alertID := seedAccessTokenMCPData(t, pool, ctx)
	handler := NewHandler(Options{Auth: authService, NodeCredentials: nodes.NewService(pool, appKey), OwnerNodes: owner, OwnerSchedules: owner,
		OwnerResults: owner, OwnerAlerts: alertStore, Audit: &audit.Store{DB: pool}, AccessTokens: &apitoken.Store{DB: pool}, Logger: slog.New(slog.NewJSONHandler(logs, nil))})
	session, csrf := loginIntegrationOwner(t, handler)
	return &accessTokenFixture{pool: pool, ctx: ctx, handler: handler, session: session, csrf: csrf, logs: logs, nodeID: nodeID, alertID: alertID}
}

func seedAccessTokenMCPData(t *testing.T, pool *pgxpool.Pool, ctx context.Context) (string, string) {
	t.Helper()
	nodeID, alertID := "00000000-0000-4000-8000-000000000921", "00000000-0000-4000-8000-000000000922"
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps,created_at) VALUES ($1,'mcp-fixture-node',100,50,$2)`, nodeID, now); err != nil {
		t.Fatal("seed MCP node:", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO schedules(node_id,interval_minutes,offset_minutes,enabled) VALUES ($1,30,0,true)`, nodeID); err != nil {
		t.Fatal("seed MCP schedule:", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO engine_schedules(node_id,engine,interval_minutes,offset_minutes,enabled) VALUES ($1,'ookla',30,0,true)`, nodeID); err != nil {
		t.Fatal("seed MCP engine schedule:", err)
	}
	seedAccessTokenResults(t, pool, ctx, nodeID, now)
	if _, err := pool.Exec(ctx, `INSERT INTO alert_events(id,node_id,severity,ts,title,detail) VALUES ($1,$2,'warn',$3,'MCP fixture alert','')`, alertID, nodeID, now); err != nil {
		t.Fatal("seed MCP alert:", err)
	}
	return nodeID, alertID
}

func seedAccessTokenResults(t *testing.T, pool *pgxpool.Pool, ctx context.Context, nodeID string, now time.Time) {
	t.Helper()
	store := &agentapi.Postgres{Pool: pool}
	for index, fixture := range []struct{ id, ip string }{
		{"00000000-0000-4000-8000-000000000923", "203.0.113.21"},
		{"00000000-0000-4000-8000-000000000924", "203.0.113.22"},
	} {
		down, up, ping, serverID := 80.0+float64(index), 40.0, 8.0, 1
		ip := fixture.ip
		result := agentapi.Result{Engine: "ookla", ID: fixture.id, TS: now.Add(time.Duration(index-2) * time.Minute).Format(time.RFC3339Nano),
			Status: "ok", Down: &down, Up: &up, Ping: &ping, ServerID: &serverID, IP: &ip, Threads: 1, Raw: json.RawMessage(`{}`),
			Warnings: []string{}, Server: &agentapi.Server{ID: &serverID, Name: "MCP fixture", Host: "mcp.example.test"}}
		if _, err := store.SaveResult(ctx, nodeID, result); err != nil {
			t.Fatal("seed MCP result:", err)
		}
	}
}

func (f *accessTokenFixture) issue(t *testing.T, name string, scope apitoken.Scope) (string, string) {
	t.Helper()
	body, _ := json.Marshal(apitoken.CreateInput{Name: name, Scope: scope})
	request := httptest.NewRequest(http.MethodPost, "http://center.test/api/access-tokens", strings.NewReader(string(body)))
	ownerHTTPWrite(request, f.session, f.csrf)
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create %s token status = %d", scope, response.Code)
	}
	return responseString(t, response.Body.Bytes(), "data", "token"), responseString(t, response.Body.Bytes(), "data", "id")
}

func (f *accessTokenFixture) verifyStoredDigest(t *testing.T, raw, id string) {
	t.Helper()
	var stored []byte
	if err := f.pool.QueryRow(f.ctx, `SELECT token_hash FROM api_access_tokens WHERE id=$1`, id).Scan(&stored); err != nil {
		t.Fatal("load token digest:", err)
	}
	expected := sha256.Sum256([]byte(raw))
	if len(stored) != sha256.Size || subtle.ConstantTimeCompare(stored, expected[:]) != 1 {
		t.Fatal("database does not contain only the token SHA-256 digest")
	}
}

func (f *accessTokenFixture) verifyReadREST(t *testing.T, raw string) {
	t.Helper()
	metadata := httptest.NewRequest(http.MethodGet, "http://center.test/api/access-tokens", nil)
	metadata.AddCookie(&http.Cookie{Name: sessionCookieName, Value: f.session})
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, metadata)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), raw) || strings.Contains(response.Body.String(), "token_hash") {
		t.Fatalf("token metadata response status = %d", response.Code)
	}
	if got := f.request(http.MethodGet, "/api/nodes", raw, "").Code; got != http.StatusOK {
		t.Fatalf("bearer REST read status = %d", got)
	}
	denied := f.request(http.MethodPost, "/api/nodes", raw, `{"name":"denied","expectedDown":10,"expectedUp":10}`)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("read token node creation status = %d, want 403", denied.Code)
	}
}

func (f *accessTokenFixture) verifyReadMCP(t *testing.T, raw, id string) {
	t.Helper()
	endpoint := httptest.NewServer(f.handler)
	defer endpoint.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "integration-client", Version: "1"}, nil)
	remote, err := client.Connect(f.ctx, &mcp.StreamableClientTransport{Endpoint: endpoint.URL + "/mcp", HTTPClient: bearerClient(endpoint.Client(), raw)}, nil)
	if err != nil {
		t.Fatal("connect streamable MCP client:", err)
	}
	defer func() {
		if err := remote.Close(); err != nil {
			t.Errorf("close MCP client session: %v", err)
		}
	}()
	tools, err := remote.ListTools(f.ctx, nil)
	if err != nil || len(tools.Tools) != 8 {
		t.Fatalf("read token MCP tool count = %d, error = %v", len(tools.Tools), err)
	}
	result, err := remote.CallTool(f.ctx, &mcp.CallToolParams{Name: "list_nodes", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatalf("MCP list_nodes error = %v, tool-error=%t", err, result != nil && result.IsError)
	}
	var used time.Time
	if err := f.pool.QueryRow(f.ctx, `SELECT last_used_at FROM api_access_tokens WHERE id=$1`, id).Scan(&used); err != nil || used.IsZero() {
		t.Fatal("successful REST or MCP call did not update last-used time")
	}
}

func (f *accessTokenFixture) verifyMCPReadAndWrites(t *testing.T, token string) {
	t.Helper()
	endpoint := httptest.NewServer(f.handler)
	defer endpoint.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "integration-write-client", Version: "1"}, nil)
	remote, err := client.Connect(f.ctx, &mcp.StreamableClientTransport{Endpoint: endpoint.URL + "/mcp", HTTPClient: bearerClient(endpoint.Client(), token)}, nil)
	if err != nil {
		t.Fatal("connect read-write MCP client:", err)
	}
	defer func() { _ = remote.Close() }()
	f.verifyMCPResultPagination(t, remote)
	f.verifyMCPScheduleWrite(t, remote)
	f.verifyMCPAlertWrite(t, remote)
}

func (f *accessTokenFixture) verifyMCPResultPagination(t *testing.T, remote *mcp.ClientSession) {
	t.Helper()
	ids := make([]string, 0, 2)
	for page := 1; page <= 2; page++ {
		result := callMCPTool(t, remote, "list_results", map[string]any{"engine": "ookla", "nodes": []any{f.nodeID}, "range": "7d",
			"status": "ok", "sort": "ts", "dir": "desc", "page": page, "size": 1})
		if result.IsError {
			t.Fatalf("list_results page %d returned an MCP error: %+v", page, result.Content)
		}
		ids = append(ids, firstMCPRowID(t, result))
	}
	if ids[0] == ids[1] || ids[0] == "" || ids[1] == "" {
		t.Fatalf("MCP pagination returned duplicate or empty result IDs: %v", ids)
	}
	alerts := callMCPTool(t, remote, "list_alerts", map[string]any{"state": "all", "page": 1, "size": 1})
	if alerts.IsError || firstMCPRowID(t, alerts) != f.alertID {
		t.Fatalf("list_alerts did not return the seeded event: %+v", alerts.StructuredContent)
	}
}

func (f *accessTokenFixture) verifyMCPScheduleWrite(t *testing.T, remote *mcp.ClientSession) {
	t.Helper()
	current := callMCPTool(t, remote, "get_schedule", map[string]any{"node": f.nodeID})
	initial := integrationETag(t, current)
	updated := callMCPTool(t, remote, "update_schedule", map[string]any{"node": f.nodeID, "If-Match": initial, "interval": 45, "offset": 5, "enabled": true})
	if updated.IsError {
		t.Fatalf("classic update_schedule failed: %+v", updated.Content)
	}
	stale := callMCPTool(t, remote, "update_schedule", map[string]any{"node": f.nodeID, "If-Match": initial, "interval": 60, "offset": 0, "enabled": true})
	if !stale.IsError {
		t.Fatal("stale schedule ETag unexpectedly succeeded")
	}
	engineUpdate := callMCPTool(t, remote, "update_schedule", map[string]any{"node": f.nodeID, "If-Match": integrationETag(t, updated),
		"engines": []any{map[string]any{"engine": "ookla", "interval": 60, "offset": 3, "enabled": true}}})
	if engineUpdate.IsError {
		t.Fatalf("engine schedule update failed: %+v", engineUpdate.Content)
	}
	missing := callMCPTool(t, remote, "update_schedule", map[string]any{"node": f.nodeID, "interval": 30, "offset": 0, "enabled": true})
	if !missing.IsError {
		t.Fatal("schedule update without If-Match unexpectedly succeeded")
	}
}

func (f *accessTokenFixture) verifyMCPAlertWrite(t *testing.T, remote *mcp.ClientSession) {
	t.Helper()
	current := callMCPTool(t, remote, "get_alert", map[string]any{"id": f.alertID})
	initial := integrationETag(t, current)
	updated := callMCPTool(t, remote, "acknowledge_alert", map[string]any{"id": f.alertID, "If-Match": initial, "acked": true})
	if updated.IsError || integrationETag(t, updated) == initial {
		t.Fatalf("alert acknowledgement failed or did not advance ETag: %+v", updated.Content)
	}
	stale := callMCPTool(t, remote, "acknowledge_alert", map[string]any{"id": f.alertID, "If-Match": initial, "acked": false})
	if !stale.IsError {
		t.Fatal("stale alert ETag unexpectedly succeeded")
	}
	var acked bool
	if err := f.pool.QueryRow(f.ctx, `SELECT acked FROM alert_events WHERE id=$1`, f.alertID).Scan(&acked); err != nil || !acked {
		t.Fatalf("alert acknowledgement was not persisted: acked=%t err=%v", acked, err)
	}
}

func callMCPTool(t *testing.T, remote *mcp.ClientSession, name string, arguments map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := remote.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("MCP call %s: %v", name, err)
	}
	return result
}

func firstMCPRowID(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	envelope, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("MCP result is not an API envelope: %#v", result.StructuredContent)
	}
	rows, ok := envelope["data"].([]any)
	if !ok || len(rows) == 0 {
		return ""
	}
	row, _ := rows[0].(map[string]any)
	id, _ := row["id"].(string)
	return id
}

func integrationETag(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	envelope, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("MCP response is not an API envelope: %#v", result.StructuredContent)
	}
	meta, _ := envelope["meta"].(map[string]any)
	etag, _ := meta["etag"].(string)
	if etag == "" {
		t.Fatalf("MCP response does not expose its ETag: %#v", envelope)
	}
	return etag
}

func (f *accessTokenFixture) verifyWriteAuditAndSafeLogs(t *testing.T, readToken, writeToken string) {
	t.Helper()
	response := f.request(http.MethodPost, "/api/nodes", writeToken, `{"name":"token-created-node","expectedDown":100,"expectedUp":50}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("read-write bearer create node status = %d", response.Code)
	}
	var count int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM audit_log WHERE action='node.create' AND actor='owner@example.test'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("bearer write audit count = %d", count)
	}
	if !strings.Contains(f.logs.String(), "/api/nodes") || strings.Contains(f.logs.String(), readToken) || strings.Contains(f.logs.String(), writeToken) {
		t.Fatal("request log omitted an operation or disclosed a bearer token")
	}
}

func (f *accessTokenFixture) verifyExpiryAndRevoke(t *testing.T, readToken, readID, writeToken, writeID string) {
	t.Helper()
	expired := time.Now().UTC().Add(-time.Minute)
	if _, err := f.pool.Exec(f.ctx, `UPDATE api_access_tokens SET expires_at=$2 WHERE id=$1`, readID, expired); err != nil {
		t.Fatal("expire test token:", err)
	}
	if got := f.request(http.MethodGet, "/api/nodes", readToken, "").Code; got != http.StatusUnauthorized {
		t.Fatalf("expired token status = %d, want 401", got)
	}
	revoke := httptest.NewRequest(http.MethodDelete, "http://center.test/api/access-tokens/"+writeID, nil)
	ownerHTTPWrite(revoke, f.session, f.csrf)
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, revoke)
	if response.Code != http.StatusNoContent {
		t.Fatalf("revoke token status = %d", response.Code)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE api_access_tokens SET expires_at=NULL WHERE id=$1`, writeID); err != nil {
		t.Fatal("clear expiry on revoked token:", err)
	}
	if got := f.request(http.MethodGet, "/api/nodes", writeToken, "").Code; got != http.StatusUnauthorized {
		t.Fatalf("revoked token status = %d, want 401", got)
	}
	f.verifyTokenAudit(t, "access_token.revoke", 1)
}

func (f *accessTokenFixture) verifyTokenAudit(t *testing.T, action string, want int) {
	t.Helper()
	var got int
	query := `SELECT count(*) FROM audit_log WHERE action=$1 AND actor='owner@example.test'`
	if err := f.pool.QueryRow(f.ctx, query, action).Scan(&got); err != nil || got != want {
		t.Fatalf("%s audit count = %d, want %d", action, got, want)
	}
}

func (f *accessTokenFixture) request(method, path, token, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "http://center.test"+path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Idempotency-Key", "token-created-node")
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	return response
}

type bearerRoundTripper struct {
	base  http.RoundTripper
	token string
}

func (t bearerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	copy := request.Clone(request.Context())
	copy.Header = request.Header.Clone()
	copy.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(copy)
}

func bearerClient(base *http.Client, token string) *http.Client {
	return &http.Client{Transport: bearerRoundTripper{base: base.Transport, token: token}, Timeout: base.Timeout}
}
