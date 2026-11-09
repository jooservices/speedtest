//go:build integration

package audit

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/shared/testenv"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/shared/config"
)

func openPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	values, err := godotenv.Read(filepath.Join("..", "..", "..", ".env"))
	if err != nil {
		t.Fatal("a configured worktree .env is required")
	}
	testenv.Apply(t, values)
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

func TestStoreRecordsAndListsNewestFirst(t *testing.T) {
	pool, ctx := openPool(t)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	clock := now
	s := &Store{DB: pool, Now: func() time.Time { return clock }}
	for i, action := range []string{"node.create", "node.update", "settings.update", "alert.rule.create"} {
		clock = now.Add(time.Duration(i) * time.Minute)
		if err := s.Record(ctx, Entry{Actor: "owner@example.com", Action: action, TargetType: "x", Summary: strings.Repeat("s", 400), Remote: "203.0.113.5"}); err != nil {
			t.Fatal(err)
		}
	}
	items, total, err := s.List(ctx, Filter{}, 1, 3)
	if err != nil || total != 4 || len(items) != 3 {
		t.Fatalf("page 1: %d items of %d, err %v", len(items), total, err)
	}
	if items[0].Action != "alert.rule.create" || items[2].Action != "node.update" {
		t.Fatalf("newest first: %+v", items)
	}
	if len(items[0].Summary) != 300 {
		t.Fatalf("summary is cut to 300 characters, got %d", len(items[0].Summary))
	}
	if !items[0].TS.Equal(now.Add(3*time.Minute)) || items[0].TS.Location() != time.UTC {
		t.Fatalf("timestamps are UTC: %v", items[0].TS)
	}
	items, total, err = s.List(ctx, Filter{}, 2, 3)
	if err != nil || total != 4 || len(items) != 1 || items[0].Action != "node.create" {
		t.Fatalf("page 2: %+v total %d err %v", items, total, err)
	}
	items, total, _ = s.List(ctx, Filter{Action: "node."}, 1, 10)
	if total != 2 || len(items) != 2 {
		t.Fatalf("prefix filter: %d items", total)
	}
	// LIKE wildcards in the filter are literal.
	if _, total, _ := s.List(ctx, Filter{Action: "%"}, 1, 10); total != 0 {
		t.Fatalf("a percent sign must not match everything: %d", total)
	}
	if items, total, _ := s.List(ctx, Filter{Action: "nothing"}, 1, 10); total != 0 || items == nil || len(items) != 0 {
		t.Fatalf("empty result is an empty list, not null: %+v", items)
	}
}
