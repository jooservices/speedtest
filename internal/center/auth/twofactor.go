package auth

import (
	"context"
	"errors"
	"strings"
)

// TwoFactor is the stored second-factor state of one account.
type TwoFactor struct {
	SecretEnc      []byte
	Enabled        bool
	LastStep       int64
	RecoveryHashes []string
}

// TwoFactorStore is the persistence boundary of the second factor.
type TwoFactorStore interface {
	// GetTwoFactor returns ErrNotFound when the account never started setup.
	GetTwoFactor(ctx context.Context, userID string) (TwoFactor, error)
	// SaveTwoFactorSetup stores a pending (not yet enabled) secret, replacing any earlier state.
	SaveTwoFactorSetup(ctx context.Context, userID string, secretEnc []byte) error
	// EnableTwoFactor turns a pending setup on, records the step of the confirming code and the recovery digests.
	EnableTwoFactor(ctx context.Context, userID string, step int64, recoveryHashes []string) error
	DeleteTwoFactor(ctx context.Context, userID string) error
	// ConsumeTwoFactorStep records a used code step; false when that step or a later one was already used.
	ConsumeTwoFactorStep(ctx context.Context, userID string, step int64) (bool, error)
	// ConsumeRecoveryCode removes one recovery digest; false when it is not stored.
	ConsumeRecoveryCode(ctx context.Context, userID, hash string) (bool, error)
}

// TwoFactorStatus is what the UI shows in Settings.
type TwoFactorStatus struct {
	Available         bool
	Enabled           bool
	RecoveryRemaining int
}

// TwoFactorSetup is returned once when setup starts; the secret is shown for manual entry.
type TwoFactorSetup struct {
	Secret string
	URI    string
	QR     string
}

// throttle limits password and code guesses on the settings actions per account, like the sign-in limiter.
func (s *Service) throttle(userID string) error {
	if allowed, wait := s.limiter.Allow("two-factor", userID, s.now()); !allowed {
		return rateLimitError{retryAfter: wait}
	}
	return nil
}

func (s *Service) available() bool { return len(s.config.AppKey) == 32 }

// identityOf returns the signed-in user of a session token.
func (s *Service) identityOf(ctx context.Context, token string) (Identity, error) {
	session, err := s.Session(ctx, token)
	if err != nil {
		return Identity{}, err
	}
	if session.Identity == nil {
		return Identity{}, ErrInvalidSession
	}
	return *session.Identity, nil
}

// TwoFactorStatus reports whether two-factor sign-in is possible and on.
func (s *Service) TwoFactorStatus(ctx context.Context, token string) (TwoFactorStatus, error) {
	who, err := s.identityOf(ctx, token)
	if err != nil {
		return TwoFactorStatus{}, err
	}
	status := TwoFactorStatus{Available: s.available()}
	record, err := s.store.GetTwoFactor(ctx, who.ID)
	if errors.Is(err, ErrNotFound) {
		return status, nil
	}
	if err != nil {
		return TwoFactorStatus{}, err
	}
	status.Enabled = record.Enabled
	if record.Enabled {
		status.RecoveryRemaining = len(record.RecoveryHashes)
	}
	return status, nil
}

// BeginTwoFactor verifies the password and stores a fresh pending secret. Calling it again replaces the pending
// secret; it fails while two-factor sign-in is already on.
func (s *Service) BeginTwoFactor(ctx context.Context, token, password string) (TwoFactorSetup, error) {
	if !s.available() {
		return TwoFactorSetup{}, ErrTwoFactorUnavailable
	}
	who, err := s.identityOf(ctx, token)
	if err != nil {
		return TwoFactorSetup{}, err
	}
	if err := s.throttle(who.ID); err != nil {
		return TwoFactorSetup{}, err
	}
	if err := s.confirmPassword(ctx, who.ID, password); err != nil {
		return TwoFactorSetup{}, err
	}
	if record, err := s.store.GetTwoFactor(ctx, who.ID); err == nil && record.Enabled {
		return TwoFactorSetup{}, ErrTwoFactorEnabled
	} else if err != nil && !errors.Is(err, ErrNotFound) {
		return TwoFactorSetup{}, err
	}
	secret, err := newTOTPSecret()
	if err != nil {
		return TwoFactorSetup{}, err
	}
	sealed, err := encryptSecret(s.config.AppKey, secret)
	if err != nil {
		return TwoFactorSetup{}, err
	}
	if err := s.store.SaveTwoFactorSetup(ctx, who.ID, sealed); err != nil {
		return TwoFactorSetup{}, err
	}
	uri := otpauthURI(TwoFactorIssuer, who.Email, secret)
	image, err := qrDataURI(uri)
	if err != nil {
		return TwoFactorSetup{}, err
	}
	return TwoFactorSetup{Secret: secret, URI: uri, QR: image}, nil
}

