// Package auth implements owner authentication, opaque sessions and login
// safeguards for the command center.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const (
	passwordMemoryKiB    = 19 * 1024
	passwordTime         = 2
	passwordThreads      = 1
	passwordKeyBytes     = 32
	passwordSaltBytes    = 16
	minimumPasswordRunes = 12
	maximumPasswordBytes = 1024
)

var (
	ErrPasswordPolicy = errors.New("password does not meet the password policy")
	dummySalt         = []byte("speedtest-dummy!")
	dummyHash         = []byte("not-a-valid-argon2id-hash-value")
)

// HashPassword returns a self-describing Argon2id hash. The password policy is
// applied to new user-chosen passwords; the documented local seed credential
// is intentionally handled by SeedAdmin separately.
func HashPassword(password string) (string, error) {
	if !validNewPassword(password) {
		return "", ErrPasswordPolicy
	}
	salt := make([]byte, passwordSaltBytes)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return "", errors.New("generate password salt")
	}
	return encodePasswordHash(password, salt), nil
}

func encodePasswordHash(password string, salt []byte) string {
	derived := argon2.IDKey([]byte(password), salt, passwordTime, passwordMemoryKiB, passwordThreads, passwordKeyBytes)
	return fmt.Sprintf("argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, passwordMemoryKiB, passwordTime, passwordThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(derived))
}

// VerifyPassword checks hashes created by this package and bounded compatible
// Argon2id parameters. Invalid hashes still perform one Argon2id operation so
// a missing account and an incorrect password have similar work costs.
func VerifyPassword(password, encoded string) bool {
	params, salt, expected, ok := parsePasswordHash(encoded)
	if !ok {
		performDummyPasswordWork(password)
		return false
	}
	actual := argon2.IDKey([]byte(password), salt, params.time, params.memory, params.threads, uint32(len(expected)))
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func performDummyPasswordWork(password string) {
	actual := argon2.IDKey([]byte(password), dummySalt, passwordTime, passwordMemoryKiB, passwordThreads, uint32(len(dummyHash)))
	_ = subtle.ConstantTimeCompare(actual, dummyHash)
}

type argonParams struct {
	memory  uint32
	time    uint32
	threads uint8
}

func parsePasswordHash(encoded string) (argonParams, []byte, []byte, bool) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "argon2id" {
		return argonParams{}, nil, nil, false
	}
	version, err := strconv.Atoi(strings.TrimPrefix(parts[1], "v="))
	if err != nil || version != argon2.Version {
		return argonParams{}, nil, nil, false
	}
	var params argonParams
	for _, field := range strings.Split(parts[2], ",") {
		pair := strings.SplitN(field, "=", 2)
		if len(pair) != 2 {
			return argonParams{}, nil, nil, false
		}
		value, parseErr := strconv.ParseUint(pair[1], 10, 32)
		if parseErr != nil {
			return argonParams{}, nil, nil, false
		}
		switch pair[0] {
		case "m":
			params.memory = uint32(value)
		case "t":
			params.time = uint32(value)
		case "p":
			if value > 255 {
				return argonParams{}, nil, nil, false
			}
			params.threads = uint8(value)
		default:
			return argonParams{}, nil, nil, false
		}
	}
	if params.memory < 8*1024 || params.memory > 64*1024 || params.time < 1 || params.time > 6 || params.threads < 1 || params.threads > 4 {
		return argonParams{}, nil, nil, false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(salt) < passwordSaltBytes || len(salt) > 32 {
		return argonParams{}, nil, nil, false
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(expected) < 16 || len(expected) > 64 {
		return argonParams{}, nil, nil, false
	}
	return params, salt, expected, true
}

func validNewPassword(password string) bool {
	return utf8.ValidString(password) && len(password) <= maximumPasswordBytes && utf8.RuneCountInString(password) >= minimumPasswordRunes
}
