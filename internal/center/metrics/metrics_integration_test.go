//go:build integration

package metrics_test

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/metrics"
	"github.com/jooservices/speedtest/internal/shared/config"
)

func openPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	values, err := godotenv.Read(filepath.Join("..", "..", "..", ".env"))
	if err != nil {
		t.Fatal("a configured worktree .env is required")
	}
	for k, v := range values {
		t.Setenv(k, v)
	}
	cfg, err := config.LoadCenter()
	if err != nil {
		t.Fatal("invalid test configuration")
	}
	if err := db.CheckTestDatabase(cfg.Database.Name); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	return pool, ctx
}

func TestCollectAgainstSchema(t *testing.T) {
	pool, ctx := openPool(t)
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES
	 ('00000000-0000-4000-8000-0000000000b1','n1',100,50)`); err != nil {
		t.Fatal(err)
	}
	s, err := metrics.Collector{DB: pool}.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.NodesEnabled != 1 || s.NodesOffline != 1 || s.LastResultAgeSeconds != -1 {
		t.Fatalf("%+v", s)
	}
	var b bytes.Buffer
	s.Write(&b)
	if !strings.Contains(b.String(), "speedtest_nodes_offline 1") {
		t.Fatal(b.String())
	}
}
