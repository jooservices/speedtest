//go:build integration

package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/shared/config"
)

func TestRunMigration(t *testing.T) {
	root, err := filepath.Abs("../../../../..")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	values, err := godotenv.Read(".env")
	if err != nil {
		t.Fatal("worktree .env required")
	}
	for k, v := range values {
		t.Setenv(k, v)
	}
	cfg, err := config.LoadCenter()
	if err != nil {
		t.Fatal("invalid test configuration")
	}
	if cfg.Database.Name != "speedtest_wt10" && cfg.Database.Name != "speedtest_test" {
		t.Fatal("refusing non-test database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var name, marker string
	err = pool.QueryRow(ctx, "SELECT current_database(),shobj_description(oid,'pg_database') FROM pg_database WHERE datname=current_database()").Scan(&name, &marker)
	if err != nil || name != cfg.Database.Name || marker != "workspaces-project:speedtest" {
		t.Fatal("database safety check failed")
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err = conn.Exec(ctx, "SELECT pg_advisory_lock(1000010)"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock(1000010)") }()
	var n int
	if e := pool.QueryRow(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_type='BASE TABLE' AND table_name <> 'goose_db_version'").Scan(&n); e != nil || n != 0 {
		t.Fatal("CLI test requires empty schema")
	}
	for _, direction := range []string{"up", "up", "down"} {
		var out bytes.Buffer
		if code := run([]string{direction}, &out); code != 0 || !strings.Contains(out.String(), "complete") {
			t.Fatalf("%s: code %d: %s", direction, code, out.String())
		}
	}
	var out bytes.Buffer
	// At version zero down must fail, with a safe error, rather than claim success.
	if code := run([]string{"down"}, &out); code != 1 || !strings.Contains(out.String(), "migration failed") {
		t.Fatalf("expected no-version error: %d %s", code, out.String())
	}
	t.Setenv("SPEEDTEST_DB_PORT", "1")
	out.Reset()
	if code := run([]string{"up"}, &out); code != 1 || !strings.Contains(out.String(), "cannot connect") {
		t.Fatal("expected connection error")
	}
	t.Log("PASS: CLI up, repeated up, down, empty down and safe connection error")
}
