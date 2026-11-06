package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type memoryStore struct {
	mu           sync.Mutex
	users        map[string]Credential
	sessions     map[string]SessionRecord
	createErr    error
	getErr       error
	findEmailErr error
	findIDErr    error
	rotateErr    error
	deleteErr    error
	changeErr    error
	seedErr      error
	tf           map[string]*TwoFactor
	tfErr        error
}

func newMemoryStore() *memoryStore {
	return &memoryStore{users: make(map[string]Credential), sessions: make(map[string]SessionRecord)}
}

func (s *memoryStore) CreateSession(_ context.Context, hash string, userID *string, csrfHash string, expires time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createErr != nil {
		return s.createErr
	}
	record := SessionRecord{CSRFTokenHash: csrfHash, ExpiresAt: expires}
	if userID != nil {
		for _, user := range s.users {
			if user.ID == *userID {
				identity := user.Identity
				record.Identity = &identity
			}
		}
	}
	s.sessions[hash] = record
	return nil
}

func (s *memoryStore) GetSession(_ context.Context, hash string, now time.Time) (SessionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return SessionRecord{}, s.getErr
	}
	record, ok := s.sessions[hash]
	if !ok || !record.ExpiresAt.After(now) {
		return SessionRecord{}, ErrInvalidSession
	}
	return record, nil
}

func (s *memoryStore) FindUserByEmail(_ context.Context, email string) (Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.findEmailErr != nil {
		return Credential{}, s.findEmailErr
	}
	user, ok := s.users[normalizeEmail(email)]
	if !ok {
		return Credential{}, ErrNotFound
	}
	return user, nil
}

func (s *memoryStore) FindUserByID(_ context.Context, id string) (Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.findIDErr != nil {
		return Credential{}, s.findIDErr
	}
	for _, user := range s.users {
		if user.ID == id {
			return user, nil
		}
	}
	return Credential{}, ErrNotFound
}

func (s *memoryStore) RotateAnonymousSession(_ context.Context, oldHash, userID, newHash, csrfHash string, expires time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rotateErr != nil {
		return s.rotateErr
	}
	old, ok := s.sessions[oldHash]
	if !ok || old.Identity != nil || !old.ExpiresAt.After(time.Now().Add(-24*time.Hour)) {
		return ErrInvalidSession
	}
	delete(s.sessions, oldHash)
	for _, user := range s.users {
		if user.ID == userID {
			identity := user.Identity
			s.sessions[newHash] = SessionRecord{Identity: &identity, CSRFTokenHash: csrfHash, ExpiresAt: expires}
			return nil
		}
	}
	return ErrNotFound
}

func (s *memoryStore) DeleteSession(_ context.Context, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleteErr != nil {
		return s.deleteErr
	}
	if _, ok := s.sessions[hash]; !ok {
		return ErrInvalidSession
	}
	delete(s.sessions, hash)
	return nil
}

func (s *memoryStore) ChangePassword(_ context.Context, userID, oldHash, newHash, oldSession, newSession, csrfHash string, expires time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.changeErr != nil {
		return s.changeErr
	}
	var found bool
	for key, user := range s.users {
		if user.ID == userID && user.PasswordHash == oldHash {
			user.PasswordHash = newHash
			s.users[key] = user
			found = true
		}
	}
	if !found {
		return ErrInvalidCurrentPass
	}
	current, ok := s.sessions[oldSession]
	if !ok || current.Identity == nil || current.Identity.ID != userID {
		return ErrInvalidSession
	}
	for key, record := range s.sessions {
		if record.Identity != nil && record.Identity.ID == userID && key != oldSession {
			delete(s.sessions, key)
		}
	}
	delete(s.sessions, oldSession)
	s.sessions[newSession] = SessionRecord{Identity: current.Identity, CSRFTokenHash: csrfHash, ExpiresAt: expires}
	return nil
}

