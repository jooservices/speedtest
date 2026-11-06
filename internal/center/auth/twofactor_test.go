package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func (s *memoryStore) GetTwoFactor(_ context.Context, userID string) (TwoFactor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tfErr != nil {
		return TwoFactor{}, s.tfErr
	}
	record, ok := s.tf[userID]
	if !ok {
		return TwoFactor{}, ErrNotFound
	}
	return *record, nil
}

func (s *memoryStore) SaveTwoFactorSetup(_ context.Context, userID string, secretEnc []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tfErr != nil {
		return s.tfErr
	}
	if s.tf == nil {
		s.tf = map[string]*TwoFactor{}
	}
	s.tf[userID] = &TwoFactor{SecretEnc: secretEnc}
	return nil
}

func (s *memoryStore) EnableTwoFactor(_ context.Context, userID string, step int64, hashes []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tfErr != nil {
		return s.tfErr
	}
	record, ok := s.tf[userID]
	if !ok || record.Enabled {
		return ErrTwoFactorNotPending
	}
	record.Enabled, record.LastStep, record.RecoveryHashes = true, step, hashes
	return nil
}

func (s *memoryStore) DeleteTwoFactor(_ context.Context, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tfErr != nil {
		return s.tfErr
	}
	delete(s.tf, userID)
	return nil
}

func (s *memoryStore) ConsumeTwoFactorStep(_ context.Context, userID string, step int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tfErr != nil {
		return false, s.tfErr
	}
	record, ok := s.tf[userID]
	if !ok || !record.Enabled || record.LastStep >= step {
		return false, nil
	}
	record.LastStep = step
	return true, nil
}

