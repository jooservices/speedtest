package auth

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
)

const (
	DefaultAdminEmail    = "jooservices@gmail.com"
	DefaultAdminPassword = "password"
)

var (
	ErrNotFound           = errors.New("record not found")
	ErrInvalidSession     = errors.New("invalid session")
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrInvalidCurrentPass = errors.New("current password is incorrect")
	ErrSessionConflict    = errors.New("session state conflict")
	ErrRateLimited        = errors.New("login rate limit exceeded")
)

// Identity is the public user information returned by auth/me and login.
type Identity struct {
	ID    string
	Email string
}

// Credential contains a user's password hash and must never be serialized or
// logged. Identity is safe for API responses.
type Credential struct {
	Identity
	PasswordHash string
}

// SessionRecord is the database representation needed to validate a session.
type SessionRecord struct {
	Identity      *Identity
	CSRFTokenHash string
	ExpiresAt     time.Time
}

// SessionView is safe to return to the UI. Token must only be placed in the
// HttpOnly cookie; CSRFToken is returned in the API response body.
type SessionView struct {
	Token     string
	Identity  *Identity
	CSRFToken string
	ExpiresAt time.Time
}

// Store is the persistence boundary for auth. Implementations store hashes of
// bearer tokens and use transactions when rotating authenticated sessions.
type Store interface {
	CreateSession(context.Context, string, *string, string, time.Time) error
	GetSession(context.Context, string, time.Time) (SessionRecord, error)
	FindUserByEmail(context.Context, string) (Credential, error)
	FindUserByID(context.Context, string) (Credential, error)
	RotateAnonymousSession(context.Context, string, string, string, string, time.Time) error
	DeleteSession(context.Context, string) error
	ChangePassword(context.Context, string, string, string, string, string, string, time.Time) error
	SeedAdmin(context.Context, string, string) error
}

// Config controls session lifetime and login rate limits.
type Config struct {
	SessionTTL       time.Duration
	LoginMaxAttempts int
	LoginWindow      time.Duration
}

// Service implements password and session operations over a Store.
type Service struct {
	store   Store
	config  Config
	limiter *RateLimiter
	now     func() time.Time
}

func NewService(store Store, config Config) (*Service, error) {
	if store == nil {
		return nil, errors.New("auth store is required")
	}
	if config.SessionTTL <= 0 || config.LoginMaxAttempts < 1 || config.LoginWindow <= 0 {
		return nil, errors.New("invalid auth configuration")
	}
	return &Service{
		store:   store,
		config:  config,
		limiter: NewRateLimiter(config.LoginMaxAttempts, config.LoginWindow),
		now:     time.Now,
	}, nil
}

// Bootstrap creates a fresh anonymous session and CSRF token.
func (s *Service) Bootstrap(ctx context.Context) (SessionView, error) {
	token, err := newOpaqueToken()
	if err != nil {
		return SessionView{}, err
	}
	csrf := csrfForSession(token)
	expires := s.now().UTC().Add(s.config.SessionTTL)
	if err := s.store.CreateSession(ctx, tokenHash(token), nil, tokenHash(csrf), expires); err != nil {
		return SessionView{}, err
	}
	return SessionView{Token: token, CSRFToken: csrf, ExpiresAt: expires}, nil
}

// Session validates an opaque cookie and returns the matching identity and
// deterministic per-session CSRF token. The database stores only token hashes.
func (s *Service) Session(ctx context.Context, token string) (SessionView, error) {
	if token == "" {
		return SessionView{}, ErrInvalidSession
	}
	record, err := s.store.GetSession(ctx, tokenHash(token), s.now().UTC())
	if err != nil {
		return SessionView{}, err
	}
	csrf := csrfForSession(token)
	if !tokenMatches(csrf, record.CSRFTokenHash) {
		return SessionView{}, ErrInvalidSession
	}
	return SessionView{Token: token, Identity: record.Identity, CSRFToken: csrf, ExpiresAt: record.ExpiresAt}, nil
}

