//go:build integration

package auth_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"

	"github.com/jooservices/speedtest/internal/center/auth"
	"github.com/jooservices/speedtest/internal/center/db"
)

func TestTwoFactorPostgresStore(t *testing.T) {
	pool, ctx := integrationPool(t)
	if err := db.Migrate(ctx, pool, "up"); err != nil {
		t.Fatal(err)
	}
	store, err := auth.NewPostgresStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	email := fmt.Sprintf("joost77-%s@example.test", hex.EncodeToString(suffix[:]))
	hash, err := auth.HashPassword("integration owner password")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SeedAdmin(ctx, email, hash); err != nil {
		t.Fatal(err)
	}
	user, err := store.FindUserByEmail(ctx, email)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, user.ID) })

	if _, err := store.GetTwoFactor(ctx, user.ID); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("before setup: %v", err)
	}
	if err := store.EnableTwoFactor(ctx, user.ID, 5, []string{"a"}); !errors.Is(err, auth.ErrTwoFactorNotPending) {
		t.Fatalf("enable without setup: %v", err)
	}
	if err := store.SaveTwoFactorSetup(ctx, user.ID, []byte("sealed-1")); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTwoFactorSetup(ctx, user.ID, []byte("sealed-2")); err != nil {
		t.Fatalf("a pending setup is replaced: %v", err)
	}
	got, err := store.GetTwoFactor(ctx, user.ID)
	if err != nil || string(got.SecretEnc) != "sealed-2" || got.Enabled || got.LastStep != 0 || len(got.RecoveryHashes) != 0 {
		t.Fatalf("pending = %+v, %v", got, err)
	}
	if used, err := store.ConsumeTwoFactorStep(ctx, user.ID, 10); err != nil || used {
		t.Fatalf("a pending setup must not accept sign-in steps: %v %v", used, err)
	}
	if err := store.EnableTwoFactor(ctx, user.ID, 7, []string{"h1", "h2", "h3"}); err != nil {
		t.Fatal(err)
	}
	if err := store.EnableTwoFactor(ctx, user.ID, 8, []string{"x"}); !errors.Is(err, auth.ErrTwoFactorNotPending) {
		t.Fatalf("enable twice: %v", err)
	}
	// Saving a new setup never overwrites an enabled secret.
	if err := store.SaveTwoFactorSetup(ctx, user.ID, []byte("sealed-3")); err != nil {
		t.Fatal(err)
	}
	got, _ = store.GetTwoFactor(ctx, user.ID)
	if string(got.SecretEnc) != "sealed-2" || !got.Enabled || got.LastStep != 7 || len(got.RecoveryHashes) != 3 {
		t.Fatalf("enabled = %+v", got)
	}
	// Steps only move forward: a replay and an older step are refused.
	for _, c := range []struct {
		step int64
		want bool
	}{{9, true}, {9, false}, {8, false}, {10, true}} {
		step, want := c.step, c.want
		if used, err := store.ConsumeTwoFactorStep(ctx, user.ID, step); err != nil || used != want {
			t.Fatalf("step %d = %v %v, want %v", step, used, err, want)
		}
	}
	if used, err := store.ConsumeRecoveryCode(ctx, user.ID, "h2"); err != nil || !used {
		t.Fatalf("recovery code: %v %v", used, err)
	}
	if used, err := store.ConsumeRecoveryCode(ctx, user.ID, "h2"); err != nil || used {
		t.Fatalf("recovery code reused: %v %v", used, err)
	}
	got, _ = store.GetTwoFactor(ctx, user.ID)
	if len(got.RecoveryHashes) != 2 {
		t.Fatalf("recovery left = %v", got.RecoveryHashes)
	}
	if err := store.DeleteTwoFactor(ctx, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetTwoFactor(ctx, user.ID); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	// Deleting the account removes its second factor.
	if err := store.SaveTwoFactorSetup(ctx, user.ID, []byte("sealed-4")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, user.ID); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_two_factor WHERE user_id=$1`, user.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("orphan rows = %d %v", rows, err)
	}
}
