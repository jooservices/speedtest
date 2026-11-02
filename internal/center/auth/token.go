package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
)

func newOpaqueToken() (string, error) {
	var raw [32]byte
	if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
		return "", errors.New("generate session token")
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func newRandomBytes(value []byte) (int, error) {
	return io.ReadFull(rand.Reader, value)
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// csrfForSession derives a per-session synchronizer token. The browser cannot
// read the HttpOnly session cookie; the server can re-issue this token on GET
// /me without storing its plaintext.
func csrfForSession(sessionToken string) string {
	mac := hmac.New(sha256.New, []byte(sessionToken))
	_, _ = mac.Write([]byte("speedtest-session-csrf-v1"))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func tokenMatches(presented, expectedHash string) bool {
	actual := tokenHash(presented)
	if len(actual) != len(expectedHash) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(actual), []byte(expectedHash)) == 1
}

// MatchCSRF compares a presented token with the synchronizer token for the
// current session in constant time.
func MatchCSRF(presented, expected string) bool {
	if len(presented) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) == 1
}
