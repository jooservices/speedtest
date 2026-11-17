//go:build integration

package agentapi

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/shared/config"
)

const legacyFailureInsertQuery = `INSERT INTO results (
	id, node_id, ts, status, down_mbps, up_mbps, ping_ms, jitter_ms, down_latency_ms, up_latency_ms,
	loss_percent, server_id, ip, threads, bytes_down, bytes_up, duration_seconds, error, manual, raw,
	loaded_down, loaded_up, server, engine_isp, engine_result_id, engine_result_url, engine_ip, warnings, engine
) VALUES (
	$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15,
	$16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29
)`

func openAgentAPIIntegrationDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	values, err := godotenv.Read(filepath.Join("..", "..", "..", ".env"))
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
	if err := db.CheckDestructiveAllowed(cfg.AppEnv); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal("cannot connect to configured worktree database")
	}
	t.Cleanup(pool.Close)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	return pool, ctx
}

func TestLegacyFailureReplayAfterMigrationAndOldBinaryWrite(t *testing.T) {
	pool, ctx := openAgentAPIIntegrationDB(t)
	const node = "00000000-0000-4000-8000-000000000073"
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES ($1,'Replay fixture',100,50)`, node); err != nil {
		t.Fatal("could not create replay node")
	}
	store := &Postgres{Pool: pool}
	for _, tc := range []struct {
		name      string
		id        string
		oldBinary bool
	}{
		{name: "row backfilled by migration", id: "00000000-0000-4000-8000-000000000074"},
		{name: "old binary writes after migration", id: "00000000-0000-4000-8000-000000000075", oldBinary: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := failedReplayPayload(tc.id, "speedtest: timeout")
			result, err := ParseResult(payload)
			if err != nil || !result.classOmitted || result.FailureClass != "timeout" {
				t.Fatalf("parsed legacy result = %+v, %v", result, err)
			}
			args := result.values(node)
			if tc.oldBinary {
				if _, err := pool.Exec(ctx, legacyFailureInsertQuery, args[:29]...); err != nil {
					t.Fatal("could not simulate old-binary insert")
				}
			} else {
				args[len(args)-1] = "other"
				if _, err := pool.Exec(ctx, resultInsertQuery, args...); err != nil {
					t.Fatal("could not create migrated legacy row")
				}
			}
			duplicate, err := store.SaveResult(ctx, node, result)
			if err != nil || !duplicate {
				t.Fatalf("unchanged legacy replay = duplicate %t, error %v", duplicate, err)
			}
			changed, err := ParseResult(failedReplayPayload(tc.id, "speedtest: no_network"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.SaveResult(ctx, node, changed); err != ErrConflict {
				t.Fatalf("changed legacy payload error = %v, want conflict", err)
			}
		})
	}
}

func failedReplayPayload(id, message string) []byte {
	data := change(validResult, "id", id)
	data = change(string(data), "status", "failed")
	data = change(string(data), "error", message)
	for _, key := range []string{"down", "up", "ping", "jitter", "dLat", "uLat", "loss", "serverId", "ip", "server"} {
		data = change(string(data), key, nil)
	}
	return data
}
