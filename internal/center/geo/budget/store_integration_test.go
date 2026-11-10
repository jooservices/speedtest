//go:build integration

package budget

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	centerdb "github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/shared/config"
)

func budgetIntegrationPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	values, err := godotenv.Read(filepath.Join("../../../../", ".env"))
	if err != nil {
		t.Fatal("a configured worktree .env is required")
	}
	for key, value := range values {
		t.Setenv(key, value)
	}
	cfg, err := config.LoadCenter()
	if err != nil {
		t.Fatal("invalid test database configuration")
	}
	if err := centerdb.CheckTestDatabase(cfg.Database.Name); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	pool, err := centerdb.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var name, marker string
	if err := pool.QueryRow(ctx, "SELECT current_database(), shobj_description(oid,'pg_database') FROM pg_database WHERE datname=current_database()").Scan(&name, &marker); err != nil || name != cfg.Database.Name || marker != "workspaces-project:speedtest" {
		t.Fatal("test database identity/ownership verification failed")
	}
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	return pool, ctx
}

func TestPostgresStoreDailyCountsIntegration(t *testing.T) {
	pool, ctx := budgetIntegrationPool(t)
	store := NewPostgresStore(pool)
	today := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	tomorrow := today.AddDate(0, 0, 1)

	t.Run("UsedIsZeroWithoutRow", func(t *testing.T) {
		got, err := store.Used(ctx, "ipapi", today)
		if err != nil || got != 0 {
			t.Fatalf("Used without row = (%d, %v), want (0, nil)", got, err)
		}
	})
	t.Run("AddAccumulatesPerProviderAndDay", func(t *testing.T) {
		if total, err := store.Add(ctx, "ipapi", today, 2); err != nil || total != 2 {
			t.Fatalf("first Add = (%d, %v), want (2, nil)", total, err)
		}
		if total, err := store.Add(ctx, "ipapi", today, 3); err != nil || total != 5 {
			t.Fatalf("second Add = (%d, %v), want (5, nil)", total, err)
		}
		if got, err := store.Used(ctx, "ipapi", today); err != nil || got != 5 {
			t.Fatalf("Used = (%d, %v), want (5, nil)", got, err)
		}
	})
	t.Run("DaysAndProvidersAreIndependent", func(t *testing.T) {
		if _, err := store.Add(ctx, "ipapi", tomorrow, 1); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Add(ctx, "geolite", today, 4); err != nil {
			t.Fatal(err)
		}
		if got, err := store.Used(ctx, "ipapi", today); err != nil || got != 5 {
			t.Fatalf("ipapi today = (%d, %v), want (5, nil)", got, err)
		}
		if got, err := store.Used(ctx, "ipapi", tomorrow); err != nil || got != 1 {
			t.Fatalf("ipapi tomorrow = (%d, %v), want (1, nil)", got, err)
		}
		if got, err := store.Used(ctx, "geolite", today); err != nil || got != 4 {
			t.Fatalf("geolite today = (%d, %v), want (4, nil)", got, err)
		}
	})
	t.Run("AtomicLimitsAndCooldownSurviveDateRollover", func(t *testing.T) {
		start := time.Date(2026, 11, 9, 23, 59, 0, 0, time.UTC)
		minute := start.Truncate(time.Minute)
		var wg sync.WaitGroup
		var allowed atomic.Int32
		for range 40 {
			wg.Go(func() {
				ok, err := store.Reserve(ctx, "limited", today, minute, start, 20, 5)
				if err != nil {
					t.Error(err)
				} else if ok {
					allowed.Add(1)
				}
			})
		}
		wg.Wait()
		if got := allowed.Load(); got != 5 {
			t.Fatalf("allowed %d concurrent reservations, want 5", got)
		}
		if err := store.BlockUntil(ctx, "limited", start, start.Add(2*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if ok, err := store.Reserve(ctx, "limited", tomorrow, start.Add(time.Minute), start.Add(time.Minute), 20, 5); err != nil || ok {
			t.Fatalf("cooldown across day boundary = (%v,%v), want (false,nil)", ok, err)
		}
		if ok, err := store.Reserve(ctx, "limited", tomorrow, start.Add(2*time.Minute), start.Add(2*time.Minute), 20, 5); err != nil || !ok {
			t.Fatalf("expired cooldown = (%v,%v), want (true,nil)", ok, err)
		}
	})
}
