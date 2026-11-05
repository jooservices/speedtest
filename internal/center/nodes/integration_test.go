//go:build integration

package nodes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/shared/config"
)

func openIntegrationPool(t *testing.T) (*pgxpool.Pool, context.Context) {
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
	var name, marker string
	if err := pool.QueryRow(ctx, `SELECT current_database(),shobj_description(oid,'pg_database') FROM pg_database WHERE datname=current_database()`).Scan(&name, &marker); err != nil || name != cfg.Database.Name || marker != "workspaces-project:speedtest" {
		t.Fatal("test database identity and ownership marker verification failed")
	}
	t.Log("PASS: connected database is the configured non-live Speedtest worktree database")
	return pool, ctx
}

func TestEnrollmentCredentialLifecycleIntegration(t *testing.T) {
	pool, ctx := openIntegrationPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	service := NewService(pool, bytes.Repeat([]byte{0x6c}, 32))
	input := Input{Name: "integration-edge", ExpectedDown: 300, ExpectedUp: 100, Enabled: true, Threads: 4}

	created, replay, err := service.CreateNode(ctx, input, "owner:test-user", "create-key-1")
	if err != nil || replay {
		t.Fatalf("CreateNode() = %s, %t, %v", created, replay, err)
	}
	var issued struct {
		Node struct {
			ID string `json:"id"`
		} `json:"node"`
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	if err := json.Unmarshal(created, &issued); err != nil {
		t.Fatal(err)
	}
	if !validUUID(issued.Node.ID) || !strings.HasPrefix(issued.Token, "enr_") {
		t.Fatalf("invalid issuance response: %s", created)
	}
	if issued.ExpiresAt.Before(time.Now().Add(23*time.Hour)) || issued.ExpiresAt.After(time.Now().Add(25*time.Hour)) {
		t.Fatalf("enrollment expiry = %s, want 24 hours", issued.ExpiresAt)
	}
	var storedHash string
	if err := pool.QueryRow(ctx, `SELECT token_hash FROM node_credentials WHERE node_id=$1 AND kind='enrollment'`, issued.Node.ID).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if storedHash != testHash(issued.Token) || storedHash == issued.Token {
		t.Fatal("enrollment token was not stored as a hash")
	}

	replayed, wasReplay, err := service.CreateNode(ctx, input, "owner:test-user", "create-key-1")
	if err != nil || !wasReplay || !bytes.Equal(created, replayed) {
		t.Fatalf("idempotent replay = %s, %t, %v", replayed, wasReplay, err)
	}
	changed := input
	changed.ExpectedDown++
	if _, _, err := service.CreateNode(ctx, changed, "owner:test-user", "create-key-1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("same key with different body error = %v", err)
	}
	var replayCiphertext []byte
	if err := pool.QueryRow(ctx, `SELECT response_ciphertext FROM api_idempotency WHERE principal=$1 AND key_hash=$2`, "owner:test-user", testHash("create-key-1")).Scan(&replayCiphertext); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(replayCiphertext, []byte(issued.Token)) {
		t.Fatal("idempotency replay stored the enrollment token in plaintext")
	}

	type enrollmentResult struct {
		value map[string]any
		err   error
	}
	start := make(chan struct{})
	results := make(chan enrollmentResult, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			value, enrollErr := service.Enroll(ctx, issued.Token, input.Name, "edge-host", "speedtest-agent/1.0")
			results <- enrollmentResult{value: value, err: enrollErr}
		}()
	}
	close(start)
	var enrolled map[string]any
	successes := 0
	for i := 0; i < 2; i++ {
		result := <-results
		if result.err == nil {
			successes++
			enrolled = result.value
		} else if !errors.Is(result.err, ErrInvalidToken) {
			t.Fatalf("concurrent Enroll() error = %v, want invalid-token loser", result.err)
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent token consumers succeeded %d times, want exactly once", successes)
	}
	if enrolled["nodeId"] != issued.Node.ID {
		t.Fatalf("enrolled node ID = %v", enrolled["nodeId"])
	}
	credential, ok := enrolled["credential"].(string)
	if !ok || !strings.HasPrefix(credential, "sp_") {
		t.Fatalf("enrolled credential = %v", enrolled["credential"])
	}
	if _, err := service.Enroll(ctx, issued.Token, input.Name, "", "speedtest-agent/1.0"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("token reuse error = %v", err)
	}
	if _, err := service.Enroll(ctx, "wrong-token", input.Name, "", "speedtest-agent/1.0"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("wrong token error = %v", err)
	}
	if _, err := service.AuthenticateAgent(ctx, "wrong-credential"); !errors.Is(err, ErrInvalidAuth) {
		t.Fatalf("wrong credential error = %v", err)
	}
	if nodeID, err := service.AuthenticateAgent(ctx, credential); err != nil || nodeID != issued.Node.ID {
		t.Fatalf("AuthenticateAgent() = %q, %v", nodeID, err)
	}
	var bearerHash string
	if err := pool.QueryRow(ctx, `SELECT token_hash FROM node_credentials WHERE node_id=$1 AND kind='bearer'`, issued.Node.ID).Scan(&bearerHash); err != nil {
		t.Fatal(err)
	}
	if bearerHash != testHash(credential) || bearerHash == credential {
		t.Fatal("bearer credential was not stored as a hash")
	}

	rotated, replay, err := service.RotateCredential(ctx, issued.Node.ID, "owner:test-user", "rotate-key-1")
	if err != nil || replay {
		t.Fatalf("RotateCredential() = %s, %t, %v", rotated, replay, err)
	}
	var newCredential struct {
		Credential string `json:"credential"`
	}
	if err := json.Unmarshal(rotated, &newCredential); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AuthenticateAgent(ctx, credential); !errors.Is(err, ErrInvalidAuth) {
		t.Fatalf("old credential after rotation error = %v", err)
	}
	if nodeID, err := service.AuthenticateAgent(ctx, newCredential.Credential); err != nil || nodeID != issued.Node.ID {
		t.Fatalf("new credential auth = %q, %v", nodeID, err)
	}
	rotatedAgain, replay, err := service.RotateCredential(ctx, issued.Node.ID, "owner:test-user", "rotate-key-1")
	if err != nil || !replay || !bytes.Equal(rotated, rotatedAgain) {
		t.Fatalf("credential replay = %s, %t, %v", rotatedAgain, replay, err)
	}
	if err := service.RevokeCredential(ctx, issued.Node.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.RevokeCredential(ctx, issued.Node.ID); err != nil {
		t.Fatalf("repeated revoke error = %v", err)
	}
	if _, err := service.AuthenticateAgent(ctx, newCredential.Credential); !errors.Is(err, ErrInvalidAuth) {
		t.Fatalf("revoked credential auth error = %v", err)
	}
	if err := service.RevokeCredential(ctx, "not-a-uuid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("malformed node ID error = %v", err)
	}
	if err := service.RevokeCredential(ctx, "00000000-0000-4000-8000-000000000099"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing node error = %v", err)
	}

	second := input
	second.Name = "expiry-edge"
	secondIssued, _, err := service.CreateNode(ctx, second, "owner:test-user", "create-key-2")
	if err != nil {
		t.Fatal(err)
	}
	var expiring struct {
		Node struct {
			ID string `json:"id"`
		} `json:"node"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(secondIssued, &expiring); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE node_credentials SET created_at=now()-interval '25 hours',expires_at=now()-interval '1 second' WHERE node_id=$1 AND kind='enrollment'`, expiring.Node.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Enroll(ctx, expiring.Token, second.Name, "", "agent"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired token error = %v", err)
	}

	third := input
	third.Name = "wrong-name-edge"
	thirdIssued, _, err := service.CreateNode(ctx, third, "owner:test-user", "create-key-3")
	if err != nil {
		t.Fatal(err)
	}
	var wrongName struct {
		Node struct {
			ID string `json:"id"`
		} `json:"node"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(thirdIssued, &wrongName); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Enroll(ctx, wrongName.Token, "another-name", "", "agent"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("wrong node name error = %v", err)
	}
	if _, err := service.Enroll(ctx, wrongName.Token, third.Name, "", "agent"); err != nil {
		t.Fatalf("token was consumed by wrong name attempt: %v", err)
	}

	keyData, replay, err := service.RotateEnrollmentKey(ctx, "owner:test-user", "settings-key-1")
	if err != nil || replay {
		t.Fatalf("RotateEnrollmentKey() = %s, %t, %v", keyData, replay, err)
	}
	var keyResponse struct {
		EnrollToken string `json:"enrollToken"`
	}
	if err := json.Unmarshal(keyData, &keyResponse); err != nil {
		t.Fatal(err)
	}
	principal, err := service.ValidateEnrollmentKey(ctx, keyResponse.EnrollToken)
	if err != nil || principal == "" {
		t.Fatalf("ValidateEnrollmentKey() = %q, %v", principal, err)
	}
	var storedKeyHash string
	if err := pool.QueryRow(ctx, `SELECT encode(secret_ciphertext,'hex') FROM settings WHERE key='enrollment_key'`).Scan(&storedKeyHash); err != nil {
		t.Fatal(err)
	}
	if storedKeyHash != testHash(keyResponse.EnrollToken) || storedKeyHash == keyResponse.EnrollToken {
		t.Fatal("Settings enrollment key was not stored as a hash")
	}
	keyReplay, replay, err := service.RotateEnrollmentKey(ctx, "owner:test-user", "settings-key-1")
	if err != nil || !replay || !bytes.Equal(keyData, keyReplay) {
		t.Fatalf("enrollment-key replay = %s, %t, %v", keyReplay, replay, err)
	}
	bootstrapInput := input
	bootstrapInput.Name = "bootstrap-edge"
	bootstrap, replay, err := service.CreateNode(ctx, bootstrapInput, principal, "bootstrap-create-1")
	if err != nil || replay {
		t.Fatalf("bootstrap token create = %s, %t, %v", bootstrap, replay, err)
	}
	if _, err := service.ValidateEnrollmentKey(ctx, "old-settings-key"); !errors.Is(err, ErrInvalidAuth) {
		t.Fatalf("wrong Settings key error = %v", err)
	}

	if _, _, err := service.CreateNode(ctx, input, "owner:test-user", ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("missing idempotency key error = %v", err)
	}
	if _, _, err := NewService(pool, nil).CreateNode(ctx, input, "owner:test-user", "no-app-key"); !errors.Is(err, ErrCryptoKeyUnset) {
		t.Fatalf("missing APP_KEY error = %v", err)
	}
	t.Log("PASS: enrollment reuse/expiry/name binding, hashed storage, credential rotation/revocation, Settings bootstrap key and encrypted idempotent replay")
}

func testHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func TestInstallerTokenRotationAndRevocationIntegration(t *testing.T) {
	pool, ctx := openIntegrationPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	service := NewService(pool, bytes.Repeat([]byte{0x6c}, 32))
	created, _, err := service.CreateNode(ctx, Input{Name: "installer-node", ExpectedDown: 100, ExpectedUp: 50, Enabled: true, Threads: 4}, "owner:test", "installer-node")
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Node struct {
			ID string `json:"id"`
		} `json:"node"`
	}
	if err := json.Unmarshal(created, &response); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.IssueInstallToken(ctx, "invalid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invalid node: %v", err)
	}
	if _, _, err := service.IssueInstallToken(ctx, "00000000-0000-4000-8000-000000000099"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing node: %v", err)
	}
	first, name, err := service.IssueInstallToken(ctx, response.Node.ID)
	if err != nil || first == "" || name != "installer-node" {
		t.Fatalf("issue token name=%q err=%v", name, err)
	}
	second, _, err := service.IssueInstallToken(ctx, response.Node.ID)
	if err != nil || first == second {
		t.Fatal("rotation failed", err)
	}
	if _, err := service.Enroll(ctx, first, name, "host", "test"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("rotated token accepted: %v", err)
	}
	if err := service.RevokeInstallToken(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := service.RevokeInstallToken(ctx, second); err != nil {
		t.Fatal("repeated revocation", err)
	}
	if err := service.RevokeInstallToken(ctx, ""); err != nil {
		t.Fatal("empty revocation", err)
	}
	if _, err := service.Enroll(ctx, second, name, "host", "test"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("revoked token accepted: %v", err)
	}
	third, _, err := service.IssueInstallToken(ctx, response.Node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Enroll(ctx, third, name, "host", "test"); err != nil {
		t.Fatal("fresh token enrollment", err)
	}
	if err := service.RevokeInstallToken(ctx, third); err != nil {
		t.Fatal("consumed token revocation", err)
	}
	var consumed, revoked bool
	if err := pool.QueryRow(ctx, `SELECT consumed_at IS NOT NULL,revoked_at IS NOT NULL FROM node_credentials WHERE token_hash=$1`, hashSecret(third)).Scan(&consumed, &revoked); err != nil || !consumed || revoked {
		t.Fatalf("historical token consumed=%t revoked=%t err=%v", consumed, revoked, err)
	}
}
