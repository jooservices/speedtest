//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/installer"
	"github.com/jooservices/speedtest/internal/center/nodes"
	"github.com/jooservices/speedtest/internal/center/settings"
	"github.com/jooservices/speedtest/internal/shared/config"
	"github.com/jooservices/speedtest/internal/shared/testenv"
)

func rotationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	values, err := godotenv.Read("../../.env")
	if err != nil {
		t.Fatal("worktree .env required")
	}
	testenv.Apply(t, values)
	t.Setenv("APP_ENV", "test")
	t.Setenv("OLD_APP_KEY", "")
	t.Setenv("NEW_APP_KEY", "")
	cfg, err := config.LoadCenter()
	if err != nil {
		t.Fatal("invalid worktree configuration")
	}
	if err := db.CheckTestDatabase(cfg.Database.Name); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Open(t.Context(), cfg.Database)
	if err != nil {
		t.Fatal("cannot open worktree database")
	}
	t.Cleanup(pool.Close)
	if err := verifyLocalDatabaseIdentity(t.Context(), pool, cfg); err != nil {
		t.Fatal(err)
	}
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	return pool
}

func rotationExec(t *testing.T, pool *pgxpool.Pool, query string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), query, args...); err != nil {
		t.Fatal("fixture SQL failed", err)
	}
}

func seedRotationValues(t *testing.T, pool *pgxpool.Pool) []byte {
	t.Helper()
	oldKey, _, _, _ := rotationTestKeys()
	c, err := settings.NewCipherFromKey(oldKey)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := c.Encrypt([]byte("notification-secret"))
	if err != nil {
		t.Fatal(err)
	}
	// Multiple batches, empty plaintext, plaintext settings, hash-only setting and expired replay.
	for i := 0; i < 105; i++ {
		rotationExec(t, pool, `INSERT INTO settings(key,secret_ciphertext) VALUES($1,$2)`, fmt.Sprintf("secret-%03d", i), encrypted)
	}
	empty, err := c.Encrypt(nil)
	if err != nil {
		t.Fatal(err)
	}
	rotationExec(t, pool, `INSERT INTO settings(key,secret_ciphertext) VALUES('empty',$1),('enrollment_key',$2)`, empty, bytes.Repeat([]byte{9}, 32))
	rotationExec(t, pool, `INSERT INTO settings(key,value) VALUES('plain','{"theme":"dark"}')`)
	aead, err := rotationCipher(oldKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"create-node", "create-alert-rule"} {
		hash := strings.Repeat("a", 64)
		nonce := bytes.Repeat([]byte{3}, aead.NonceSize())
		aad := []byte("owner:test\x00" + op + "\x00" + hash)
		replay := aead.Seal(nonce, nonce, []byte(`{"secret":"replay"}`), aad)
		rotationExec(t, pool, `INSERT INTO api_idempotency(principal,operation,key_hash,request_hash,response_ciphertext,created_at,expires_at) VALUES('owner:test',$1,$2,$2,$3,now()-interval '2 days',now()-interval '1 day')`, op, hash, replay)
	}
	return encrypted
}

