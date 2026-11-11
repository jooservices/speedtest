//go:build integration

package settings

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/shared/testenv"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	centerdb "github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/shared/config"
)

func settingsPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	values, err := godotenv.Read(filepath.Join("../../../", ".env"))
	if err != nil {
		t.Fatal("a configured worktree .env is required")
	}
	testenv.Apply(t, values)
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
	return pool
}

func integrationCipher(t *testing.T) *Cipher {
	t.Helper()
	c, err := NewCipher("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func validInputWithSecrets() SettingsInput {
	input := SettingsInput{
		Theme:          "light",
		Range:          "24h",
		Units:          "Gbps",
		Timezone:       "Asia/Ho_Chi_Minh",
		Hour12:         true,
		Decimals:       2,
		RetentionDays:  90,
		GeoProvider:    "geolite2",
		GeoFallback:    "ip-api",
		GeoAutoUpdate:  false,
		Engine:         "ookla",
		DefaultThreads: 8,
		ServerMode:     "pinned",
		MaxConcurrent:  2,
		Stagger:        false,
		QuietStart:     "23:00",
		QuietEnd:       "06:00",
		CenterURL:      "https://speedtest.example.com",
		Notify: NotificationSettingsWrite{
			Email:    ChannelWrite{On: true, To: "ops@example.com"},
			Telegram: ChannelWrite{On: true, To: "-1001234567890"},
			Slack:    ChannelWrite{On: false, To: ""},
			Webhook:  ChannelWrite{On: true, To: ""},
		},
	}
	geoKey := "maxmind-license-key"
	input.GeoKey = &geoKey
	webhook := "https://hooks.example.com/abc123"
	input.Notify.Webhook.Secret = &webhook
	return input
}

func TestSettingsStoreRoundTrip(t *testing.T) {
	pool := settingsPool(t)
	ctx := context.Background()
	cipher := integrationCipher(t)
	store := NewPostgresStore(pool, cipher, Defaults(""))

	view, etag, err := store.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if view.Theme != "dark" || view.RetentionDays != 365 || view.GeoKeyConfigured || view.EnrollKeyConfigured {
		t.Fatalf("fresh defaults = %+v", view)
	}

	input := validInputWithSecrets()
	updated, newETag, err := store.Replace(ctx, etag, input)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Theme != "light" || updated.Decimals != 2 || updated.Timezone != "Asia/Ho_Chi_Minh" {
		t.Fatalf("updated settings = %+v", updated)
	}
	if !updated.GeoKeyConfigured || !updated.Notify.Webhook.Configured {
		t.Fatalf("secret flags not set: %+v", updated)
	}
	if newETag == etag {
		t.Fatal("revision did not advance")
	}

	got, etag2, err := store.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Theme != "light" || !got.GeoKeyConfigured {
		t.Fatalf("masked read = %+v", got)
	}
	if etag2 != newETag {
		t.Fatalf("etag after read = %q, want %q", etag2, newETag)
	}

	var ciphertext []byte
	if err := pool.QueryRow(ctx, "SELECT secret_ciphertext FROM settings WHERE key = 'geolite2.license'").Scan(&ciphertext); err != nil {
		t.Fatalf("secret not stored: %v", err)
	}
	plaintext, err := cipher.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("decrypt stored secret: %v", err)
	}
	if string(plaintext) != "maxmind-license-key" {
		t.Fatalf("decrypted secret = %q", plaintext)
	}

	// Omitting the secret on a subsequent PUT preserves it.
	preserve := validInputWithSecrets()
	preserve.GeoKey = nil
	preserve.Notify.Webhook.Secret = nil
	if _, _, err := store.Replace(ctx, etag2, preserve); err != nil {
		t.Fatal(err)
	}
	after, _, err := store.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !after.GeoKeyConfigured || !after.Notify.Webhook.Configured {
		t.Fatalf("secret was lost when omitted: %+v", after)
	}
	t.Log("PASS: settings round trip, encryption at rest, masking and secret preservation")
}

func TestSettingsStoreConcurrencyControl(t *testing.T) {
	pool := settingsPool(t)
	ctx := context.Background()
	store := NewPostgresStore(pool, integrationCipher(t), Defaults(""))

	_, etag, err := store.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Replace(ctx, "", validInputWithSecrets()); !errors.Is(err, ErrPreconditionRequired) {
		t.Fatalf("missing If-Match = %v, want ErrPreconditionRequired", err)
	}
	if _, _, err := store.Replace(ctx, `"999"`, validInputWithSecrets()); !errors.Is(err, ErrStale) {
		t.Fatalf("stale If-Match = %v, want ErrStale", err)
	}
	if _, _, err := store.Replace(ctx, etag, validInputWithSecrets()); err != nil {
		t.Fatalf("valid replace = %v", err)
	}
	if _, _, err := store.Replace(ctx, etag, validInputWithSecrets()); !errors.Is(err, ErrStale) {
		t.Fatalf("reused If-Match = %v, want ErrStale", err)
	}
	t.Log("PASS: If-Match precondition enforcement")
}

