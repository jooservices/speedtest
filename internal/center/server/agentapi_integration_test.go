//go:build integration

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jooservices/speedtest/internal/center/agentapi"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/nodes"
)

const ingestionResult = `{"id":"00000000-0000-4000-8000-000000000001","ts":"2026-11-03T00:00:00Z","status":"ok","down":123.4,"up":56.7,"ping":3,"jitter":null,"dLat":null,"uLat":null,"loss":null,"serverId":1,"ip":"203.0.113.1","threads":4,"bytesDown":1024,"bytesUp":512,"duration":2.3,"error":null,"manual":false,"raw":{"nested":[1,true]},"loadedDown":{"low":null,"iqm":4,"high":10},"loadedUp":{"low":1,"iqm":2,"high":3},"server":{"id":1,"name":"edge","city":"city","sponsor":"isp","host":"test.example"},"engineISP":"isp","engineResultId":"engine-1","engineResultUrl":"https://example.test/result/1","engineIp":"2001:db8::1","warnings":["IP mismatch"]}`

func ingestRequest(t *testing.T, h http.Handler, token, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.RemoteAddr = "198.51.100.10:1234"
	r.Header.Set("X-Forwarded-For", "203.0.113.99")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func outcomes(t *testing.T, w *httptest.ResponseRecorder, want ...int) {
	t.Helper()
	var response struct {
		Data []agentapi.Outcome `json:"data"`
	}
	if w.Code != 207 || json.Unmarshal(w.Body.Bytes(), &response) != nil || len(response.Data) != len(want) {
		t.Fatalf("batch: %d %s", w.Code, w.Body.String())
	}
	for i, status := range want {
		if response.Data[i].Status != status || response.Data[i].Index != i {
			t.Fatalf("item %d: %+v", i, response.Data[i])
		}
	}
}
func enrollIngestionNode(t *testing.T, ctx context.Context, service *nodes.Service, name string) (string, string) {
	t.Helper()
	data, _, err := service.CreateNode(ctx, nodes.Input{Name: name, ExpectedDown: 300, ExpectedUp: 100, Threads: 4, Enabled: true}, "owner:test", name)
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		Node struct {
			ID string `json:"id"`
		}
		Token string
	}
	if err = json.Unmarshal(data, &created); err != nil {
		t.Fatal(err)
	}
	enrolled, err := service.Enroll(ctx, created.Token, name, "host", "v1")
	if err != nil {
		t.Fatal(err)
	}
	return created.Node.ID, enrolled["credential"].(string)
}

type recordingHook struct {
	calls int
	fail  bool
}

