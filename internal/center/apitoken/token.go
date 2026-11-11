// Package apitoken issues and verifies owner API bearer tokens.
package apitoken

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const tokenPrefix = "st_at_"

// Scope is the permission tier granted to an API token.
type Scope string

const (
	ReadOnly  Scope = "read"
	ReadWrite Scope = "read-write"
	Full      Scope = "full"
)

// Token is safe to return to the owner; it never contains the bearer value or digest.
type Token struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Scope      Scope      `json:"scope"`
	CreatedAt  time.Time  `json:"createdAt"`
	ExpiresAt  *time.Time `json:"expiresAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
	RevokedAt  *time.Time `json:"revokedAt"`
}

// Principal is the identity authenticated by an API token.
type Principal struct {
	TokenID string
	UserID  string
	Name    string
	Email   string
	Scope   Scope
}

// CreateInput describes a token without accepting any credential material from the caller.
type CreateInput struct {
	Name      string     `json:"name"`
	Scope     Scope      `json:"scope"`
	ExpiresAt *time.Time `json:"expiresAt"`
}

type contextKey struct{}

// WithPrincipal stores verified token metadata for the duration of a request.
func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, contextKey{}, principal)
}

// PrincipalFromContext returns token metadata installed by the authentication middleware.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(contextKey{}).(Principal)
	return principal, ok && principal.TokenID != "" && principal.UserID != ""
}

func validateInput(input CreateInput, now time.Time) CreateInput {
	input.Name = strings.TrimSpace(input.Name)
	if input.ExpiresAt != nil {
		value := input.ExpiresAt.UTC()
		input.ExpiresAt = &value
	}
	return input
}

// ValidInput reports whether a token request has a bounded name, a known scope, and a future expiry.
func ValidInput(input CreateInput, now time.Time) bool {
	input = validateInput(input, now)
	if input.Name == "" || utf8.RuneCountInString(input.Name) > 80 || input.Scope != ReadOnly && input.Scope != ReadWrite && input.Scope != Full {
		return false
	}
	return input.ExpiresAt == nil || input.ExpiresAt.After(now)
}

func normalizedInput(input CreateInput, now time.Time) (CreateInput, bool) {
	input = validateInput(input, now)
	return input, ValidInput(input, now)
}

func issue() (string, string, [32]byte, error) {
	id := uuid.NewString()
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", "", [32]byte{}, err
	}
	raw := tokenPrefix + id + "." + base64.RawURLEncoding.EncodeToString(secret)
	return id, raw, digest(raw), nil
}

func parse(raw string) (string, bool) {
	if !strings.HasPrefix(raw, tokenPrefix) {
		return "", false
	}
	parts := strings.Split(strings.TrimPrefix(raw, tokenPrefix), ".")
	if len(parts) != 2 || len(parts[1]) != 43 {
		return "", false
	}
	if _, err := uuid.Parse(parts[0]); err != nil {
		return "", false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(parts[1])
	return parts[0], err == nil && len(decoded) == 32
}

func digest(raw string) [32]byte { return sha256.Sum256([]byte(raw)) }

func matches(stored []byte, raw string) bool {
	actual := digest(raw)
	var expected [32]byte
	if len(stored) == len(expected) {
		copy(expected[:], stored)
	}
	return subtle.ConstantTimeCompare(expected[:], actual[:]) == 1 && len(stored) == len(expected)
}
