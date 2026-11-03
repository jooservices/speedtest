package settings

import (
	"strings"
	"testing"
)

func TestChannelValidationEdgeCases(t *testing.T) {
	empty, long, ok := "", strings.Repeat("x", 8193), "https://hooks.example.com/x"
	for name, tc := range map[string]struct {
		ch   ChannelWrite
		kind channelKind
		want string
	}{
		"email too long":         {ChannelWrite{To: strings.Repeat("a", 321) + "@example.com"}, emailChannel, ".to"},
		"email with a name":      {ChannelWrite{To: "Ops <ops@example.com>"}, emailChannel, ".to"},
		"email secret empty":     {ChannelWrite{To: "ops@example.com", Secret: &empty}, emailChannel, ".secret"},
		"telegram not a chat id": {ChannelWrite{To: "@channel"}, telegramChannel, ".to"},
		"telegram secret long":   {ChannelWrite{To: "-1001", Secret: &long}, telegramChannel, ".secret"},
		"webhook has a to":       {ChannelWrite{To: "x", Secret: &ok}, webhookChannel, ".to"},
		"webhook secret empty":   {ChannelWrite{Secret: &empty}, webhookChannel, ".secret"},
		"webhook not a url":      {ChannelWrite{Secret: strPtr("ftp://x")}, webhookChannel, ".secret"},
		"webhook url no host":    {ChannelWrite{Secret: strPtr("https://")}, webhookChannel, ".secret"},
	} {
		fields := map[string][]string{}
		validateChannel(fields, "notify.x", tc.ch, tc.kind)
		if len(fields["notify.x"+tc.want]) == 0 {
			t.Errorf("%s: no error for notify.x%s (%v)", name, tc.want, fields)
		}
	}
	fields := map[string][]string{}
	validateChannel(fields, "notify.x", ChannelWrite{Secret: strPtr(ok)}, webhookChannel)
	validateChannel(fields, "notify.x", ChannelWrite{To: "ops@example.com", Secret: strPtr("pw")}, emailChannel)
	if len(fields) != 0 {
		t.Fatalf("valid channels were rejected: %v", fields)
	}
}

func strPtr(s string) *string { return &s }

func TestCipherRejectsBadKeysAndTamperedData(t *testing.T) {
	for _, key := range []string{"", "zz", strings.Repeat("a", 62), strings.Repeat("a", 66)} {
		if _, err := NewCipher(key); err == nil {
			t.Errorf("key %q accepted", key)
		}
	}
	c, err := NewCipher(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := c.Encrypt([]byte("secret value"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), "secret value") {
		t.Fatal("ciphertext must not contain the plaintext")
	}
	if got, err := c.Decrypt(sealed); err != nil || string(got) != "secret value" {
		t.Fatalf("round trip = %q %v", got, err)
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := c.Decrypt(sealed); err == nil {
		t.Fatal("a modified ciphertext must not decrypt")
	}
	if _, err := c.Decrypt([]byte("short")); err == nil {
		t.Fatal("too short")
	}
	other, _ := NewCipher(strings.Repeat("cd", 32))
	again, _ := c.Encrypt([]byte("x"))
	if _, err := other.Decrypt(again); err == nil {
		t.Fatal("another key must not decrypt")
	}
}

func TestValidationErrorAndEnrolmentKeyFormat(t *testing.T) {
	if (&ValidationError{}).Error() == "" {
		t.Fatal("error text")
	}
	a, err := generateEnrollmentKey()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := generateEnrollmentKey()
	if a == b || len(a) < 32 || hashToken(a) == hashToken(b) || len(hashToken(a)) != 64 {
		t.Fatalf("keys must be long, unique and hashed to 64 hex characters: %q %q", a, b)
	}
}

func TestCipherFromRawKey(t *testing.T) {
	if _, err := NewCipherFromKey(make([]byte, 16)); err == nil {
		t.Fatal("a 16 byte key must be refused")
	}
	if c, err := NewCipherFromKey(make([]byte, 32)); err != nil || c == nil {
		t.Fatalf("32 byte key: %v", err)
	}
}