func (h *recordingHook) AfterResultStored(ctx context.Context, tx pgx.Tx, node string, r agentapi.Result) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM results WHERE id=$1 AND node_id=$2)`, r.ID, node).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("hook called before insertion")
	}
	if h.fail {
		return fmt.Errorf("hook failure")
	}
	h.calls++
	return nil
}
func TestAgentIngestionProtocolIntegration(t *testing.T) {
	pool, ctx, key := openServerIntegrationPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	service := nodes.NewService(pool, key)
	node, token := enrollIngestionNode(t, ctx, service, "ingestion-edge")
	foreign, foreignToken := enrollIngestionNode(t, ctx, service, "other-edge")
	hook := &recordingHook{}
	store := &agentapi.Postgres{Pool: pool, Hook: hook}
	handler := NewHandler(Options{NodeCredentials: service, AgentAPI: store, AgentRateLimit: 10000, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil))})
	batch := `{"results":[` + ingestionResult + `,{"id":"00000000-0000-4000-8000-000000000002"},{"id":"bad"}]}`
	outcomes(t, ingestRequest(t, handler, token, "POST", "/api/agent/results", batch), 201, 422, 422)
	outcomes(t, ingestRequest(t, handler, token, "POST", "/api/agent/results", `{"results":[`+ingestionResult+`]}`), 200)
	changed := strings.Replace(ingestionResult, `"down":123.4`, `"down":124.4`, 1)
	outcomes(t, ingestRequest(t, handler, token, "POST", "/api/agent/results", `{"results":[`+changed+`]}`), 409)
	cross := ingestRequest(t, handler, foreignToken, "POST", "/api/agent/results", `{"results":[`+ingestionResult+`]}`)
	outcomes(t, cross, 409)
	if strings.Contains(cross.Body.String(), node) || strings.Contains(cross.Body.String(), "123.4") {
		t.Fatal("foreign data leaked")
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM results`).Scan(&count); err != nil || count != 1 || hook.calls != 1 {
		t.Fatal("idempotency or hook", count, hook.calls, err)
	}
	// Every normalized/engine field is checked by replay equality, then compare
	// the durable row to the parsed payload after numeric/IP/JSON normalization.
	result, err := agentapi.ParseResult([]byte(ingestionResult))
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := store.SaveResult(ctx, node, result)
	if err != nil || !duplicate {
		t.Fatal(duplicate, err)
	}
	var raw, loaded, server, warnings []byte
	var engineISP, engineID, engineURL, engineIP string
	if err = pool.QueryRow(ctx, `SELECT raw,loaded_down,server,warnings,engine_isp,engine_result_id,engine_result_url,host(engine_ip) FROM results WHERE id=$1`, result.ID).Scan(&raw, &loaded, &server, &warnings, &engineISP, &engineID, &engineURL, &engineIP); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "nested") || !strings.Contains(string(loaded), "iqm") || !strings.Contains(string(server), "test.example") || !strings.Contains(string(warnings), "IP mismatch") || engineISP != "isp" || engineID != "engine-1" || engineURL != "https://example.test/result/1" || engineIP != "2001:db8::1" {
		t.Fatal("engine fields lost")
	}
	// Hook errors roll back both the result and newly inserted address.
	hook.fail = true
	failed := result
	failed.ID = "00000000-0000-4000-8000-000000000003"
	failedIP := "203.0.113.3"
	failed.IP = &failedIP
	if _, err = store.SaveResult(ctx, node, failed); err == nil {
		t.Fatal("hook failure ignored")
	}
	hook.fail = false
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM ip_addresses WHERE ip='203.0.113.3'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("orphan address", err)
	}
	// Concurrent identical submissions acknowledge exactly one insert.
	concurrent := result
	concurrent.ID = "00000000-0000-4000-8000-000000000004"
	store.Hook = nil
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	inserted := make(chan bool, 8)
	for range 8 {
		wg.Go(func() { duplicate, err := store.SaveResult(ctx, node, concurrent); errs <- err; inserted <- !duplicate })
	}
	wg.Wait()
	close(errs)
	close(inserted)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	count = 0
	for value := range inserted {
		if value {
			count++
		}
	}
	if count != 1 {
		t.Fatal("concurrent inserts", count)
	}
	first := ingestRequest(t, handler, token, "GET", "/api/agent/config", "")
	if first.Code != 200 || !strings.Contains(first.Body.String(), `"schedule":{"enabled":true,"interval":60,"offset":0}`) {
		t.Fatal(first.Code, first.Body.String())
	}
	etag := first.Header().Get("ETag")
	r := httptest.NewRequest("GET", "/api/agent/config", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("If-None-Match", etag)
	cached := httptest.NewRecorder()
	handler.ServeHTTP(cached, r)
	if cached.Code != 304 || cached.Body.Len() != 0 {
		t.Fatal(cached)
	}
	if _, err = pool.Exec(ctx, `UPDATE nodes SET name='renamed',binding_interface='eth0',server_id=7,auto_update=true,primary_engine='librespeed',installed_engines=ARRAY['ookla','librespeed'],config_revision=config_revision+1 WHERE id=$1`, node); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE schedules SET interval_minutes=60,offset_minutes=10,revision=revision+1 WHERE node_id=$1`, node); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE engine_schedules SET interval_minutes=60,offset_minutes=10,revision=revision+1 WHERE node_id=$1 AND engine='ookla'`, node); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO engine_schedules(node_id,engine,interval_minutes,offset_minutes,enabled,revision) VALUES ($1,'librespeed',60,0,true,1)`, node); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO settings(key,value) VALUES ('quietStart','"22:00"'),('quietEnd','"06:00"'),('timezone','"Asia/Ho_Chi_Minh"'),('agentRelease','{"version":"v2","url":"https://example.test/agent","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}')`); err != nil {
		t.Fatal(err)
	}
	updated := ingestRequest(t, handler, token, "GET", "/api/agent/config", "")
	if updated.Code != 200 || updated.Header().Get("ETag") == etag || !strings.Contains(updated.Body.String(), `"engine":"librespeed"`) || !strings.Contains(updated.Body.String(), `"schedules"`) || !strings.Contains(updated.Body.String(), "renamed") || !strings.Contains(updated.Body.String(), "22:00") {
		t.Fatal(updated.Body.String())
	}
	heartbeat := ingestRequest(t, handler, token, "POST", "/api/agent/heartbeat", `{"agent":"2.0","uptime":30,"lastTest":null}`)
	if heartbeat.Code != 200 {
		t.Fatal(heartbeat.Body.String())
	}
	var version string
	var seen time.Time
	if err = pool.QueryRow(ctx, `SELECT agent,last_seen_at FROM nodes WHERE id=$1`, node).Scan(&version, &seen); err != nil || version != "2.0" || time.Since(seen) > time.Minute {
		t.Fatal("heartbeat", err)
	}
	empty := ingestRequest(t, handler, token, "GET", "/api/agent/commands", "")
	if empty.Code != 200 || !strings.Contains(empty.Body.String(), `"data":[]`) {
		t.Fatal(empty.Body.String())
	}
	commandID := "00000000-0000-4000-8000-000000000101"
	if _, err = pool.Exec(ctx, `INSERT INTO agent_commands(id,node_id,type) VALUES ($1,$2,'run_now')`, commandID, node); err != nil {
		t.Fatal(err)
	}
	poll := ingestRequest(t, handler, token, "GET", "/api/agent/commands", "")
	if poll.Code != 200 || !strings.Contains(poll.Body.String(), commandID) {
		t.Fatal(poll.Body.String())
	}
	path := "/api/agent/commands/" + commandID + "/ack"
	if w := ingestRequest(t, handler, foreignToken, "POST", path, `{"status":"running"}`); w.Code != 404 {
		t.Fatal(w.Code)
	}
	for _, tc := range []struct {
		data string
		want int
	}{{`{"status":"running","pct":50,"phase":"download"}`, 200}, {`{"status":"running","pct":40}`, 409}, {`{"status":"succeeded","pct":100,"resultId":"00000000-0000-4000-8000-000000000001"}`, 200}, {`{"status":"succeeded","pct":100,"resultId":"00000000-0000-4000-8000-000000000001"}`, 200}, {`{"status":"failed","error":"failure"}`, 409}} {
		w := ingestRequest(t, handler, token, "POST", path, tc.data)
		if w.Code != tc.want {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := ingestRequest(t, handler, token, "GET", "/api/agent/commands", ""); !strings.Contains(w.Body.String(), `"data":[]`) {
		t.Fatal(w.Body.String())
	}
	// A different node's result cannot be attached to this node's command.
	if _, err = pool.Exec(ctx, `INSERT INTO agent_commands(id,node_id,type) VALUES ('00000000-0000-4000-8000-000000000102',$1,'run_now')`, foreign); err != nil {
		t.Fatal(err)
	}
	w := ingestRequest(t, handler, foreignToken, "POST", "/api/agent/commands/00000000-0000-4000-8000-000000000102/ack", `{"status":"succeeded","resultId":"00000000-0000-4000-8000-000000000001"}`)
	if w.Code != 422 {
		t.Fatal(w.Code, w.Body.String())
	}
	ip := ingestRequest(t, handler, token, "GET", "/api/agent/ip", "")
	if ip.Code != 200 || !strings.Contains(ip.Body.String(), "198.51.100.10") || strings.Contains(ip.Body.String(), "203.0.113.99") {
		t.Fatal(ip.Body.String())
	}
	trusted := NewHandler(Options{NodeCredentials: service, AgentAPI: store, AgentRateLimit: 10000, TrustedProxyCIDRs: "198.51.100.0/24", Logger: slog.New(slog.NewJSONHandler(io.Discard, nil))})
	if ip = ingestRequest(t, trusted, token, "GET", "/api/agent/ip", ""); !strings.Contains(ip.Body.String(), "203.0.113.99") {
		t.Fatal(ip.Body.String())
	}
	if err = service.RevokeCredential(ctx, node); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []struct{ method, path, body string }{{"POST", "/api/agent/results", batch}, {"GET", "/api/agent/config", ""}, {"POST", "/api/agent/heartbeat", `{}`}, {"GET", "/api/agent/commands", ""}, {"POST", path, `{}`}, {"GET", "/api/agent/ip", ""}} {
		if w := ingestRequest(t, handler, token, operation.method, operation.path, operation.body); w.Code != 401 {
			t.Fatal(operation.path, w.Code)
		}
	}
	// The store also persists failed measurements with unavailable metrics intact.
	failedJSON := strings.ReplaceAll(ingestionResult, "00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000005")
	var failedFields map[string]any
	if err = json.Unmarshal([]byte(failedJSON), &failedFields); err != nil {
		t.Fatal(err)
	}
	failedFields["status"] = "failed"
	failedFields["error"] = "engine timed out"
	for _, key := range []string{"down", "up", "ping", "serverId"} {
		failedFields[key] = nil
	}
	failedBytes, err := json.Marshal(failedFields)
	if err != nil {
		t.Fatal(err)
	}
	failedResult, err := agentapi.ParseResult(failedBytes)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, err = store.SaveResult(ctx, node, failedResult); err != nil || duplicate {
		t.Fatal(duplicate, err)
	}
	var missing bool
	if err = pool.QueryRow(ctx, `SELECT down_mbps IS NULL AND up_mbps IS NULL AND ping_ms IS NULL AND server_id IS NULL AND status='failed' AND error='engine timed out' FROM results WHERE id=$1`, failedResult.ID).Scan(&missing); err != nil || !missing {
		t.Fatal("failed result nulls", err)
	}
	// Bounded polling has a deterministic UUID tie-breaker even with equal times.
	if _, err = pool.Exec(ctx, `INSERT INTO agent_commands(id,node_id,type,created_at) SELECT ('00000000-0000-4000-8000-' || lpad(i::text,12,'0'))::uuid,$1,'run_now','2026-11-03T00:00:00Z' FROM generate_series(1000,1100) i`, node); err != nil {
		t.Fatal(err)
	}
	pending, err := store.Commands(ctx, node)
	if err != nil || len(pending) != 100 || pending[0].ID != "00000000-0000-4000-8000-000000001000" || pending[99].ID != "00000000-0000-4000-8000-000000001099" {
		t.Fatal("poll limit/order", len(pending), err)
	}
	failedAck, err := store.Ack(ctx, node, pending[0].ID, agentapi.Ack{Status: "failed", Error: func() *string { message := "safe engine failure"; return &message }()})
	if err != nil || failedAck.AckedAt == nil || failedAck.Error == nil {
		t.Fatal("failed acknowledgement", err)
	}
	repeated, err := store.Ack(ctx, node, pending[0].ID, agentapi.Ack{Status: "failed", Error: failedAck.Error})
	if err != nil || repeated.AckedAt == nil || !repeated.AckedAt.Equal(*failedAck.AckedAt) {
		t.Fatal("terminal timestamp changed", err)
	}

	t.Log("PASS: batch partial failure; all fields/raw persisted; identical, changed and cross-node replays; concurrent UUID; transactional hook rollback; config ETag; heartbeat; command ack ownership/progress/idempotency; IP proxy trust; immediate revocation on all six operations")
}

func TestServerDiscoveryCommandStoresCatalogWithoutDroppingPinIntegration(t *testing.T) {
	pool, ctx, appKey := openServerIntegrationPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	service := nodes.NewService(pool, appKey)
	node, token := enrollIngestionNode(t, ctx, service, "server-discovery")
	if _, err := pool.Exec(ctx, `UPDATE nodes SET installed_engines=ARRAY['ookla','librespeed']::text[] WHERE id=$1`, node); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO engine_schedules(node_id,engine,interval_minutes,enabled) VALUES($1,'librespeed',60,true)`, node); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO node_engine_servers(node_id,engine,servers,pinned_server_id) VALUES($1,'ookla','[]',54321)`, node); err != nil {
		t.Fatal(err)
	}
	store := &agentapi.Postgres{Pool: pool}
	handler := NewHandler(Options{NodeCredentials: service, AgentAPI: store, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil))})
	commandID := "00000000-0000-4000-8000-000000000176"
	if _, err := pool.Exec(ctx, `INSERT INTO agent_commands(id,node_id,type,engine) VALUES($1,$2,'list_servers','ookla')`, commandID, node); err != nil {
		t.Fatal(err)
	}
	polled := ingestRequest(t, handler, token, http.MethodGet, "/api/agent/commands", "")
	if polled.Code != http.StatusOK || !strings.Contains(polled.Body.String(), `"engine":"ookla"`) {
		t.Fatalf("agent command poll=%d %s", polled.Code, polled.Body.String())
	}
	ack := `{"status":"succeeded","phase":"done","pct":100,"servers":[{"id":54321,"name":"Hanoi","city":"Hanoi","isp":"Fiber","host":"hn.example","distanceKm":22.8}]}`
	acknowledged := ingestRequest(t, handler, token, http.MethodPost, "/api/agent/commands/"+commandID+"/ack", ack)
	if acknowledged.Code != http.StatusOK {
		t.Fatalf("server discovery acknowledgement=%d %s", acknowledged.Code, acknowledged.Body.String())
	}
	var servers []byte
	var pin int64
	var refreshed time.Time
	if err := pool.QueryRow(ctx, `SELECT servers,pinned_server_id,refreshed_at FROM node_engine_servers WHERE node_id=$1 AND engine='ookla'`, node).Scan(&servers, &pin, &refreshed); err != nil {
		t.Fatalf("stored catalog=%s pin=%d refreshed=%s err=%v", servers, pin, refreshed, err)
	}
	var listed []agentapi.ServerOption
	if err := json.Unmarshal(servers, &listed); err != nil || len(listed) != 1 || listed[0].City != "Hanoi" || pin != 54321 || refreshed.IsZero() {
		t.Fatalf("stored catalog=%s pin=%d refreshed=%s err=%v", servers, pin, refreshed, err)
	}
	refreshID := "00000000-0000-4000-8000-000000000178"
	if _, err := pool.Exec(ctx, `INSERT INTO agent_commands(id,node_id,type,engine) VALUES($1,$2,'list_servers','ookla')`, refreshID, node); err != nil {
		t.Fatal(err)
	}
	refreshAck := `{"status":"succeeded","phase":"done","pct":100,"servers":[{"id":54322,"name":"Hue","city":"Hue","isp":"Fiber","host":"hue.example"}]}`
	if refreshed := ingestRequest(t, handler, token, http.MethodPost, "/api/agent/commands/"+refreshID+"/ack", refreshAck); refreshed.Code != http.StatusOK {
		t.Fatalf("second discovery acknowledgement=%d %s", refreshed.Code, refreshed.Body.String())
	}
	replay := ingestRequest(t, handler, token, http.MethodPost, "/api/agent/commands/"+commandID+"/ack", ack)
	if replay.Code != http.StatusOK {
		t.Fatalf("identical terminal catalog replay=%d %s", replay.Code, replay.Body.String())
	}
	changedReplay := strings.Replace(ack, `"name":"Hanoi"`, `"name":"Changed Hanoi"`, 1)
	if conflict := ingestRequest(t, handler, token, http.MethodPost, "/api/agent/commands/"+commandID+"/ack", changedReplay); conflict.Code != http.StatusConflict {
		t.Fatalf("different terminal catalog replay=%d %s, want 409", conflict.Code, conflict.Body.String())
	}
	if err := pool.QueryRow(ctx, `SELECT servers,pinned_server_id,refreshed_at FROM node_engine_servers WHERE node_id=$1 AND engine='ookla'`, node).Scan(&servers, &pin, &refreshed); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(servers, &listed); err != nil || len(listed) != 1 || listed[0].ID != 54322 || pin != 54321 {
		t.Fatalf("catalog replay replaced later refresh: servers=%s pin=%d err=%v", servers, pin, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agent_commands(id,node_id,type,engine) VALUES('00000000-0000-4000-8000-000000000177',$1,'list_servers','librespeed')`, node); err != nil {
		t.Fatal(err)
	}
	missingList := ingestRequest(t, handler, token, http.MethodPost, "/api/agent/commands/00000000-0000-4000-8000-000000000177/ack", `{"status":"succeeded","pct":100}`)
	if missingList.Code != http.StatusUnprocessableEntity {
		t.Fatalf("success without list=%d %s", missingList.Code, missingList.Body.String())
	}
	t.Log("PASS: discovery command carries engine; success ACK validates and transactionally replaces that engine catalog; existing pin survives; missing catalog payload refused")
}

func TestLibreSpeedFallbackResultResolvesCatalogServerAndKeepsMeasurementPinIntegration(t *testing.T) {
	pool, ctx, key := openServerIntegrationPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	service := nodes.NewService(pool, key)
	node, _ := enrollIngestionNode(t, ctx, service, "libre-fallback")
	if _, err := pool.Exec(ctx, `UPDATE nodes SET installed_engines=ARRAY['ookla','librespeed']::text[] WHERE id=$1`, node); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO node_engine_servers(node_id,engine,servers,pinned_server_id) VALUES($1,'librespeed',
		'[{"id":51,"name":"Hanoi","host":"hn.example.test"},{"id":52,"name":"Singapore","host":"sg.example.test"}]',52)`, node); err != nil {
		t.Fatal(err)
	}
	// The agent may deliver a spooled measurement after the owner clears the live pin.
	if _, err := pool.Exec(ctx, `UPDATE node_engine_servers SET pinned_server_id=NULL WHERE node_id=$1 AND engine='librespeed'`, node); err != nil {
		t.Fatal(err)
	}
	body := strings.ReplaceAll(ingestionResult, "00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000003")
	body = strings.ReplaceAll(body, `"serverId":1`, `"serverId":null`)
	body = strings.ReplaceAll(body, `"server":{"id":1,"name":"edge","city":"city","sponsor":"isp","host":"test.example"}`, `"engine":"librespeed","server":{"name":"Hanoi","city":"Hanoi","sponsor":"Hanoi ISP","host":"hn.example.test"},"serverSelectionKnown":true,"pinnedServerId":52`)
	result, err := agentapi.ParseResult([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, err := (&agentapi.Postgres{Pool: pool}).SaveResult(ctx, node, result); err != nil || duplicate {
		t.Fatalf("late LibreSpeed result duplicate=%v err=%v", duplicate, err)
	}
	var serverID, pinnedID *int64
	var known bool
	var server []byte
	if err := pool.QueryRow(ctx, `SELECT server_id,pinned_server_id_at_test,server_selection_known,server FROM results WHERE id=$1`, result.ID).
		Scan(&serverID, &pinnedID, &known, &server); err != nil {
		t.Fatal(err)
	}
	var actual map[string]any
	if err := json.Unmarshal(server, &actual); err != nil || serverID == nil || *serverID != 51 || actual["id"] != float64(51) || pinnedID == nil || *pinnedID != 52 || !known {
		t.Fatalf("stored fallback identity=%v server=%v pin=%v known=%v err=%v", serverID, actual, pinnedID, known, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE node_engine_servers SET servers=servers || '[{"id":53,"name":"Hanoi duplicate","host":"hn.example.test"}]'::jsonb WHERE node_id=$1 AND engine='librespeed'`, node); err != nil {
		t.Fatal(err)
	}
	ambiguous := result
	ambiguous.ID = "00000000-0000-4000-8000-000000000004"
	if duplicate, err := (&agentapi.Postgres{Pool: pool}).SaveResult(ctx, node, ambiguous); err != nil || duplicate {
		t.Fatalf("ambiguous LibreSpeed result duplicate=%v err=%v", duplicate, err)
	}
	if err := pool.QueryRow(ctx, `SELECT server_id FROM results WHERE id=$1`, ambiguous.ID).Scan(&serverID); err != nil || serverID != nil {
		t.Fatalf("ambiguous host match=%v err=%v, want unknown ID", serverID, err)
	}
	if duplicate, err := (&agentapi.Postgres{Pool: pool}).SaveResult(ctx, node, result); err != nil || !duplicate {
		t.Fatalf("replay after catalog changed duplicate=%v err=%v", duplicate, err)
	}
}

// The paced test measures the production executable, not the race/coverage
// instrumented test harness or the load generator. The service shares this
// suite's verified worktree database and is stopped before fixture cleanup.
func TestAgentIngestionLoad1000PerMinuteIntegration(t *testing.T) {
	pool, ctx, key := openServerIntegrationPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	service := nodes.NewService(pool, key)
	_, token := enrollIngestionNode(t, ctx, service, "load-edge")
	// The executable now wires IP watching. Use a deterministic warm geo cache
	// and disable public fallbacks so the load test never contacts real providers.
	if _, err := pool.Exec(ctx, `INSERT INTO ip_addresses(ip,geo,source,looked_up_at) VALUES ('203.0.113.1','{"city":"fixture"}','fixture',clock_timestamp())`); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPEEDTEST_GEO_IPAPI_ENABLED", "false")
	t.Setenv("SPEEDTEST_GEO_IPINFO_ENABLED", "false")
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(root, ".tmp"), 0700); err != nil {
		t.Fatal(err)
	}
	temp, err := os.MkdirTemp(filepath.Join(root, ".tmp"), "ingestion-load-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(temp) }()
	binary := filepath.Join(temp, "center")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/center")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build center: %v %s", err, output)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	t.Setenv("SPEEDTEST_HTTP_ADDR", addr)
	t.Setenv("APP_ENV", "test")
	t.Setenv("SPEEDTEST_UI_DIR", "")
	t.Setenv("SPEEDTEST_GEO_DBIP_AUTO_UPDATE", "false")
	process := exec.CommandContext(ctx, binary)
	process.Dir = root
	process.Stdout = os.Stdout
	process.Stderr = os.Stderr
	if err = process.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = process.Process.Kill()
			_ = process.Wait()
		}
	}()
	client := &http.Client{Timeout: 10 * time.Second}
	defer client.CloseIdleConnections()
	base := "http://" + addr
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, err := client.Get(base + "/readyz")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("production center did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cpuStarted := centerCPUSeconds(t, process.Process.Pid)
	started := time.Now()
	for batch := range 10 {
		results := make([]string, 100)
		for i := range 100 {
			id := fmt.Sprintf("00000000-0000-4000-8000-%012d", batch*100+i+1000)
			results[i] = strings.Replace(ingestionResult, "00000000-0000-4000-8000-000000000001", id, 1)
		}
		request, err := http.NewRequestWithContext(ctx, "POST", base+"/api/agent/results", strings.NewReader(`{"results":[`+strings.Join(results, ",")+`]}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		w.Code = response.StatusCode
		_, _ = w.Body.Write(data)
		statuses := make([]int, 100)
		for i := range statuses {
			statuses[i] = 201
		}
		outcomes(t, w, statuses...)
		time.Sleep(time.Until(started.Add(time.Duration(batch+1) * 6 * time.Second)))
	}
	elapsed := time.Since(started).Seconds()
	cpu := centerCPUSeconds(t, process.Process.Pid) - cpuStarted
	if err = process.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	err = process.Wait()
	stopped = true
	if err != nil {
		t.Fatal("center failed to stop cleanly", err)
	}
	percent := 100 * cpu / elapsed
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM results`).Scan(&count); err != nil || count != 1000 {
		t.Fatal(count, err)
	}
	t.Logf("1000 committed results / %.3fs; production center CPU %.4fs = %.3f%% of one core (target <=5%%); startup/shutdown, shared PostgreSQL and load-generator CPU excluded", elapsed, cpu, percent)
	// One second allows scheduler/timer overhead, not an unbounded slow run.
	if elapsed > 61 {
		t.Fatalf("1000-result workload exceeded the one-minute pace: %.3fs", elapsed)
	}
	t.Logf("CPU before workload %.4fs; total process lifetime %.4fs (startup/shutdown excluded from gate)", cpuStarted, process.ProcessState.UserTime().Seconds()+process.ProcessState.SystemTime().Seconds())
	if percent > 5 {
		t.Fatalf("CPU %.3f%% exceeds 5%%", percent)
	}
}

// Every measurement an agent reports must be stored as sent; a dropped column would silently
// corrupt statistics, so each one is read back with a distinct non-null value.
func TestAgentIngestionStoresEveryMeasurementIntegration(t *testing.T) {
	pool, ctx, key := openServerIntegrationPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	service := nodes.NewService(pool, key)
	_, token := enrollIngestionNode(t, ctx, service, "ingestion-measures")
	handler := NewHandler(Options{NodeCredentials: service, AgentAPI: &agentapi.Postgres{Pool: pool}, AgentRateLimit: 10000, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil))})
	body := strings.NewReplacer(`"jitter":null`, `"jitter":1.5`, `"dLat":null`, `"dLat":21.25`, `"uLat":null`, `"uLat":31.75`, `"loss":null`, `"loss":0.5`).Replace(ingestionResult)
	outcomes(t, ingestRequest(t, handler, token, "POST", "/api/agent/results", `{"results":[`+body+`]}`), 201)
	var down, up, ping, jitter, dLat, uLat, loss float64
	var threads, bytesDown int
	if err := pool.QueryRow(ctx, `SELECT down_mbps,up_mbps,ping_ms,jitter_ms,down_latency_ms,up_latency_ms,loss_percent,threads,bytes_down FROM results`).
		Scan(&down, &up, &ping, &jitter, &dLat, &uLat, &loss, &threads, &bytesDown); err != nil {
		t.Fatal(err)
	}
	if down != 123.4 || up != 56.7 || ping != 3 || jitter != 1.5 || dLat != 21.25 || uLat != 31.75 || loss != 0.5 || threads != 4 || bytesDown != 1024 {
		t.Fatalf("stored %v %v %v %v %v %v %v %v %v", down, up, ping, jitter, dLat, uLat, loss, threads, bytesDown)
	}
}

// Linux process accounting includes all center threads but excludes unrelated
// test processes. Sampling after readiness and before shutdown isolates the
// paced workload; Rusage at exit also charges startup and background teardown.
func centerCPUSeconds(t *testing.T, pid int) float64 {
	t.Helper()
	ticksOutput, err := exec.Command("getconf", "CLK_TCK").Output()
	if err != nil {
		t.Fatal("read kernel clock tick rate", err)
	}
	ticks, err := strconv.ParseFloat(strings.TrimSpace(string(ticksOutput)), 64)
	if err != nil || ticks <= 0 {
		t.Fatal("invalid kernel clock tick rate")
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal("read center process accounting", err)
	}
	// comm is parenthesized and may contain spaces; fields after it start at 3.
	_, rest, ok := strings.Cut(string(data), ") ")
	fields := strings.Fields(rest)
	if !ok || len(fields) < 13 {
		t.Fatal("invalid center process accounting")
	}
	user, err := strconv.ParseFloat(fields[11], 64)
	if err != nil {
		t.Fatal("invalid center user CPU")
	}
	system, err := strconv.ParseFloat(fields[12], 64)
	if err != nil {
		t.Fatal("invalid center system CPU")
	}
	return (user + system) / ticks
}
