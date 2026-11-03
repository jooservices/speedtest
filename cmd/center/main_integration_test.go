//go:build integration

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/agentapi"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/shared/config"
)

func TestRunConnectsDatabaseAndWiresNodeAPIIntegration(t *testing.T) {
	values, err := godotenv.Read("../../.env")
	if err != nil {
		t.Fatal("a configured worktree .env is required")
	}
	for key, value := range values {
		t.Setenv(key, value)
	}
	cfg, err := config.LoadCenter()
	if err != nil {
		t.Fatal("invalid worktree test configuration")
	}
	if err := db.CheckTestDatabase(cfg.Database.Name); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var name, marker string
	if err := pool.QueryRow(ctx, `SELECT current_database(),shobj_description(oid,'pg_database') FROM pg_database WHERE datname=current_database()`).Scan(&name, &marker); err != nil || name != cfg.Database.Name || marker != "workspaces-project:speedtest" {
		t.Fatal("integration database identity and ownership marker verification failed")
	}
	t.Log("PASS: command-center startup is checked against the verified Speedtest worktree database")

	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	var locationCalls atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		locationCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "query": strings.TrimPrefix(r.URL.Path, "/"), "countryCode": "ZZ", "city": "Fixture city", "lat": 1, "lon": 2})
	}))
	t.Cleanup(provider.Close)
	t.Setenv("SPEEDTEST_GEO_IPAPI_ENABLED", "true")
	t.Setenv("SPEEDTEST_GEO_IPAPI_URL", provider.URL)
	t.Setenv("SPEEDTEST_GEO_IPINFO_ENABLED", "false")
	t.Setenv("SPEEDTEST_GEOIP_CITY_DB", t.TempDir()+"/missing-city.mmdb")
	t.Setenv("SPEEDTEST_GEOIP_ASN_DB", t.TempDir()+"/missing-asn.mmdb")
	address := freeTCPAddress(t)
	t.Setenv("SPEEDTEST_HTTP_ADDR", address)
	t.Setenv("SPEEDTEST_UI_DIR", "")
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	runCtx, stop := context.WithCancel(t.Context())
	t.Cleanup(stop)
	runDone := make(chan error, 1)
	go func() { runDone <- runWithDatabase(runCtx, nil, logger, io.Discard) }()
	client := &http.Client{Timeout: 5 * time.Second}
	baseURL := "http://" + address
	deadline := time.Now().Add(3 * time.Second)
	for {
		response, requestErr := client.Get(baseURL + "/readyz")
		if requestErr == nil {
			if response.StatusCode != http.StatusOK {
				_ = response.Body.Close()
				stop()
				t.Fatalf("GET /readyz status = %d, want 200", response.StatusCode)
			}
			_ = response.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			stop()
			t.Fatalf("database-backed command center did not start: %v", requestErr)
		}
		select {
		case runErr := <-runDone:
			stop()
			t.Fatalf("run() returned before serving: %v", runErr)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	request, err := http.NewRequest(http.MethodPost, baseURL+"/api/agent/enroll", strings.NewReader("{"))
	if err != nil {
		stop()
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		stop()
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		stop()
		t.Fatalf("POST /api/agent/enroll status = %d, want 400 from configured API service", response.StatusCode)
	}
	verifyIPHistoryHTTP(t, ctx, pool, client, baseURL)
	if locationCalls.Load() != 2 {
		t.Fatal("expected one fallback lookup for each new public IP", locationCalls.Load())
	}
	client.CloseIdleConnections()
	stop()
	select {
	case runErr := <-runDone:
		if runErr != nil {
			t.Fatalf("run() after cancellation error = %v", runErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("database-backed command center did not stop after cancellation")
	}
	t.Log("PASS: database readiness and node API service wiring are active in the executable")
}

func TestSeedAndStartupGuardsIntegration(t *testing.T) {
	values, err := godotenv.Read("../../.env")
	if err != nil {
		t.Fatal("worktree configuration required")
	}
	for key, value := range values {
		t.Setenv(key, value)
	}
	cfg, err := config.LoadCenter()
	if err != nil {
		t.Fatal("invalid worktree test configuration")
	}
	if err := db.CheckTestDatabase(cfg.Database.Name); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Open(t.Context(), cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	t.Setenv("APP_ENV", "test")
	if err = runSeedAdmin(t.Context(), nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err = runSeedAdmin(t.Context(), nil, io.Discard); err != nil {
		t.Fatal("seed not idempotent", err)
	}
	if err = runSeedAdmin(t.Context(), []string{"unexpected"}, io.Discard); err == nil {
		t.Fatal("seed args accepted")
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	for _, args := range [][]string{{"--unknown"}, {"unexpected"}, {"--ui-dir", "does-not-exist"}} {
		if err = runWithDatabase(t.Context(), args, logger, io.Discard); err == nil {
			t.Fatal("invalid args accepted", args)
		}
	}
	t.Setenv("SPEEDTEST_GEO_PROVIDER_TIMEOUT", "invalid")
	if err = runWithDatabase(t.Context(), nil, logger, io.Discard); err == nil {
		t.Fatal("invalid geo configuration accepted")
	}
	t.Setenv("SPEEDTEST_GEO_PROVIDER_TIMEOUT", "5s")
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err = runWithDatabase(canceled, nil, logger, io.Discard); err == nil {
		t.Fatal("canceled startup succeeded")
	}
	if err = runSeedAdmin(canceled, nil, io.Discard); err == nil {
		t.Fatal("canceled seed succeeded")
	}
	if err = verifyLocalDatabaseIdentity(canceled, pool, cfg); err == nil {
		t.Fatal("canceled identity check succeeded")
	}
	cfg.AppEnv = "production"
	if err = verifyLocalDatabaseIdentity(canceled, pool, cfg); err != nil {
		t.Fatal("production identity path", err)
	}
	t.Setenv("APP_ENV", "production")
	if err = runSeedAdmin(t.Context(), nil, io.Discard); err == nil {
		t.Fatal("production seed allowed")
	}
	t.Setenv("APP_ENV", "test")
	t.Setenv("SPEEDTEST_DB_NAME", "speedtest")
	if err = runSeedAdmin(t.Context(), nil, io.Discard); err == nil {
		t.Fatal("live seed allowed")
	}
	t.Setenv("SPEEDTEST_DB_PORT", "invalid")
	if err = runSeedAdmin(t.Context(), nil, io.Discard); err == nil {
		t.Fatal("invalid config accepted")
	}
}

// verifyIPHistoryHTTP exercises the real executable wiring, authentication,
// batch replay contract, transaction hook and configured geolocation provider.
func verifyIPHistoryHTTP(t *testing.T, ctx context.Context, pool *pgxpool.Pool, client *http.Client, baseURL string) {
	t.Helper()
	const node = "00000000-0000-4000-8000-000000000035"
	const bearer = "sp_fixture_public_test_credential"
	hash := sha256.Sum256([]byte(bearer))
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES ($1,'fixture',100,50)`, node); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO node_credentials(id,node_id,kind,token_hash) VALUES ($1,$1,'bearer',$2)`, node, hex.EncodeToString(hash[:])); err != nil {
		t.Fatal(err)
	}
	body := `{"results":[{"id":"10000000-0000-4000-8000-000000000001","ts":"2026-11-03T10:00:00Z","status":"failed","error":"offline","down":null,"up":null,"ping":null,"jitter":null,"loss":null,"dLat":null,"uLat":null,"serverId":null,"ip":"203.0.113.1","threads":1,"bytesDown":0,"bytesUp":0,"duration":0,"manual":false,"raw":{}}]}`
	post := func(payload, token string, wantStatus int, wantOutcome ...int) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/agent/results", strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != wantStatus {
			data, _ := io.ReadAll(response.Body)
			t.Fatalf("ingestion status=%d want=%d body=%s", response.StatusCode, wantStatus, data)
		}
		if len(wantOutcome) > 0 {
			var wire struct{ Data []agentapi.Outcome }
			if err = json.NewDecoder(response.Body).Decode(&wire); err != nil || len(wire.Data) != len(wantOutcome) {
				t.Fatal("batch outcomes", err, wire)
			}
			for i, status := range wantOutcome {
				if wire.Data[i].Status != status {
					t.Fatal(wire.Data)
				}
			}
		}
	}
	post(body, "invalid", 401)
	post(body, bearer, 207, 201)
	newest := strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(body, "000000000001", "000000000003"), "10:00:00Z", "10:10:00Z"), "203.0.113.1", "203.0.113.2")
	post(newest, bearer, 207, 201)
	late := strings.ReplaceAll(strings.ReplaceAll(body, "000000000001", "000000000002"), "10:00:00Z", "10:05:00Z")
	post(late, bearer, 207, 201)
	post(newest, bearer, 207, 200)
	var tests, history, signals, locations int
	if err := pool.QueryRow(ctx, `SELECT (SELECT sum(tests) FROM node_ip_addresses),(SELECT count(*) FROM node_ip_history),(SELECT count(*) FROM ip_change_signals),(SELECT count(*) FROM ip_addresses WHERE looked_up_at IS NOT NULL)`).Scan(&tests, &history, &signals, &locations); err != nil || tests != 3 || history != 2 || signals != 1 || locations != 2 {
		t.Fatal("HTTP IP watch persistence", tests, history, signals, locations, err)
	}
	t.Log("PASS: executable HTTP ingestion rejects unauthorized bearer; 201/200 replay outcomes; 3 samples, 2 history openings/transitions, 1 newest-change signal, 2 cached locations; late sample emits no alert")
}
