package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jooservices/speedtest/internal/center/auth"
	"github.com/jooservices/speedtest/internal/shared/config"
)

// runSeedAdmin creates the documented default administrator. It exists for local and test environments only.
func runSeedAdmin(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) != 0 {
		return errors.New("usage: center seed-admin")
	}
	pool, cfg, err := openCenterDatabase(ctx, func(config.CenterConfig) error {
		appEnv, configured := os.LookupEnv("APP_ENV")
		return validateExplicitDefaultSeedEnvironment(appEnv, configured)
	})
	if err != nil {
		return err
	}
	defer pool.Close()
	store, err := auth.NewPostgresStore(pool)
	if err != nil {
		return err
	}
	service, err := newAuthService(store, cfg)
	if err != nil {
		return err
	}
	if err := service.EnsureDefaultAdmin(ctx); err != nil {
		return errors.New("could not seed the default administrator")
	}
	_, _ = fmt.Fprintln(stdout, "default administrator ensured")
	return nil
}

// readNewPassword reads the new password from the first line of input, so it never appears in the process list,
// shell history or logs, and returns its hash. The default password and anything the policy refuses are rejected.
func readNewPassword(input io.Reader) (string, error) {
	line, _ := bufio.NewReader(io.LimitReader(input, 4096)).ReadString('\n')
	password := strings.TrimRight(line, "\r\n")
	if password == auth.DefaultAdminPassword {
		return "", errors.New("the default password is not allowed")
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return "", errors.New("the password does not meet the password policy (read it from the first line of standard input)")
	}
	return hash, nil
}

// ownerEmail returns SPEEDTEST_OWNER_EMAIL lower-cased and trimmed.
func ownerEmail() string {
	return strings.ToLower(strings.TrimSpace(os.Getenv("SPEEDTEST_OWNER_EMAIL")))
}

// runCreateOwner creates the single owner account of a production install. The email comes from
// SPEEDTEST_OWNER_EMAIL and the password from standard input. It refuses to run when a user already exists.
func runCreateOwner(ctx context.Context, args []string, input io.Reader, stdout io.Writer) error {
	if len(args) != 0 {
		return errors.New("usage: SPEEDTEST_OWNER_EMAIL=you@example.com center create-owner < password-file")
	}
	email := ownerEmail()
	if email == "" || !strings.Contains(email, "@") || strings.ContainsAny(email, " \t\r\n") {
		return errors.New("SPEEDTEST_OWNER_EMAIL must be a valid email address")
	}
	hash, err := readNewPassword(input)
	if err != nil {
		return err
	}
	pool, _, err := openCenterDatabase(ctx, nil)
	if err != nil {
		return err
	}
	defer pool.Close()
	var users int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&users); err != nil {
		return errors.New("could not check for existing users (are migrations applied?)")
	}
	if users > 0 {
		return errors.New("an owner account already exists; nothing was changed")
	}
	store, err := auth.NewPostgresStore(pool)
	if err != nil {
		return err
	}
	if err := store.SeedAdmin(ctx, email, hash); err != nil {
		return errors.New("could not create the owner account")
	}
	_, _ = fmt.Fprintln(stdout, "owner account created")
	return nil
}

// runResetOwnerPassword sets a new owner password and signs every session out. It is the recovery path for a
// forgotten password: whoever can run it already has shell and database access to the server. The account is
// SPEEDTEST_OWNER_EMAIL, or the only account when unset.
func runResetOwnerPassword(ctx context.Context, args []string, input io.Reader, stdout io.Writer) error {
	if len(args) != 0 {
		return errors.New("usage: [SPEEDTEST_OWNER_EMAIL=you@example.com] center reset-owner-password < password-file")
	}
	hash, err := readNewPassword(input)
	if err != nil {
		return err
	}
	pool, _, err := openCenterDatabase(ctx, nil)
	if err != nil {
		return err
	}
	defer pool.Close()
	email := ownerEmail()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return errors.New("could not start the password reset")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id string
	if email == "" {
		var users int
		if err := tx.QueryRow(ctx, `SELECT count(*), coalesce(min(id::text), '') FROM users`).Scan(&users, &id); err != nil || users != 1 {
			return errors.New("set SPEEDTEST_OWNER_EMAIL: there is not exactly one account")
		}
	} else if err := tx.QueryRow(ctx, `SELECT id::text FROM users WHERE email=$1`, email).Scan(&id); err != nil {
		return errors.New("no account has that email address")
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET password_hash=$2, updated_at=now() WHERE id=$1`, id, hash); err != nil {
		return errors.New("could not set the password")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sessions`); err != nil {
		return errors.New("could not sign existing sessions out")
	}
	if err := tx.Commit(ctx); err != nil {
		return errors.New("could not save the new password")
	}
	_, _ = fmt.Fprintln(stdout, "owner password reset; all sessions were signed out")
	return nil
}

func validateDefaultSeedEnvironment(appEnv string) error {
	if appEnv != "local" && appEnv != "test" {
		return errors.New("default administrator seed is allowed only when APP_ENV is local or test")
	}
	return nil
}

func validateExplicitDefaultSeedEnvironment(appEnv string, configured bool) error {
	if !configured || strings.TrimSpace(appEnv) == "" {
		return errors.New("APP_ENV must be explicitly set to local or test to seed the default administrator")
	}
	return validateDefaultSeedEnvironment(strings.ToLower(strings.TrimSpace(appEnv)))
}

// runDisableTwoFactor removes the owner's two-factor sign-in and signs every session out. It is the recovery path
// when the authenticator and the recovery codes are lost; whoever can run it already controls the center host.
func runDisableTwoFactor(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) != 0 {
		return errors.New("usage: [SPEEDTEST_OWNER_EMAIL=you@example.com] center disable-2fa")
	}
	pool, _, err := openCenterDatabase(ctx, nil)
	if err != nil {
		return err
	}
	defer pool.Close()
	email := ownerEmail()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return errors.New("could not start the two-factor reset")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id string
	if email == "" {
		var users int
		if err := tx.QueryRow(ctx, `SELECT count(*), coalesce(min(id::text), '') FROM users`).Scan(&users, &id); err != nil || users != 1 {
			return errors.New("set SPEEDTEST_OWNER_EMAIL: there is not exactly one account")
		}
	} else if err := tx.QueryRow(ctx, `SELECT id::text FROM users WHERE email=$1`, email).Scan(&id); err != nil {
		return errors.New("no account has that email address")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_two_factor WHERE user_id=$1`, id); err != nil {
		return errors.New("could not remove two-factor sign-in")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sessions`); err != nil {
		return errors.New("could not sign existing sessions out")
	}
	if err := tx.Commit(ctx); err != nil {
		return errors.New("could not save the change")
	}
	_, _ = fmt.Fprintln(stdout, "two-factor sign-in removed; all sessions were signed out")
	return nil
}