func (s *memoryStore) SeedAdmin(_ context.Context, email, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seedErr != nil {
		return s.seedErr
	}
	key := normalizeEmail(email)
	if _, exists := s.users[key]; !exists {
		s.users[key] = Credential{Identity: Identity{ID: "owner-id", Email: key}, PasswordHash: hash}
	}
	return nil
}

func testService(t *testing.T, store Store, config Config) *Service {
	t.Helper()
	service, err := NewService(store, config)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func defaultAuthConfig() Config {
	return Config{SessionTTL: time.Hour, LoginMaxAttempts: 5, LoginWindow: 15 * time.Minute}
}

func addTestUser(t *testing.T, store *memoryStore, email, password string) Credential {
	t.Helper()
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	user := Credential{Identity: Identity{ID: "user-id", Email: normalizeEmail(email)}, PasswordHash: hash}
	store.users[normalizeEmail(email)] = user
	return user
}

func TestPasswordHashRoundTripAndPolicy(t *testing.T) {
	password := "a correct horse battery staple"
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("HashPassword() returned an unexpected Argon2id format")
	}
	if !VerifyPassword(password, hash) || VerifyPassword("a different password", hash) {
		t.Fatal("VerifyPassword() did not distinguish the matching password")
	}
	second, err := HashPassword(password)
	if err != nil || second == hash {
		t.Fatal("HashPassword() must generate a fresh salt for every password")
	}
	if _, err := HashPassword("short"); !errors.Is(err, ErrPasswordPolicy) {
		t.Fatalf("HashPassword(short) error = %v, want ErrPasswordPolicy", err)
	}
	if VerifyPassword(password, "invalid") || VerifyPassword(password, "argon2id$v=19$m=4294967295,t=6,p=4$bad$bad") {
		t.Fatal("VerifyPassword() accepted a malformed or over-budget hash")
	}
}

func TestSessionTokenCSRFAndExpiry(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store, defaultAuthConfig())
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	created, err := service.Bootstrap(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if created.Token == "" || created.CSRFToken == "" || !created.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatal("bootstrap session fields were not created as expected")
	}
	if _, storedPlaintext := store.sessions[created.Token]; storedPlaintext {
		t.Fatal("store contains the raw session token")
	}
	if !MatchCSRF(created.CSRFToken, created.CSRFToken) || MatchCSRF(created.CSRFToken, "wrong") {
		t.Fatal("CSRF token comparison failed")
	}
	read, err := service.Session(context.Background(), created.Token)
	if err != nil || read.CSRFToken != created.CSRFToken || read.Identity != nil {
		t.Fatalf("Session() = %+v, %v; want the anonymous session", read, err)
	}
	now = now.Add(time.Hour)
	if _, err := service.Session(context.Background(), created.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("expired Session() error = %v, want ErrInvalidSession", err)
	}
}

func TestLoginRotatesAnonymousSessionAndKeepsFailuresGeneric(t *testing.T) {
	store := newMemoryStore()
	user := addTestUser(t, store, "Owner@Example.com", "a valid owner password")
	service := testService(t, store, defaultAuthConfig())
	anonymous, err := service.Bootstrap(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Login(context.Background(), anonymous.Token, "owner@example.com", "wrong password", "192.0.2.4"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong-password Login() error = %v, want ErrInvalidCredentials", err)
	}
	if _, err := service.Login(context.Background(), anonymous.Token, "missing@example.com", "wrong password", "192.0.2.5"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown-account Login() error = %v, want ErrInvalidCredentials", err)
	}
	created, err := service.Login(context.Background(), anonymous.Token, "OWNER@example.com", "a valid owner password", "192.0.2.4")
	if err != nil || created.Identity == nil || created.Identity.ID != user.ID || created.Token == anonymous.Token {
		t.Fatalf("successful Login() = %+v, %v", created, err)
	}
	if _, err := service.Session(context.Background(), anonymous.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatal("login did not invalidate the anonymous session")
	}
	if _, err := service.Login(context.Background(), created.Token, "owner@example.com", "a valid owner password", "192.0.2.6"); !errors.Is(err, ErrSessionConflict) {
		t.Fatalf("login with authenticated session error = %v, want conflict", err)
	}
}

func TestLoginRateLimitIsPerIPAndAccount(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store, Config{SessionTTL: time.Hour, LoginMaxAttempts: 1, LoginWindow: time.Minute})
	first, _ := service.Bootstrap(context.Background())
	if _, err := service.Login(context.Background(), first.Token, "missing@example.com", "invalid password", "192.0.2.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("first Login() error = %v, want invalid credentials", err)
	}
	second, _ := service.Bootstrap(context.Background())
	_, err := service.Login(context.Background(), second.Token, "another@example.com", "invalid password", "192.0.2.1")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("same-IP Login() error = %v, want ErrRateLimited", err)
	}
	third, _ := service.Bootstrap(context.Background())
	_, err = service.Login(context.Background(), third.Token, "missing@example.com", "invalid password", "192.0.2.2")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("same-account Login() error = %v, want ErrRateLimited", err)
	}
}

