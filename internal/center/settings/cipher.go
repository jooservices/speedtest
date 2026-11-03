package settings

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
)

// Cipher encrypts recoverable integration secrets with AES-256-GCM. Each
// Encrypt call uses a fresh random nonce; the ciphertext is stored as
// nonce || ciphertext and authenticated on decrypt.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher decodes the application key and prepares AES-GCM. The key must be
// 64 hexadecimal characters (32 bytes). It must come from the environment, not
// from this API, and must never be logged.
func NewCipher(appKey string) (*Cipher, error) {
	key, err := hex.DecodeString(appKey)
	if err != nil || len(key) != 32 {
		return nil, errors.New("APP_KEY must be 64 hexadecimal characters (32 bytes)")
	}
	return NewCipherFromKey(key)
}

// NewCipherFromKey prepares AES-GCM from a raw 32-byte key, as decoded from APP_KEY by the configuration.
func NewCipherFromKey(key []byte) (*Cipher, error) {
	if len(key) != 32 {
		return nil, errors.New("the application key must be exactly 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("cannot initialize AES")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("cannot initialize AES-GCM")
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt seals plaintext with a fresh random nonce, returning the nonce
// followed by the ciphertext. The result is safe to store in secret_ciphertext.
func (c *Cipher) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, errors.New("cannot generate encryption nonce")
	}
	return c.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt recovers the original plaintext from a value produced by Encrypt.
func (c *Cipher) Decrypt(ciphertext []byte) ([]byte, error) {
	size := c.aead.NonceSize()
	if len(ciphertext) < size {
		return nil, errors.New("ciphertext is too short")
	}
	nonce, sealed := ciphertext[:size], ciphertext[size:]
	plaintext, err := c.aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		return nil, errors.New("cannot decrypt secret")
	}
	return plaintext, nil
}
