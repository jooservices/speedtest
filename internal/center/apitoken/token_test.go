package apitoken

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestValidInputTrimsNameAndChecksBoundaries(t *testing.T) {
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name  string
		input CreateInput
		valid bool
	}{
		{"valid", CreateInput{Name: "  e2e token  ", Scope: ReadOnly}, true},
		{"unicode length", CreateInput{Name: strings.Repeat("界", 80), Scope: Full}, true},
		{"empty", CreateInput{Name: "  ", Scope: ReadOnly}, false},
		{"too long", CreateInput{Name: strings.Repeat("a", 81), Scope: ReadOnly}, false},
		{"unknown scope", CreateInput{Name: "tool", Scope: "admin"}, false},
		{"expiry now", CreateInput{Name: "tool", Scope: ReadOnly, ExpiresAt: &now}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ValidInput(test.input, now); got != test.valid {
				t.Fatalf("ValidInput() = %t, want %t", got, test.valid)
			}
		})
	}
}

func TestIssueParseAndConstantTimeDigestMatch(t *testing.T) {
	id, raw, hash, err := issue()
	if err != nil {
		t.Fatal(err)
	}
	parsed, ok := parse(raw)
	if !ok || parsed != id {
		t.Fatalf("parse(%q) = (%q, %t), want token id %q", raw[:10]+"…", parsed, ok, id)
	}
	if !matches(hash[:], raw) || matches(hash[:], raw+"x") || matches(nil, raw) {
		t.Fatal("digest matching accepted an invalid digest or credential")
	}
	for _, invalid := range []string{"", "st_at_bad", "st_at_" + id + ".short", "st_at_" + id + "." + strings.Repeat("!", 43)} {
		if _, ok := parse(invalid); ok {
			t.Errorf("parse(%q) unexpectedly succeeded", invalid)
		}
	}
}

func TestPrincipalContextRequiresCompleteIdentity(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{TokenID: "token", UserID: "user", Scope: ReadWrite})
	if got, ok := PrincipalFromContext(ctx); !ok || got.Scope != ReadWrite {
		t.Fatalf("PrincipalFromContext() = (%+v, %t)", got, ok)
	}
	if _, ok := PrincipalFromContext(context.Background()); ok {
		t.Fatal("empty context returned a principal")
	}
	if _, ok := PrincipalFromContext(WithPrincipal(context.Background(), Principal{TokenID: "token"})); ok {
		t.Fatal("incomplete principal was accepted")
	}
}
