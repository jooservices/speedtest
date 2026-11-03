package settings

import (
	"bytes"
	"testing"
)

func testCipher(t *testing.T) *Cipher {
	t.Helper()
	c, err := NewCipher("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	if err != nil {
		t.Fatalf("NewCipher() error = %v", err)
	}
	return c
}

func TestCipherRoundTrip(t *testing.T) {
	c := testCipher(t)
	secret := "super-secret-webhook-token"
	encrypted, err := c.Encrypt([]byte(secret))
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}
	if bytes.Contains(encrypted, []byte(secret)) {
		t.Fatal("ciphertext must not contain the plaintext")
	}
	decrypted, err := c.Decrypt(encrypted)
	if err != nil {
		t.Fatalf("Decrypt() error = %v", err)
	}
	if string(decrypted) != secret {
		t.Fatalf("Decrypt() = %q, want %q", decrypted, secret)
	}
}

func TestCipherEncryptUsesFreshNonce(t *testing.T) {
	c := testCipher(t)
	secret := []byte("same-secret")
	first, err := c.Encrypt(secret)
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}
	second, err := c.Encrypt(secret)
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("two encryptions of the same secret produced identical ciphertext")
	}
}

func TestCipherRejectsWrongKey(t *testing.T) {
	c := testCipher(t)
	encrypted, err := c.Encrypt([]byte("secret"))
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}

	other, err := NewCipher("ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100")
	if err != nil {
		t.Fatalf("NewCipher() error = %v", err)
	}
	if _, err := other.Decrypt(encrypted); err == nil {
		t.Fatal("Decrypt() with the wrong key = nil, want an error")
	}
}

func TestCipherRejectsTamperedCiphertext(t *testing.T) {
	c := testCipher(t)
	encrypted, err := c.Encrypt([]byte("secret"))
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}
	encrypted[len(encrypted)-1] ^= 0xff
	if _, err := c.Decrypt(encrypted); err == nil {
		t.Fatal("Decrypt() of tampered ciphertext = nil, want an error")
	}
}

func TestNewCipherRejectsInvalidKeys(t *testing.T) {
	for _, key := range []string{"", "short", "zz00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff", "00112233445566778899aabbccddeeff"} {
		if _, err := NewCipher(key); err == nil {
			t.Errorf("NewCipher(%q) = nil, want an error", key)
		}
	}
}

func TestCipherRejectsShortCiphertext(t *testing.T) {
	c := testCipher(t)
	if _, err := c.Decrypt([]byte{0x01}); err == nil {
		t.Fatal("Decrypt() of short ciphertext = nil, want an error")
	}
}
