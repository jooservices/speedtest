//go:build integration

package catalog

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/shared/testenv"

	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/shared/config"
)

func TestMigratedDatabaseCatalogDrift(t *testing.T) {
	values, err := godotenv.Read(filepath.Join(root, ".env"))
	if err != nil {
		t.Fatal("worktree .env required")
	}
	testenv.Apply(t, values)
	cfg, err := config.LoadCenter()
	if err != nil {
		t.Fatal("invalid database configuration")
	}
	if err := db.CheckTestDatabase(cfg.Database.Name); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal("cannot open worktree database")
	}
	defer pool.Close()
	// Reset verifies the connected database name and ownership marker first.
	dbtest.Reset(t, pool)
	defer dbtest.Reset(t, pool)
	if err := db.Migrate(ctx, pool, "up"); err != nil {
		t.Fatal(err)
	}
	channels, metrics, operators, _, _ := contractNames()
	for _, tc := range []struct {
		table, column string
		want          []string
	}{
		{"alert_rules", "metric", metrics}, {"alert_rules", "op", operators},
		{"alert_rules", "channels", channels}, {"notification_deliveries", "channel", channels},
	} {
		t.Run(tc.table+"/"+tc.column, func(t *testing.T) {
			rows, err := pool.Query(context.Background(), `SELECT pg_get_constraintdef(c.oid)
    FROM pg_constraint c JOIN pg_class r ON r.oid=c.conrelid
    JOIN pg_namespace n ON n.oid=r.relnamespace
    WHERE n.nspname='public' AND r.relname=$1 AND c.contype='c'`, tc.table)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var names []string
			for rows.Next() {
				var definition string
				if err := rows.Scan(&definition); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(definition, tc.column+" ") || strings.Contains(definition, tc.column+")") {
					names = append(names, captures(definition, `'([^']+)'`)...)
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			equalNames(t, "effective CHECK values", names, tc.want)
		})
	}
}
