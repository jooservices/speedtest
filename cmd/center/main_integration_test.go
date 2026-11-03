//go:build integration

package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/joho/godotenv"
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

	address := freeTCPAddress(t)
	t.Setenv("SPEEDTEST_HTTP_ADDR", address)
	t.Setenv("SPEEDTEST_UI_DIR", "")
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	runCtx, stop := context.WithCancel(t.Context())
	runDone := make(chan error, 1)
	go func() { runDone <- runWithDatabase(runCtx, nil, logger, io.Discard) }()
	client := &http.Client{Timeout: time.Second}
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
	if err != nil || db.CheckTestDatabase(cfg.Database.Name) != nil {
		t.Fatal("worktree database required")
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