// Login consumes an anonymous session and creates a new authenticated session.
func (s *Service) Login(ctx context.Context, currentToken, email, password, clientIP string) (SessionView, error) {
	current, err := s.Session(ctx, currentToken)
	if err != nil {
		return SessionView{}, err
	}
	if current.Identity != nil {
		return SessionView{}, ErrSessionConflict
	}
	identity := normalizeEmail(email)
	if allowed, wait := s.limiter.Allow(clientIP, identity, s.now()); !allowed {
		return SessionView{}, rateLimitError{retryAfter: wait}
	}
	credential, err := s.store.FindUserByEmail(ctx, identity)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			performDummyPasswordWork(password)
			return SessionView{}, ErrInvalidCredentials
		}
		return SessionView{}, err
	}
	if !VerifyPassword(password, credential.PasswordHash) {
		return SessionView{}, ErrInvalidCredentials
	}
	newToken, err := newOpaqueToken()
	if err != nil {
		return SessionView{}, err
	}
	csrf := csrfForSession(newToken)
	expires := s.now().UTC().Add(s.config.SessionTTL)
	if err := s.store.RotateAnonymousSession(ctx, tokenHash(currentToken), credential.ID, tokenHash(newToken), tokenHash(csrf), expires); err != nil {
		return SessionView{}, err
	}
	return SessionView{Token: newToken, Identity: &credential.Identity, CSRFToken: csrf, ExpiresAt: expires}, nil
}

// Logout deletes the current server-side session.
func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return ErrInvalidSession
	}
	return s.store.DeleteSession(ctx, tokenHash(token))
}

// ChangePassword verifies the current password, updates its Argon2id hash,
// invalidates other sessions and rotates the caller's current session.
func (s *Service) ChangePassword(ctx context.Context, currentToken, currentPassword, newPassword string) (SessionView, error) {
	current, err := s.Session(ctx, currentToken)
	if err != nil {
		return SessionView{}, err
	}
	if current.Identity == nil {
		return SessionView{}, ErrInvalidSession
	}
	credential, err := s.store.FindUserByID(ctx, current.Identity.ID)
	if err != nil {
		return SessionView{}, err
	}
	if !VerifyPassword(currentPassword, credential.PasswordHash) {
		return SessionView{}, ErrInvalidCurrentPass
	}
	if !validNewPassword(newPassword) {
		return SessionView{}, ErrPasswordPolicy
	}
	passwordHash, err := HashPassword(newPassword)
	if err != nil {
		return SessionView{}, err
	}
	newToken, err := newOpaqueToken()
	if err != nil {
		return SessionView{}, err
	}
	csrf := csrfForSession(newToken)
	expires := s.now().UTC().Add(s.config.SessionTTL)
	if err := s.store.ChangePassword(ctx, credential.ID, credential.PasswordHash, passwordHash,
		tokenHash(currentToken), tokenHash(newToken), tokenHash(csrf), expires); err != nil {
		return SessionView{}, err
	}
	return SessionView{Token: newToken, Identity: &credential.Identity, CSRFToken: csrf, ExpiresAt: expires}, nil
}

// EnsureDefaultAdmin idempotently inserts the documented local/test owner.
func (s *Service) EnsureDefaultAdmin(ctx context.Context) error {
	hash, err := hashSeedPassword(DefaultAdminPassword)
	if err != nil {
		return fmt.Errorf("hash local administrator password: %w", err)
	}
	return s.store.SeedAdmin(ctx, DefaultAdminEmail, hash)
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func validEmail(email string) bool {
	if len(email) == 0 || len(email) > 254 || strings.TrimSpace(email) != email {
		return false
	}
	parsed, err := mail.ParseAddress(email)
	return err == nil && parsed.Address == email && strings.Contains(email, "@")
}

// ValidEmailForAPI validates a single mailbox address without display-name
// syntax and within the storage/API length bound.
func ValidEmailForAPI(email string) bool { return validEmail(email) }

type rateLimitError struct {
	retryAfter time.Duration
}

func (e rateLimitError) Error() string             { return ErrRateLimited.Error() }
func (e rateLimitError) Unwrap() error             { return ErrRateLimited }
func (e rateLimitError) RetryAfter() time.Duration { return e.retryAfter }

func hashSeedPassword(password string) (string, error) {
	salt := make([]byte, passwordSaltBytes)
	if _, err := newRandomBytes(salt); err != nil {
		return "", err
	}
	return encodePasswordHash(password, salt), nil
}