func rotationSnapshot(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var snapshot string
	if err := pool.QueryRow(t.Context(), `SELECT (SELECT json_agg(s ORDER BY key)::text FROM settings s) || (SELECT coalesce(json_agg(i ORDER BY operation)::text,'[]') FROM api_idempotency i) || (SELECT coalesce(json_agg(i ORDER BY principal,key_hash)::text,'[]') FROM installer_idempotency i)`).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func assertRotationValues(t *testing.T, pool *pgxpool.Pool, key []byte) {
	t.Helper()
	c, err := rotationCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(t.Context(), `SELECT secret_ciphertext FROM settings WHERE key<>'enrollment_key' AND secret_ciphertext IS NOT NULL ORDER BY key`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var encrypted []byte
		if err := rows.Scan(&encrypted); err != nil {
			t.Fatal(err)
		}
		plain, err := openRotationValue(c, encrypted, nil)
		if err != nil || (len(plain) != 0 && string(plain) != "notification-secret") {
			t.Fatal("settings plaintext changed")
		}
		count++
	}
	if rows.Err() != nil || count != 106 {
		t.Fatal("settings count mismatch")
	}
	rows.Close()
	rows, err = pool.Query(t.Context(), `SELECT principal,operation,key_hash,response_ciphertext FROM api_idempotency`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count = 0
	for rows.Next() {
		var principal, op, hash string
		var encrypted []byte
		if err := rows.Scan(&principal, &op, &hash, &encrypted); err != nil {
			t.Fatal(err)
		}
		plain, err := openRotationValue(c, encrypted, []byte(principal+"\x00"+op+"\x00"+hash))
		if err != nil || string(plain) != `{"secret":"replay"}` {
			t.Fatal("replay plaintext/AAD changed")
		}
		count++
	}
	if rows.Err() != nil || count != 2 {
		t.Fatal("replay count mismatch")
	}
	rows.Close()
	var hash []byte
	var value string
	if err := pool.QueryRow(t.Context(), `SELECT secret_ciphertext FROM settings WHERE key='enrollment_key'`).Scan(&hash); err != nil || !bytes.Equal(hash, bytes.Repeat([]byte{9}, 32)) {
		t.Fatal("enrollment hash changed")
	}
	if err := pool.QueryRow(t.Context(), `SELECT value->>'theme' FROM settings WHERE key='plain'`).Scan(&value); err != nil || value != "dark" {
		t.Fatal("plaintext setting changed")
	}
}

func TestRotateAppKeyIntegration(t *testing.T) {
	pool := rotationPool(t)
	oldKey, newKey, _, _ := rotationTestKeys()
	count, err := rotateAppKey(t.Context(), pool, oldKey, newKey)
	if err != nil || count != (rotationCounts{}) {
		t.Fatal("empty rotation", count, err)
	}
	seedRotationValues(t, pool)
	before := rotationSnapshot(t, pool)
	for _, keys := range [][2][]byte{{oldKey[:2], newKey}, {oldKey, newKey[:2]}, {oldKey, oldKey}, {newKey, oldKey}} {
		if _, err := rotateAppKey(t.Context(), pool, keys[0], keys[1]); err == nil {
			t.Fatal("invalid rotation accepted")
		}
		if rotationSnapshot(t, pool) != before {
			t.Fatal("failed rotation changed storage")
		}
	}
	count, err = rotateAppKey(t.Context(), pool, oldKey, newKey)
	if err != nil || count.values != 108 {
		t.Fatal("rotation", count, err)
	}
	assertRotationValues(t, pool, newKey)
	c, _ := rotationCipher(oldKey)
	var encrypted []byte
	if err := pool.QueryRow(t.Context(), `SELECT secret_ciphertext FROM settings WHERE key='secret-000'`).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if _, err := openRotationValue(c, encrypted, nil); err == nil {
		t.Fatal("old key still decrypts")
	}
	count, err = rotateAppKey(t.Context(), pool, newKey, oldKey)
	if err != nil || count.values != 108 {
		t.Fatal("reverse rotation failed")
	}
	assertRotationValues(t, pool, oldKey)
	t.Log("PASS: 108 values across multiple batches, settings and expired node/alert replay AAD; hash/plaintext unchanged; old key rejected; reverse rotation")
}

func TestRotateAppKeyRollbackIntegration(t *testing.T) {
	pool := rotationPool(t)
	oldKey, newKey, _, _ := rotationTestKeys()
	seedRotationValues(t, pool)
	seedRotationInstaller(t, pool)
	for _, scenario := range []string{"corrupt replay", "write failure", "stored verification failure"} {
		t.Run(scenario, func(t *testing.T) {
			if scenario == "corrupt replay" {
				rotationExec(t, pool, `UPDATE api_idempotency SET response_ciphertext=repeat('x',28)::bytea`)
			} else {
				body := `RAISE EXCEPTION 'private database diagnostic';`
				if scenario == "stored verification failure" {
					body = `NEW.response_ciphertext=repeat('x',28)::bytea; RETURN NEW;`
				}
				rotationExec(t, pool, `CREATE FUNCTION rotation_test_trigger() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN `+body+` END $$`)
				rotationExec(t, pool, `CREATE TRIGGER rotation_test BEFORE UPDATE ON api_idempotency FOR EACH ROW EXECUTE FUNCTION rotation_test_trigger()`)
			}
			before := rotationSnapshot(t, pool)
			if count, err := rotateAppKey(t.Context(), pool, oldKey, newKey); err == nil || count != (rotationCounts{}) || strings.Contains(err.Error(), "private") {
				t.Fatal("failure not safely refused")
			}
			if rotationSnapshot(t, pool) != before {
				t.Fatal("partial rotation committed")
			}
			if scenario != "corrupt replay" {
				rotationExec(t, pool, `DROP TRIGGER rotation_test ON api_idempotency; DROP FUNCTION rotation_test_trigger()`)
			}
			rotationExec(t, pool, `DELETE FROM settings; DELETE FROM api_idempotency`)
			seedRotationValues(t, pool)
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := rotateAppKey(ctx, pool, oldKey, newKey); err == nil {
		t.Fatal("cancelled rotation succeeded")
	}
	t.Log("PASS: corrupt ciphertext, write failure and stored verification failure roll back earlier settings updates without private diagnostics")
}

func TestRotateAppKeyCommandEndToEndIntegration(t *testing.T) {
	pool := rotationPool(t)
	seedRotationValues(t, pool)
	oldKey, newKey, oldValue, newValue := rotationTestKeys()
	if err := os.MkdirAll("../../.tmp", 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("../../.tmp", "rotation-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	binary, err := filepath.Abs(filepath.Join(dir, "center"))
	if err != nil {
		t.Fatal(err)
	}
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %s: %v", output, err)
	}
	seedRotationInstaller(t, pool)
	for _, status := range []string{"pending", "running"} {
		rotationExec(t, pool, `UPDATE installer_jobs SET status=$1`, status)
		before := rotationSnapshot(t, pool)
		cmd := exec.CommandContext(t.Context(), binary, "rotate-app-key")
		cmd.Stdin = strings.NewReader(oldValue + "\n" + newValue + "\n")
		output, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(output), "queued/running") || rotationSnapshot(t, pool) != before {
			t.Fatal("active-job process refusal failed")
		}
	}
	rotationExec(t, pool, `UPDATE installer_jobs SET status='succeeded'`)
	for _, mode := range []string{"stdin", "env"} {
		cmd := exec.CommandContext(t.Context(), binary, "rotate-app-key")
		if mode == "stdin" {
			cmd.Stdin = strings.NewReader(oldValue + "\n" + newValue + "\n")
		} else {
			t.Setenv("OLD_APP_KEY", newValue)
			t.Setenv("NEW_APP_KEY", oldValue)
		}
		output, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(output), "108 values verified") || strings.Contains(string(output), oldValue) || strings.Contains(string(output), newValue) {
			t.Fatal("rotation process failed or exposed key")
		}
		expectedCleared := "0 APP_KEY-HMAC idempotency records cleared"
		if mode == "stdin" {
			expectedCleared = "2 APP_KEY-HMAC idempotency records cleared"
		}
		if !strings.Contains(string(output), expectedCleared) {
			t.Fatal("process cleared count mismatch")
		}
		if mode == "stdin" {
			assertRotationValues(t, pool, newKey)
		} else {
			assertRotationValues(t, pool, oldKey)
		}
	}
	t.Setenv("OLD_APP_KEY", newValue)
	t.Setenv("NEW_APP_KEY", oldValue)
	before := rotationSnapshot(t, pool)
	cmd := exec.CommandContext(t.Context(), binary, "rotate-app-key")
	output, err := cmd.CombinedOutput()
	if err == nil || strings.Contains(string(output), newValue) || rotationSnapshot(t, pool) != before {
		t.Fatal("wrong-key process did not fail safely")
	}
	// Exercise the callable command's input refusal without contacting a database.
	t.Setenv("NEW_APP_KEY", "")
	if err := runRotateAppKey(t.Context(), nil, strings.NewReader(""), io.Discard); err == nil {
		t.Fatal("partial environment accepted")
	}
	t.Log("PASS: actual center binary stdin/env rotation, decrypt after restart with new key, reverse rotation and wrong-key process rollback; no key output")
}

func TestRotateAppKeyCommandIntegration(t *testing.T) {
	pool := rotationPool(t)
	seedRotationValues(t, pool)
	oldKey, newKey, oldValue, newValue := rotationTestKeys()
	var output bytes.Buffer
	if err := runRotateAppKey(t.Context(), nil, strings.NewReader(oldValue+"\n"+newValue+"\n"), &output); err != nil || !strings.Contains(output.String(), "108 values verified") {
		t.Fatal("command rotation failed", err)
	}
	assertRotationValues(t, pool, newKey)
	t.Setenv("OLD_APP_KEY", newValue)
	t.Setenv("NEW_APP_KEY", oldValue)
	if err := runRotateAppKey(t.Context(), nil, strings.NewReader(""), io.Discard); err != nil {
		t.Fatal("command reversal failed", err)
	}
	assertRotationValues(t, pool, oldKey)
	t.Setenv("OLD_APP_KEY", newValue)
	if err := runRotateAppKey(t.Context(), nil, strings.NewReader(""), io.Discard); err == nil {
		t.Fatal("wrong key accepted")
	}
	t.Setenv("SPEEDTEST_DB_PORT", "invalid")
	if err := runRotateAppKey(t.Context(), nil, strings.NewReader(""), io.Discard); err == nil {
		t.Fatal("invalid config accepted")
	}
	t.Setenv("SPEEDTEST_DB_PORT", fmt.Sprint(pool.Config().ConnConfig.Port))
	t.Setenv("SPEEDTEST_DB_NAME", "speedtest")
	if err := runRotateAppKey(t.Context(), nil, strings.NewReader(""), io.Discard); err == nil {
		t.Fatal("live test DB accepted")
	}
	t.Setenv("SPEEDTEST_DB_NAME", pool.Config().ConnConfig.Database)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := runRotateAppKey(ctx, nil, strings.NewReader(""), io.Discard); err == nil {
		t.Fatal("cancelled connection accepted")
	}
}

func TestRotateAppKeyConsumersEndToEndIntegration(t *testing.T) {
	pool := rotationPool(t)
	oldKey, newKey, oldValue, newValue := rotationTestKeys()
	input := nodes.Input{Name: "rotation-node", Enabled: true, Threads: 4, ExpectedDown: 100, ExpectedUp: 100}
	original, replay, err := nodes.NewService(pool, oldKey).CreateNode(t.Context(), input, "owner:rotation", "rotation-request")
	if err != nil || replay {
		t.Fatal("node issuance failed", err)
	}
	// Use a closed worker pool to exercise the real installer persistence/retry
	// consumer without performing an unrelated remote SSH installation.
	var nodeID string
	if err := pool.QueryRow(t.Context(), `SELECT id::text FROM nodes WHERE name='rotation-node'`).Scan(&nodeID); err != nil {
		t.Fatal(err)
	}
	oldInstaller := rotationInstallerConsumer(t, pool, oldKey)
	installInput := installer.InstallInput{ProbeID: "00000000-0000-4000-8000-000000000061", Fingerprint: "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", SSH: installer.SSHInput{Host: "127.0.0.1", Port: 22, User: "root", Auth: "password", Secret: "fixture-password"}}
	rotationExec(t, pool, `INSERT INTO installer_probes(id,host,port,fingerprint,expires_at) VALUES($1,$2,$3,$4,now()+interval '10 minutes')`, installInput.ProbeID, installInput.SSH.Host, installInput.SSH.Port, installInput.Fingerprint)
	job, err := oldInstaller.Start(t.Context(), "owner:rotation", "installer-retry", nodeID, installInput)
	if err != nil {
		t.Fatal(err)
	}
	prior, err := oldInstaller.Start(t.Context(), "owner:rotation", "installer-retry", nodeID, installInput)
	if err != nil || prior.ID != job.ID {
		t.Fatal("pre-rotation installer replay failed", err)
	}
	c, err := settings.NewCipherFromKey(oldKey)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := c.Encrypt([]byte("worker-notification-secret"))
	if err != nil {
		t.Fatal(err)
	}
	rotationExec(t, pool, `INSERT INTO settings(key,secret_ciphertext) VALUES('notify.webhook.secret',$1)`, encrypted)
	if err := runRotateAppKey(t.Context(), nil, strings.NewReader(oldValue+"\n"+newValue+"\n"), io.Discard); err != nil {
		t.Fatal(err)
	}
	current, replay, err := nodes.NewService(pool, newKey).CreateNode(t.Context(), input, "owner:rotation", "rotation-request")
	if err != nil || !replay || !bytes.Equal(current, original) {
		t.Fatal("new-key service did not replay original issuance")
	}
	if _, _, err := nodes.NewService(pool, oldKey).CreateNode(t.Context(), input, "owner:rotation", "rotation-request"); err == nil {
		t.Fatal("old-key service decrypted rotated replay")
	}
	c, err = settings.NewCipherFromKey(newKey)
	if err != nil {
		t.Fatal(err)
	}
	store := settings.NewPostgresStore(pool, c, settings.Defaults(""))
	secret, err := store.NotificationSecret(t.Context(), "webhook")
	if err != nil || secret != "worker-notification-secret" {
		t.Fatal("new-key notification worker cannot decrypt secret")
	}
	newInstaller := rotationInstallerConsumer(t, pool, newKey)
	if _, err := newInstaller.Start(t.Context(), "owner:rotation", "installer-retry", nodeID, installInput); !errors.Is(err, installer.ErrConflict) {
		t.Fatal("consumed probe was replayed instead of validated as new request", err)
	}
	installInput.ProbeID = "00000000-0000-4000-8000-000000000062"
	rotationExec(t, pool, `INSERT INTO installer_probes(id,host,port,fingerprint,expires_at) VALUES($1,$2,$3,$4,now()+interval '10 minutes')`, installInput.ProbeID, installInput.SSH.Host, installInput.SSH.Port, installInput.Fingerprint)
	next, err := newInstaller.Start(t.Context(), "owner:rotation", "installer-retry", nodeID, installInput)
	if err != nil || next.ID == job.ID {
		t.Fatal("new-key installer did not create a new request", err)
	}
	replayed, err := newInstaller.Start(t.Context(), "owner:rotation", "installer-retry", nodeID, installInput)
	if err != nil || replayed.ID != next.ID {
		t.Fatal("new-key installer retry did not replay", err)
	}
	t.Log("PASS: installer old retry invalidated; consumed probe refused; fresh probe with same idempotency key creates a new job and subsequent retry replays")
	t.Log("PASS: real node service replays identical enrollment response after rotation; old-key service rejected; notification settings consumer decrypts with NEW")
}

// The queued state is represented as pending in the persisted installer model.
func seedRotationInstaller(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	rotationExec(t, pool, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES('00000000-0000-4000-8000-000000000059','rotation-installer',100,100)`)
	rotationExec(t, pool, `INSERT INTO installer_jobs(id,node_id,principal,status) VALUES('00000000-0000-4000-8000-000000000060','00000000-0000-4000-8000-000000000059','owner','succeeded')`)
	rotationExec(t, pool, `INSERT INTO installer_idempotency(principal,key_hash,request_hash,installation_id,created_at,expires_at) VALUES('owner',repeat('a',64),repeat('b',64),'00000000-0000-4000-8000-000000000060',now()-interval '2 days',now()-interval '1 day'),('owner',repeat('c',64),repeat('d',64),'00000000-0000-4000-8000-000000000060',now(),now()+interval '24 hours')`)
}

func TestRotateAppKeyInstallerPolicyIntegration(t *testing.T) {
	pool := rotationPool(t)
	oldKey, newKey, oldValue, newValue := rotationTestKeys()
	seedRotationValues(t, pool)
	seedRotationInstaller(t, pool)
	before := rotationSnapshot(t, pool)
	for _, status := range []string{"pending", "running"} {
		rotationExec(t, pool, `UPDATE installer_jobs SET status=$1`, status)
		counts, err := rotateAppKey(t.Context(), pool, oldKey, newKey)
		if err == nil || !strings.Contains(err.Error(), "queued/running") || counts != (rotationCounts{}) || rotationSnapshot(t, pool) != before {
			t.Fatal("active installation did not refuse atomically", err)
		}
	}
	rotationExec(t, pool, `UPDATE installer_jobs SET status='succeeded'`)
	// Simulate an SQL failure on HMAC invalidation; no ciphertext may change.
	rotationExec(t, pool, `CREATE FUNCTION rotation_delete_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'private delete diagnostic'; END $$`)
	rotationExec(t, pool, `CREATE TRIGGER rotation_delete_failure BEFORE DELETE ON installer_idempotency FOR EACH ROW EXECUTE FUNCTION rotation_delete_failure()`)
	if counts, err := rotateAppKey(t.Context(), pool, oldKey, newKey); err == nil || counts != (rotationCounts{}) || strings.Contains(err.Error(), "private") || rotationSnapshot(t, pool) != before {
		t.Fatal("delete failure was not atomic and sanitized")
	}
	rotationExec(t, pool, `DROP TRIGGER rotation_delete_failure ON installer_idempotency; DROP FUNCTION rotation_delete_failure()`)
	var output bytes.Buffer
	if err := runRotateAppKey(t.Context(), nil, strings.NewReader(oldValue+"\n"+newValue+"\n"), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "2 APP_KEY-HMAC idempotency records cleared (pre-rotation retries are new requests)") {
		t.Fatal("missing invalidation count/retry semantics")
	}
	var remaining, jobs int
	if err := pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM installer_idempotency),(SELECT count(*) FROM installer_jobs)`).Scan(&remaining, &jobs); err != nil || remaining != 0 || jobs != 1 {
		t.Fatal("HMAC records not cleared or installation history changed")
	}
	assertRotationValues(t, pool, newKey)
	t.Log("PASS: pending/running jobs refuse atomically; delete failure rolls back; expired and active HMAC records cleared; 2 cleared reported; installation history retained")
}

func rotationInstallerConsumer(t *testing.T, pool *pgxpool.Pool, key []byte) *installer.Service {
	t.Helper()
	service, err := installer.NewService(t.Context(), installer.ServiceOptions{Database: pool, Enrollment: nodes.NewService(pool, key), CenterURL: "http://127.0.0.1", AppKey: key})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	return service
}

func TestRotateAppKeyReencryptsTwoFactorSecretsIntegration(t *testing.T) {
	pool := rotationPool(t)
	oldKey, newKey, _, _ := rotationTestKeys()
	oldAEAD, err := rotationCipher(oldKey)
	if err != nil {
		t.Fatal(err)
	}
	nonce := bytes.Repeat([]byte{5}, oldAEAD.NonceSize())
	sealed := oldAEAD.Seal(nonce, nonce, []byte("JBSWY3DPEHPK3PXP"), nil)
	rotationExec(t, pool, `INSERT INTO users(id,email,password_hash) VALUES('00000000-0000-4000-8000-0000000000aa','owner@example.test','x')`)
	rotationExec(t, pool, `INSERT INTO user_two_factor(user_id,secret_enc,enabled,recovery_hashes) VALUES('00000000-0000-4000-8000-0000000000aa',$1,true,'{h1,h2}')`, sealed)
	count, err := rotateAppKey(t.Context(), pool, oldKey, newKey)
	if err != nil || count.values != 1 {
		t.Fatal("rotation", count, err)
	}
	var stored []byte
	var hashes []string
	if err := pool.QueryRow(t.Context(), `SELECT secret_enc, recovery_hashes FROM user_two_factor`).Scan(&stored, &hashes); err != nil {
		t.Fatal(err)
	}
	newAEAD, _ := rotationCipher(newKey)
	if plain, err := openRotationValue(newAEAD, stored, nil); err != nil || string(plain) != "JBSWY3DPEHPK3PXP" {
		t.Fatal("the new key cannot open the rotated two-factor secret", err)
	}
	if _, err := openRotationValue(oldAEAD, stored, nil); err == nil {
		t.Fatal("the old key still opens the rotated two-factor secret")
	}
	if len(hashes) != 2 {
		t.Fatalf("recovery digests changed: %v", hashes)
	}
}
