package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 authenticator apps use HMAC-SHA1.
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"rsc.io/qr"
)

// TOTP parameters of RFC 6238 as every authenticator app expects them.
const (
	totpPeriod        = 30 // seconds
	totpDigits        = 6
	totpSkew          = 1 // accept one step either side of now
	recoveryCodeCount = 10
	// TwoFactorIssuer is the account label shown in the authenticator app.
	TwoFactorIssuer = "Speedtest"
)

var secretEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// newTOTPSecret returns a fresh 160-bit secret in base32.
func newTOTPSecret() (string, error) {
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return secretEncoding.EncodeToString(raw), nil
}

// totpAt computes the code of one time step.
func totpAt(secret []byte, step int64) string {
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step)) //nolint:gosec // step is a positive Unix time quotient.
	mac := hmac.New(sha1.New, secret)
	_, _ = mac.Write(counter[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, value%1_000_000)
}

// verifyTOTP accepts a code of the current step or its neighbours and returns the matching step. A step at or below
// lastStep is refused so a code cannot be replayed.
func verifyTOTP(secretB32, code string, now time.Time, lastStep int64) (int64, bool) {
	code = strings.TrimSpace(code)
	if len(code) != totpDigits {
		return 0, false
	}
	secret, err := secretEncoding.DecodeString(strings.ToUpper(secretB32))
	if err != nil {
		return 0, false
	}
	current := now.Unix() / totpPeriod
	matched := int64(0)
	for step := current - totpSkew; step <= current+totpSkew; step++ {
		if subtle.ConstantTimeCompare([]byte(totpAt(secret, step)), []byte(code)) == 1 && step > lastStep && step > matched {
			matched = step
		}
	}
	return matched, matched != 0
}

// otpauthURI is the provisioning URI authenticator apps read from a QR code or a tapped link.
func otpauthURI(issuer, account, secretB32 string) string {
	label := url.PathEscape(issuer + ":" + account)
	query := url.Values{"secret": {secretB32}, "issuer": {issuer}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	return "otpauth://totp/" + label + "?" + query.Encode()
}

// qrDataURI renders the provisioning URI as a PNG data URI (the UI allows data: images).
func qrDataURI(content string) (string, error) {
	code, err := qr.Encode(content, qr.M)
	if err != nil {
		return "", err
	}
	code.Scale = 6
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(code.PNG()), nil
}

// newRecoveryCodes returns n single-use codes (shown once) and their stored digests.
func newRecoveryCodes(n int) (codes, hashes []string, err error) {
	for i := 0; i < n; i++ {
		raw := make([]byte, 6)
		if _, err := rand.Read(raw); err != nil {
			return nil, nil, err
		}
		text := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)) // 10 characters
		code := text[:5] + "-" + text[5:]
		codes = append(codes, code)
		hashes = append(hashes, hashRecoveryCode(code))
	}
	return codes, hashes, nil
}

// normalizeRecoveryCode lowercases and removes separators; ok is false when the result cannot be a recovery code.
func normalizeRecoveryCode(code string) (string, bool) {
	code = strings.ToLower(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(code)))
	return code, len(code) == 10
}

func hashRecoveryCode(code string) string {
	normalized, _ := normalizeRecoveryCode(code)
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

// encryptSecret seals the secret with AES-256-GCM; the result is nonce then ciphertext, the layout rotate-app-key handles.
func encryptSecret(key []byte, plain string) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(plain), nil), nil
}

func decryptSecret(key, sealed []byte) (string, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	if len(sealed) < gcm.NonceSize() {
		return "", errors.New("invalid two-factor secret")
	}
	plain, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
	if err != nil {
		return "", errors.New("invalid two-factor secret")
	}
	return string(plain), nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, ErrTwoFactorUnavailable
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