func (s *memoryStore) ConsumeRecoveryCode(_ context.Context, userID, hash string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tfErr != nil {
		return false, s.tfErr
	}
	record, ok := s.tf[userID]
	if !ok || !record.Enabled {
		return false, nil
	}
	for i, h := range record.RecoveryHashes {
		if h == hash {
			record.RecoveryHashes = append(record.RecoveryHashes[:i:i], record.RecoveryHashes[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

const tfPassword = "a valid owner password"

func tfSetup(t *testing.T, attempts int) (*Service, *memoryStore, *time.Time, SessionView) {
	t.Helper()
	store := newMemoryStore()
	addTestUser(t, store, "owner@example.com", tfPassword)
	cfg := defaultAuthConfig()
	cfg.AppKey = []byte(strings.Repeat("k", 32))
	cfg.LoginMaxAttempts = attempts
	service := testService(t, store, cfg)
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	anonymous, err := service.Bootstrap(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.Login(context.Background(), anonymous.Token, "owner@example.com", tfPassword, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	return service, store, &now, session
}

// enableTwoFactor runs setup and enable and returns the secret and the recovery codes.
func enableTwoFactor(t *testing.T, service *Service, now *time.Time, token string) (string, []string) {
	t.Helper()
	setup, err := service.BeginTwoFactor(context.Background(), token, tfPassword)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := secretEncoding.DecodeString(setup.Secret)
	codes, err := service.EnableTwoFactor(context.Background(), token, totpAt(raw, now.Unix()/totpPeriod))
	if err != nil {
		t.Fatal(err)
	}
	return setup.Secret, codes
}

func TestTOTPMatchesTheRFC6238Vectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	for unix, want := range map[int64]string{59: "287082", 1111111109: "081804", 1111111111: "050471", 1234567890: "005924", 2000000000: "279037"} {
		if got := totpAt(secret, unix/30); got != want {
			t.Errorf("totpAt(%d) = %s, want %s", unix, got, want)
		}
	}
}

func TestVerifyTOTPWindowReplayAndMalformedInput(t *testing.T) {
	secretB32, err := newTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := secretEncoding.DecodeString(secretB32)
	now := time.Unix(1_800_000_000, 0)
	step := now.Unix() / totpPeriod
	if got, ok := verifyTOTP(secretB32, totpAt(raw, step), now, 0); !ok || got != step {
		t.Fatalf("current step: %d %v", got, ok)
	}
	for _, offset := range []int64{-1, 1} {
		if _, ok := verifyTOTP(secretB32, totpAt(raw, step+offset), now, 0); !ok {
			t.Errorf("neighbour step %d refused", offset)
		}
	}
	if _, ok := verifyTOTP(secretB32, totpAt(raw, step+2), now, 0); ok {
		t.Error("a step outside the window was accepted")
	}
	if _, ok := verifyTOTP(secretB32, totpAt(raw, step), now, step); ok {
		t.Error("a replayed step was accepted")
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef"} {
		if _, ok := verifyTOTP(secretB32, bad, now, 0); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, ok := verifyTOTP("!!!", "123456", now, 0); ok {
		t.Error("an invalid secret was accepted")
	}
}

func TestProvisioningHelpers(t *testing.T) {
	uri := otpauthURI("Speedtest", "owner@example.com", "ABCDEFGH")
	if !strings.HasPrefix(uri, "otpauth://totp/Speedtest:owner@example.com?") || !strings.Contains(uri, "secret=ABCDEFGH") || !strings.Contains(uri, "issuer=Speedtest") {
		t.Fatalf("uri = %s", uri)
	}
	image, err := qrDataURI(uri)
	if err != nil || !strings.HasPrefix(image, "data:image/png;base64,") || len(image) < 200 {
		t.Fatalf("qr = %.40s %v", image, err)
	}
	if _, err := qrDataURI(strings.Repeat("x", 8000)); err == nil {
		t.Fatal("expected an oversize error")
	}
	codes, hashes, err := newRecoveryCodes(3)
	if err != nil || len(codes) != 3 || len(hashes) != 3 {
		t.Fatal(err)
	}
	if len(codes[0]) != 11 || codes[0][5] != '-' || hashRecoveryCode(codes[0]) != hashes[0] || hashRecoveryCode(strings.ToUpper(codes[0])) != hashes[0] {
		t.Fatalf("code format %q", codes[0])
	}
	if _, ok := normalizeRecoveryCode("short"); ok {
		t.Fatal("a short code was valid")
	}
}

func TestSecretEncryption(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	sealed, err := encryptSecret(key, "JBSWY3DPEHPK3PXP")
	if err != nil || strings.Contains(string(sealed), "JBSWY3DP") {
		t.Fatalf("seal: %v", err)
	}
	if plain, err := decryptSecret(key, sealed); err != nil || plain != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("open: %q %v", plain, err)
	}
	if _, err := decryptSecret([]byte(strings.Repeat("x", 32)), sealed); err == nil {
		t.Fatal("a wrong key opened the secret")
	}
	for _, bad := range [][]byte{nil, []byte("AAAA"), []byte(strings.Repeat("z", 40))} {
		if _, err := decryptSecret(key, bad); err == nil {
			t.Errorf("%q opened", bad)
		}
	}
	if _, err := encryptSecret([]byte("short"), "x"); !errors.Is(err, ErrTwoFactorUnavailable) {
		t.Fatalf("short key: %v", err)
	}
	if _, err := decryptSecret(nil, sealed); !errors.Is(err, ErrTwoFactorUnavailable) {
		t.Fatalf("no key: %v", err)
	}
}

func TestTwoFactorLifecycleAndSignIn(t *testing.T) {
	service, store, now, session := tfSetup(t, 100)
	ctx := context.Background()
	if st, err := service.TwoFactorStatus(ctx, session.Token); err != nil || !st.Available || st.Enabled {
		t.Fatalf("initial status = %+v, %v", st, err)
	}
	if _, err := service.EnableTwoFactor(ctx, session.Token, "123456"); !errors.Is(err, ErrTwoFactorNotPending) {
		t.Fatalf("enable before setup = %v", err)
	}
	if _, err := service.BeginTwoFactor(ctx, session.Token, "wrong password"); !errors.Is(err, ErrInvalidCurrentPass) {
		t.Fatalf("setup with a wrong password = %v", err)
	}
	setup, err := service.BeginTwoFactor(ctx, session.Token, tfPassword)
	if err != nil || setup.Secret == "" || setup.URI == "" || !strings.HasPrefix(setup.QR, "data:image/png") {
		t.Fatalf("setup = %+v, %v", setup, err)
	}
	if strings.Contains(string(store.tf["user-id"].SecretEnc), setup.Secret) {
		t.Fatal("the secret is stored in clear")
	}
	if _, err := service.EnableTwoFactor(ctx, session.Token, "000000"); !errors.Is(err, ErrInvalidTwoFactorCode) {
		t.Fatalf("enable with a wrong code = %v", err)
	}
	secret, codes := enableTwoFactor(t, service, now, session.Token)
	if len(codes) != 10 {
		t.Fatalf("%d recovery codes", len(codes))
	}
	if st, _ := service.TwoFactorStatus(ctx, session.Token); !st.Enabled || st.RecoveryRemaining != 10 {
		t.Fatalf("status after enable = %+v", st)
	}
	if _, err := service.BeginTwoFactor(ctx, session.Token, tfPassword); !errors.Is(err, ErrTwoFactorEnabled) {
		t.Fatalf("setup while enabled = %v", err)
	}
	if _, err := service.EnableTwoFactor(ctx, session.Token, "123456"); !errors.Is(err, ErrTwoFactorEnabled) {
		t.Fatalf("enable twice = %v", err)
	}

	raw, _ := secretEncoding.DecodeString(secret)
	signIn := func(code string) error {
		anonymous, _ := service.Bootstrap(ctx)
		_, err := service.LoginWithCode(ctx, anonymous.Token, "owner@example.com", tfPassword, code, "192.0.2.9")
		return err
	}
	if err := signIn(""); !errors.Is(err, ErrTwoFactorRequired) {
		t.Fatalf("no code = %v", err)
	}
	if err := signIn("000000"); !errors.Is(err, ErrInvalidTwoFactorCode) {
		t.Fatalf("wrong code = %v", err)
	}
	// The step used to enable is spent: the same code cannot sign in, the next step can.
	if err := signIn(totpAt(raw, now.Unix()/totpPeriod)); !errors.Is(err, ErrInvalidTwoFactorCode) {
		t.Fatalf("replayed code = %v", err)
	}
	*now = now.Add(time.Duration(totpPeriod) * time.Second)
	good := totpAt(raw, now.Unix()/totpPeriod)
	if err := signIn(good); err != nil {
		t.Fatalf("valid code = %v", err)
	}
	if err := signIn(good); !errors.Is(err, ErrInvalidTwoFactorCode) {
		t.Fatalf("code used twice = %v", err)
	}
	// A recovery code works once, in any case and with or without the dash.
	if err := signIn(strings.ToUpper(codes[0])); err != nil {
		t.Fatalf("recovery code = %v", err)
	}
	if err := signIn(codes[0]); !errors.Is(err, ErrInvalidTwoFactorCode) {
		t.Fatalf("recovery code reused = %v", err)
	}
	if err := signIn("not-a-code-at-all"); !errors.Is(err, ErrInvalidTwoFactorCode) {
		t.Fatalf("garbage code = %v", err)
	}
	if st, _ := service.TwoFactorStatus(ctx, session.Token); st.RecoveryRemaining != 9 {
		t.Fatalf("recovery codes left = %d", st.RecoveryRemaining)
	}

	// Disabling needs the password and a code.
	if err := service.DisableTwoFactor(ctx, session.Token, "wrong password", codes[1]); !errors.Is(err, ErrInvalidCurrentPass) {
		t.Fatalf("disable with a wrong password = %v", err)
	}
	if err := service.DisableTwoFactor(ctx, session.Token, tfPassword, ""); !errors.Is(err, ErrInvalidTwoFactorCode) {
		t.Fatalf("disable without a code = %v", err)
	}
	if err := service.DisableTwoFactor(ctx, session.Token, tfPassword, codes[1]); err != nil {
		t.Fatalf("disable = %v", err)
	}
	if err := service.DisableTwoFactor(ctx, session.Token, tfPassword, codes[2]); !errors.Is(err, ErrTwoFactorNotEnabled) {
		t.Fatalf("disable twice = %v", err)
	}
	if err := signIn(""); err != nil {
		t.Fatalf("sign-in after disable = %v", err)
	}
}

func TestTwoFactorUnavailableWithoutAppKeyAndInvalidSessions(t *testing.T) {
	store := newMemoryStore()
	addTestUser(t, store, "owner@example.com", tfPassword)
	service := testService(t, store, defaultAuthConfig())
	ctx := context.Background()
	anonymous, _ := service.Bootstrap(ctx)
	session, err := service.Login(ctx, anonymous.Token, "owner@example.com", tfPassword, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	if st, err := service.TwoFactorStatus(ctx, session.Token); err != nil || st.Available {
		t.Fatalf("status = %+v, %v", st, err)
	}
	if _, err := service.BeginTwoFactor(ctx, session.Token, tfPassword); !errors.Is(err, ErrTwoFactorUnavailable) {
		t.Fatalf("setup = %v", err)
	}
	for name, call := range map[string]func() error{
		"status":  func() error { _, err := service.TwoFactorStatus(ctx, "nope"); return err },
		"begin":   func() error { _, err := service.BeginTwoFactor(ctx, "nope", tfPassword); return err },
		"enable":  func() error { _, err := service.EnableTwoFactor(ctx, "nope", "123456"); return err },
		"disable": func() error { return service.DisableTwoFactor(ctx, "nope", tfPassword, "123456") },
	} {
		if err := call(); !errors.Is(err, ErrInvalidSession) && !errors.Is(err, ErrTwoFactorUnavailable) {
			t.Errorf("%s with an invalid session = %v", name, err)
		}
	}
	// An anonymous session has no identity.
	if _, err := service.TwoFactorStatus(ctx, anonymous.Token); err == nil {
		t.Fatal("anonymous status should fail")
	}
}

func TestTwoFactorStoreErrorsAndThrottle(t *testing.T) {
	service, store, _, session := tfSetup(t, 5)
	ctx := context.Background()
	boom := errors.New("store down")
	store.tfErr = boom
	if _, err := service.TwoFactorStatus(ctx, session.Token); !errors.Is(err, boom) {
		t.Errorf("status = %v", err)
	}
	if _, err := service.BeginTwoFactor(ctx, session.Token, tfPassword); !errors.Is(err, boom) {
		t.Errorf("begin = %v", err)
	}
	if _, err := service.EnableTwoFactor(ctx, session.Token, "123456"); !errors.Is(err, boom) {
		t.Errorf("enable = %v", err)
	}
	if err := service.DisableTwoFactor(ctx, session.Token, tfPassword, "123456"); !errors.Is(err, boom) {
		t.Errorf("disable = %v", err)
	}
	anonymous, _ := service.Bootstrap(ctx)
	if _, err := service.LoginWithCode(ctx, anonymous.Token, "owner@example.com", tfPassword, "", "192.0.2.3"); !errors.Is(err, boom) {
		t.Errorf("login = %v", err)
	}
	store.tfErr = nil
	// A stored secret that cannot be opened (APP_KEY changed) is an error, not a bypass.
	store.tf = map[string]*TwoFactor{"user-id": {SecretEnc: []byte("garbage-garbage-garbage-garbage-garbage"), Enabled: true}}
	if _, err := service.LoginWithCode(ctx, anonymous.Token, "owner@example.com", tfPassword, "123456", "192.0.2.4"); err == nil || errors.Is(err, ErrInvalidTwoFactorCode) {
		t.Errorf("unopenable secret = %v", err)
	}
	if _, err := service.EnableTwoFactor(ctx, session.Token, "123456"); err == nil {
		t.Error("enable with an unopenable pending secret should fail")
	}
	store.tf = nil
	// The settings actions share a per-account attempt limit.
	var limited error
	for i := 0; i < 20 && limited == nil; i++ {
		_, limited = service.BeginTwoFactor(ctx, session.Token, "wrong password")
		if errors.Is(limited, ErrInvalidCurrentPass) {
			limited = nil
		}
	}
	if !errors.Is(limited, ErrRateLimited) {
		t.Fatalf("no rate limit: %v", limited)
	}
	if err := service.DisableTwoFactor(ctx, session.Token, tfPassword, ""); !errors.Is(err, ErrRateLimited) {
		t.Errorf("disable should be limited too: %v", err)
	}
	if _, err := service.EnableTwoFactor(ctx, session.Token, "123456"); !errors.Is(err, ErrRateLimited) {
		t.Errorf("enable should be limited too: %v", err)
	}
}