// EnableTwoFactor confirms the pending setup with a code from the authenticator and returns the one-time recovery
// codes. They are shown only here and stored as digests.
func (s *Service) EnableTwoFactor(ctx context.Context, token, code string) ([]string, error) {
	who, err := s.identityOf(ctx, token)
	if err != nil {
		return nil, err
	}
	if err := s.throttle(who.ID); err != nil {
		return nil, err
	}
	record, err := s.store.GetTwoFactor(ctx, who.ID)
	if errors.Is(err, ErrNotFound) {
		return nil, ErrTwoFactorNotPending
	}
	if err != nil {
		return nil, err
	}
	if record.Enabled {
		return nil, ErrTwoFactorEnabled
	}
	secret, err := decryptSecret(s.config.AppKey, record.SecretEnc)
	if err != nil {
		return nil, err
	}
	step, ok := verifyTOTP(secret, code, s.now(), record.LastStep)
	if !ok {
		return nil, ErrInvalidTwoFactorCode
	}
	codes, hashes, err := newRecoveryCodes(recoveryCodeCount)
	if err != nil {
		return nil, err
	}
	if err := s.store.EnableTwoFactor(ctx, who.ID, step, hashes); err != nil {
		return nil, err
	}
	return codes, nil
}

// DisableTwoFactor needs the password and a current code (or a recovery code) and removes the second factor.
func (s *Service) DisableTwoFactor(ctx context.Context, token, password, code string) error {
	who, err := s.identityOf(ctx, token)
	if err != nil {
		return err
	}
	if err := s.throttle(who.ID); err != nil {
		return err
	}
	if err := s.confirmPassword(ctx, who.ID, password); err != nil {
		return err
	}
	if err := s.checkSecondFactor(ctx, who.ID, code); err != nil {
		if errors.Is(err, ErrTwoFactorRequired) {
			return ErrInvalidTwoFactorCode
		}
		return err
	}
	if record, err := s.store.GetTwoFactor(ctx, who.ID); errors.Is(err, ErrNotFound) || (err == nil && !record.Enabled) {
		return ErrTwoFactorNotEnabled
	}
	return s.store.DeleteTwoFactor(ctx, who.ID)
}

func (s *Service) confirmPassword(ctx context.Context, userID, password string) error {
	credential, err := s.store.FindUserByID(ctx, userID)
	if err != nil {
		return err
	}
	if !VerifyPassword(password, credential.PasswordHash) {
		return ErrInvalidCurrentPass
	}
	return nil
}

// checkSecondFactor passes when the account has no enabled second factor or the code is valid. A used code step
// and a used recovery code can never be accepted twice.
func (s *Service) checkSecondFactor(ctx context.Context, userID, code string) error {
	record, err := s.store.GetTwoFactor(ctx, userID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !record.Enabled {
		return nil
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return ErrTwoFactorRequired
	}
	if len(code) == totpDigits {
		secret, err := decryptSecret(s.config.AppKey, record.SecretEnc)
		if err != nil {
			return err
		}
		step, ok := verifyTOTP(secret, code, s.now(), record.LastStep)
		if !ok {
			return ErrInvalidTwoFactorCode
		}
		fresh, err := s.store.ConsumeTwoFactorStep(ctx, userID, step)
		if err != nil {
			return err
		}
		if !fresh {
			return ErrInvalidTwoFactorCode
		}
		return nil
	}
	if _, ok := normalizeRecoveryCode(code); !ok {
		return ErrInvalidTwoFactorCode
	}
	used, err := s.store.ConsumeRecoveryCode(ctx, userID, hashRecoveryCode(code))
	if err != nil {
		return err
	}
	if !used {
		return ErrInvalidTwoFactorCode
	}
	return nil
}