func TestEnrollmentKeyRotation(t *testing.T) {
	pool := settingsPool(t)
	ctx := context.Background()
	store := NewPostgresStore(pool, integrationCipher(t), Defaults(""))

	issued, err := store.RotateEnrollmentKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if issued.EnrollToken == "" {
		t.Fatal("issued token is empty")
	}
	if _, err := time.Parse(time.RFC3339, issued.CreatedAt); err != nil {
		t.Fatalf("createdAt = %q is not RFC3339: %v", issued.CreatedAt, err)
	}

	view, _, err := store.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !view.EnrollKeyConfigured {
		t.Fatal("enrollKeyConfigured not set after rotation")
	}

	var value []byte
	if err := pool.QueryRow(ctx, "SELECT value FROM settings WHERE key = 'enrolment'").Scan(&value); err != nil {
		t.Fatalf("enrolment row missing: %v", err)
	}
	var record enrolmentRecord
	if err := json.Unmarshal(value, &record); err != nil {
		t.Fatal(err)
	}
	if record.Hash == issued.EnrollToken || record.Hash == "" {
		t.Fatalf("stored hash = %q must be a non-empty hash, not the plaintext", record.Hash)
	}
	want := hashToken(issued.EnrollToken)
	if record.Hash != want {
		t.Fatalf("stored hash = %q, want sha256 %q", record.Hash, want)
	}
	if _, err := hex.DecodeString(record.Hash); err != nil {
		t.Fatalf("stored hash is not hex: %v", err)
	}

	// Rotating again replaces the hash and yields a new token.
	second, err := store.RotateEnrollmentKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second.EnrollToken == issued.EnrollToken {
		t.Fatal("two rotations produced the same token")
	}
	t.Log("PASS: enrolment key rotated and stored hashed, never as plaintext")
}

func flatValue(t *testing.T, ctx context.Context, pool *pgxpool.Pool, key string) string {
	t.Helper()
	var value *string
	if err := pool.QueryRow(ctx, `SELECT value #>> '{}' FROM settings WHERE key=$1`, key).Scan(&value); err != nil || value == nil {
		return ""
	}
	return *value
}

func TestSharedValuesReachTheComponentsThatReadThem(t *testing.T) {
	pool := settingsPool(t)
	ctx := t.Context()
	if _, err := pool.Exec(ctx, `INSERT INTO nodes (id,name,expected_down_mbps,expected_up_mbps) VALUES ('00000000-0000-4000-8000-0000000000a1','Alpha',100,50)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO schedules (node_id) VALUES ('00000000-0000-4000-8000-0000000000a1')`); err != nil {
		t.Fatal(err)
	}
	store := NewPostgresStore(pool, integrationCipher(t), Defaults(""))
	_, etag, err := store.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	input := validInputWithSecrets()
	input.Engine = "librespeed"
	if _, etag, err = store.Replace(ctx, etag, input); err != nil {
		t.Fatal(err)
	}
	// The retention job, the agent configuration and the schedule rules read these rows.
	for key, want := range map[string]string{"retentionDays": "90", "engine": "librespeed", "quietStart": "23:00", "quietEnd": "06:00",
		"stagger": "false", "maxConcurrent": "2", "timezone": "Asia/Ho_Chi_Minh"} {
		if got := flatValue(t, ctx, pool, key); got != want {
			t.Errorf("settings row %s = %q, want %q", key, got, want)
		}
	}
	var revision int64
	if err := pool.QueryRow(ctx, `SELECT revision FROM schedules WHERE node_id='00000000-0000-4000-8000-0000000000a1'`).Scan(&revision); err != nil || revision < 2 {
		t.Fatalf("a change of quiet hours must move the schedule revision so agents refetch: %d %v", revision, err)
	}

	// Saving the same values again changes no schedule revision.
	before := revision
	if _, etag, err = store.Replace(ctx, etag, input); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT revision FROM schedules WHERE node_id='00000000-0000-4000-8000-0000000000a1'`).Scan(&revision); err != nil || revision != before {
		t.Fatalf("an unchanged save moved the schedule revision: %d -> %d", before, revision)
	}

	// A value changed elsewhere (the schedule rules write the same rows) is what the settings API reads back.
	if _, err := pool.Exec(ctx, `UPDATE settings SET value = '"01:00"'::jsonb WHERE key = 'quietStart'`); err != nil {
		t.Fatal(err)
	}
	view, _, err := store.Get(ctx)
	if err != nil || view.QuietStart != "01:00" {
		t.Fatalf("settings must show the shared row: %+v %v", view, err)
	}

	// A display zone of "browser" is not an IANA name, so it must not replace the zone agents use for quiet hours.
	input.Timezone = "browser"
	if _, _, err = store.Replace(ctx, etag, input); err != nil {
		t.Fatal(err)
	}
	if got := flatValue(t, ctx, pool, "timezone"); got != "Asia/Ho_Chi_Minh" {
		t.Fatalf("quiet-hours zone after saving browser = %q", got)
	}
}

func TestStoreReportsDatabaseFailures(t *testing.T) {
	pool := settingsPool(t)
	// A second pool is closed on purpose so that the first one can still reset the database afterwards.
	closed, err := pgxpool.NewWithConfig(t.Context(), pool.Config())
	if err != nil {
		t.Fatal(err)
	}
	store := NewPostgresStore(closed, integrationCipher(t), Defaults(""))
	_, etag, err := store.Get(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	if _, _, err := store.Get(t.Context()); err == nil {
		t.Error("Get on a closed pool must fail")
	}
	if _, _, err := store.Replace(t.Context(), etag, validInputWithSecrets()); err == nil {
		t.Error("Replace on a closed pool must fail")
	}
	if _, err := store.RotateEnrollmentKey(t.Context()); err == nil {
		t.Error("RotateEnrollmentKey on a closed pool must fail")
	}
}

func TestMalformedStoredSettingsAreReported(t *testing.T) {
	pool := settingsPool(t)
	if _, err := pool.Exec(t.Context(), `INSERT INTO settings (key, value) VALUES ('display', '"not an object"'::jsonb)`); err != nil {
		t.Fatal(err)
	}
	store := NewPostgresStore(pool, integrationCipher(t), Defaults(""))
	if _, _, err := store.Get(t.Context()); err == nil {
		t.Fatal("a malformed stored value must not be served")
	}
}