func TestPasswordChangeRotatesSessionAndInvalidatesOthers(t *testing.T) {
	store := newMemoryStore()
	user := addTestUser(t, store, "owner@example.com", "the old owner password")
	service := testService(t, store, defaultAuthConfig())
	anonymous, err := service.Bootstrap(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	current, err := service.Login(context.Background(), anonymous.Token, "owner@example.com", "the old owner password", "192.0.2.10")
	if err != nil {
		t.Fatal(err)
	}
	otherToken, _ := newOpaqueToken()
	otherCSRF := csrfForSession(otherToken)
	if err := store.CreateSession(context.Background(), tokenHash(otherToken), &user.ID, tokenHash(otherCSRF), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ChangePassword(context.Background(), current.Token, "incorrect current password", "a new secure password"); !errors.Is(err, ErrInvalidCurrentPass) {
		t.Fatalf("wrong current password error = %v", err)
	}
	rotated, err := service.ChangePassword(context.Background(), current.Token, "the old owner password", "a new secure password")
	if err != nil || rotated.Token == current.Token {
		t.Fatalf("ChangePassword() = %+v, %v", rotated, err)
	}
	if _, err := service.Session(context.Background(), current.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatal("password change did not rotate the current session")
	}
	if _, err := service.Session(context.Background(), otherToken); !errors.Is(err, ErrInvalidSession) {
		t.Fatal("password change did not invalidate other sessions")
	}
	credential, _ := store.FindUserByID(context.Background(), user.ID)
	if !VerifyPassword("a new secure password", credential.PasswordHash) || VerifyPassword("the old owner password", credential.PasswordHash) {
		t.Fatal("password change did not replace the stored Argon2id hash")
	}
	if _, err := service.ChangePassword(context.Background(), rotated.Token, "a new secure password", "short"); !errors.Is(err, ErrPasswordPolicy) {
		t.Fatalf("short new password error = %v, want policy error", err)
	}
}

func TestEnsureDefaultAdminIsIdempotentAndHashed(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store, defaultAuthConfig())
	if err := service.EnsureDefaultAdmin(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, err := store.FindUserByEmail(context.Background(), DefaultAdminEmail)
	if err != nil || !VerifyPassword(DefaultAdminPassword, first.PasswordHash) || strings.Contains(first.PasswordHash, DefaultAdminPassword) {
		t.Fatal("default admin was not stored using a password hash")
	}
	if err := service.EnsureDefaultAdmin(context.Background()); err != nil {
		t.Fatal(err)
	}
	second, _ := store.FindUserByEmail(context.Background(), DefaultAdminEmail)
	if first.PasswordHash != second.PasswordHash {
		t.Fatal("re-running the seed changed the existing password")
	}
}

func TestNewServiceRejectsInvalidConfiguration(t *testing.T) {
	if _, err := NewService(newMemoryStore(), Config{}); err == nil {
		t.Fatal("NewService() accepted missing rate/session limits")
	}
	if _, err := NewService(nil, defaultAuthConfig()); err == nil {
		t.Fatal("NewService() accepted a nil store")
	}
}

func TestAuthServicePropagatesStoreErrorsAndRejectsInvalidCSRF(t *testing.T) {
	failure := errors.New("store unavailable")
	store := newMemoryStore()
	service := testService(t, store, defaultAuthConfig())

	store.createErr = failure
	if _, err := service.Bootstrap(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("Bootstrap() error = %v, want store error", err)
	}
	store.createErr = nil
	anonymous, err := service.Bootstrap(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Session(context.Background(), ""); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("Session(empty token) error = %v, want invalid session", err)
	}
	store.getErr = failure
	if _, err := service.Login(context.Background(), anonymous.Token, "owner@example.com", "a valid password", "192.0.2.3"); !errors.Is(err, failure) {
		t.Fatalf("Login() session lookup error = %v, want store error", err)
	}
	if _, err := service.ChangePassword(context.Background(), anonymous.Token, "current password", "a new secure password"); !errors.Is(err, failure) {
		t.Fatalf("ChangePassword() session lookup error = %v, want store error", err)
	}
	store.getErr = nil
	if _, err := service.ChangePassword(context.Background(), anonymous.Token, "current password", "a new secure password"); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("ChangePassword(anonymous session) error = %v, want invalid session", err)
	}
	store.getErr = failure
	if _, err := service.Session(context.Background(), anonymous.Token); !errors.Is(err, failure) {
		t.Fatalf("Session() error = %v, want store error", err)
	}
	store.getErr = nil
	store.sessions[tokenHash(anonymous.Token)] = SessionRecord{CSRFTokenHash: tokenHash("wrong csrf"), ExpiresAt: time.Now().Add(time.Hour)}
	if _, err := service.Session(context.Background(), anonymous.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("Session() with mismatched CSRF hash error = %v, want invalid session", err)
	}

	store = newMemoryStore()
	service = testService(t, store, defaultAuthConfig())
	addTestUser(t, store, "owner@example.com", "a valid owner password")
	anonymous, err = service.Bootstrap(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	store.findEmailErr = failure
	if _, err := service.Login(context.Background(), anonymous.Token, "owner@example.com", "a valid owner password", "192.0.2.1"); !errors.Is(err, failure) {
		t.Fatalf("Login() lookup error = %v, want store error", err)
	}
	store.findEmailErr = nil
	store.rotateErr = failure
	if _, err := service.Login(context.Background(), anonymous.Token, "owner@example.com", "a valid owner password", "192.0.2.2"); !errors.Is(err, failure) {
		t.Fatalf("Login() rotation error = %v, want store error", err)
	}
	store.rotateErr = nil
	current, err := service.Login(context.Background(), anonymous.Token, "owner@example.com", "a valid owner password", "192.0.2.3")
	if err != nil {
		t.Fatal(err)
	}
	store.findIDErr = failure
	if _, err := service.ChangePassword(context.Background(), current.Token, "a valid owner password", "a new secure owner password"); !errors.Is(err, failure) {
		t.Fatalf("ChangePassword() lookup error = %v, want store error", err)
	}
	store.findIDErr = nil
	store.changeErr = failure
	if _, err := service.ChangePassword(context.Background(), current.Token, "a valid owner password", "a new secure owner password"); !errors.Is(err, failure) {
		t.Fatalf("ChangePassword() update error = %v, want store error", err)
	}
	store.changeErr = nil
	if err := service.Logout(context.Background(), ""); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("Logout(empty token) error = %v, want invalid session", err)
	}
	store.deleteErr = failure
	if err := service.Logout(context.Background(), current.Token); !errors.Is(err, failure) {
		t.Fatalf("Logout() store error = %v, want store error", err)
	}
	store.seedErr = failure
	if err := service.EnsureDefaultAdmin(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("EnsureDefaultAdmin() store error = %v, want store error", err)
	}
}

func TestRateLimitErrorAndTokenMismatch(t *testing.T) {
	err := rateLimitError{retryAfter: 17 * time.Second}
	if err.Error() != ErrRateLimited.Error() || !errors.Is(err, ErrRateLimited) || err.RetryAfter() != 17*time.Second {
		t.Fatalf("rate-limit error = %q, retry %s", err.Error(), err.RetryAfter())
	}
	if tokenMatches("presented", "short") {
		t.Fatal("tokenMatches accepted hashes with different lengths")
	}
}

type failingRandomReader struct{}

func (failingRandomReader) Read([]byte) (int, error) {
	return 0, errors.New("random source unavailable")
}

func TestRandomSourceFailuresAreReported(t *testing.T) {
	original := rand.Reader
	t.Cleanup(func() { rand.Reader = original })
	rand.Reader = failingRandomReader{}

	if _, err := HashPassword("a sufficiently long password"); err == nil {
		t.Fatal("HashPassword() accepted an unavailable random source")
	}
	if _, err := newOpaqueToken(); err == nil {
		t.Fatal("newOpaqueToken() accepted an unavailable random source")
	}
	if _, err := hashSeedPassword(DefaultAdminPassword); err == nil {
		t.Fatal("hashSeedPassword() accepted an unavailable random source")
	}
	if _, err := randomUUID(); err == nil {
		t.Fatal("randomUUID() accepted an unavailable random source")
	}

	store := newMemoryStore()
	service := testService(t, store, defaultAuthConfig())
	if _, err := service.Bootstrap(context.Background()); err == nil {
		t.Fatal("Bootstrap() accepted an unavailable random source")
	}
	if err := service.EnsureDefaultAdmin(context.Background()); err == nil {
		t.Fatal("EnsureDefaultAdmin() accepted an unavailable random source")
	}

	// Prepare password-change and login paths before making their token/hash
	// generation fail, so the failure is observed at the intended boundary.
	rand.Reader = original
	roundTripStore := newMemoryStore()
	addTestUser(t, roundTripStore, "owner@example.com", "a valid owner password")
	roundTripService := testService(t, roundTripStore, defaultAuthConfig())
	anonymous, err := roundTripService.Bootstrap(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	current, err := roundTripService.Login(context.Background(), anonymous.Token, "owner@example.com", "a valid owner password", "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	secondAnonymous, err := roundTripService.Bootstrap(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rand.Reader = failingRandomReader{}
	if _, err := roundTripService.Login(context.Background(), secondAnonymous.Token, "owner@example.com", "a valid owner password", "192.0.2.2"); err == nil {
		t.Fatal("Login() accepted an unavailable random source")
	}
	rand.Reader = failingRandomReader{}
	if _, err := roundTripService.ChangePassword(context.Background(), current.Token, "a valid owner password", "another newly changed owner password"); err == nil {
		t.Fatal("ChangePassword() accepted an unavailable random source while hashing")
	}
	rand.Reader = bytes.NewReader(make([]byte, passwordSaltBytes))
	if _, err := roundTripService.ChangePassword(context.Background(), current.Token, "a valid owner password", "a newly changed owner password"); err == nil {
		t.Fatal("ChangePassword() accepted an unavailable random source after hashing")
	}
}

func TestPasswordHashParserRejectsMalformedParameters(t *testing.T) {
	salt := make([]byte, passwordSaltBytes)
	expected := make([]byte, passwordKeyBytes)
	valid := "argon2id$v=19$m=19456,t=2,p=1$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(expected)
	cases := []string{
		"invalid",
		"argon2i$v=19$m=19456,t=2,p=1$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(expected),
		"argon2id$v=bad$m=19456,t=2,p=1$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(expected),
		"argon2id$v=18$m=19456,t=2,p=1$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(expected),
		"argon2id$v=19$m=19456,t=2,p$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(expected),
		"argon2id$v=19$m=bad,t=2,p=1$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(expected),
		"argon2id$v=19$m=19456,t=2,p=256$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(expected),
		"argon2id$v=19$m=19456,t=2,p=1,x=1$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(expected),
		"argon2id$v=19$m=8191,t=2,p=1$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(expected),
		"argon2id$v=19$m=19456,t=0,p=1$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(expected),
		"argon2id$v=19$m=19456,t=2,p=0$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(expected),
		"argon2id$v=19$m=19456,t=2,p=1$%%%$" + base64.RawStdEncoding.EncodeToString(expected),
		"argon2id$v=19$m=19456,t=2,p=1$AQ$" + base64.RawStdEncoding.EncodeToString(expected),
		"argon2id$v=19$m=19456,t=2,p=1$" + base64.RawStdEncoding.EncodeToString(make([]byte, 33)) + "$" + base64.RawStdEncoding.EncodeToString(expected),
		"argon2id$v=19$m=19456,t=2,p=1$" + base64.RawStdEncoding.EncodeToString(salt) + "$%%%",
		"argon2id$v=19$m=19456,t=2,p=1$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(make([]byte, 15)),
		"argon2id$v=19$m=19456,t=2,p=1$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(make([]byte, 65)),
	}
	for _, encoded := range cases {
		if _, _, _, ok := parsePasswordHash(encoded); ok {
			t.Errorf("parsePasswordHash(%q) accepted malformed input", encoded)
		}
	}
	if _, _, _, ok := parsePasswordHash(valid); !ok {
		t.Fatal("parsePasswordHash() rejected a valid hash")
	}
	for _, email := range []string{"", " owner@example.com", "owner@example.com ", "display <owner@example.com>", "owner..example@example.com", strings.Repeat("a", 247) + "@example.com"} {
		if ValidEmailForAPI(email) {
			t.Errorf("ValidEmailForAPI(%q) unexpectedly succeeded", email)
		}
	}
	for _, email := range []string{"owner@example.com", "Owner+tag@example.test"} {
		if !ValidEmailForAPI(email) {
			t.Errorf("ValidEmailForAPI(%q) unexpectedly failed", email)
		}
	}
}

func TestPostgresStoreHandlesClosedPoolAndNilInput(t *testing.T) {
	if _, err := NewPostgresStore(nil); err == nil {
		t.Fatal("NewPostgresStore(nil) unexpectedly succeeded")
	}
	pool, err := pgxpool.New(context.Background(), "postgres://unused:unused@127.0.0.1:1/speedtest_test")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	store, err := NewPostgresStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.CreateSession(ctx, "token", nil, "csrf", time.Now().Add(time.Hour)); err == nil {
		t.Fatal("CreateSession() succeeded with a closed pool")
	}
	if _, err := store.GetSession(ctx, "token", time.Now()); err == nil {
		t.Fatal("GetSession() succeeded with a closed pool")
	}
	if _, err := store.FindUserByEmail(ctx, "owner@example.com"); err == nil {
		t.Fatal("FindUserByEmail() succeeded with a closed pool")
	}
	if _, err := store.FindUserByID(ctx, "owner-id"); err == nil {
		t.Fatal("FindUserByID() succeeded with a closed pool")
	}
	if err := store.RotateAnonymousSession(ctx, "old", "user", "new", "csrf", time.Now().Add(time.Hour)); err == nil {
		t.Fatal("RotateAnonymousSession() succeeded with a closed pool")
	}
	if err := store.DeleteSession(ctx, "token"); err == nil {
		t.Fatal("DeleteSession() succeeded with a closed pool")
	}
	if err := store.ChangePassword(ctx, "user", "old", "new", "session", "rotated", "csrf", time.Now().Add(time.Hour)); err == nil {
		t.Fatal("ChangePassword() succeeded with a closed pool")
	}
	if err := store.SeedAdmin(ctx, DefaultAdminEmail, "hash"); err == nil {
		t.Fatal("SeedAdmin() succeeded with a closed pool")
	}
}
