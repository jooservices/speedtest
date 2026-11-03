package installer

import (
	"errors"
	"strings"
	"testing"
)

func TestProbeInputNormalize(t *testing.T) {
	tests := []struct {
		name string
		in   ProbeInput
		want string
		bad  bool
	}{
		{name: "trims host", in: ProbeInput{Host: " center.example.test ", Port: 22}, want: "center.example.test"},
		{name: "IPv4", in: ProbeInput{Host: "192.0.2.5", Port: 2200}, want: "192.0.2.5"},
		{name: "IPv6", in: ProbeInput{Host: "2001:db8::1", Port: 22}, want: "2001:db8::1"},
		{name: "empty", in: ProbeInput{Port: 22}, bad: true},
		{name: "invalid hostname", in: ProbeInput{Host: "bad host", Port: 22}, bad: true},
		{name: "bad port low", in: ProbeInput{Host: "node.example.test", Port: 0}, bad: true},
		{name: "bad port high", in: ProbeInput{Host: "node.example.test", Port: 65536}, bad: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.in.normalize()
			if test.bad {
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("normalize() error = %v, want ErrInvalid", err)
				}
				return
			}
			if err != nil || got.Host != test.want {
				t.Fatalf("normalize() = %+v, %v, want host %q", got, err, test.want)
			}
		})
	}
}

func TestSSHInputNormalize(t *testing.T) {
	valid := SSHInput{Host: "node.example.test", Port: 22, User: "agent_admin", Auth: "password", Secret: "ssh-secret"}
	if got, err := valid.normalize(); err != nil || got.Host != valid.Host {
		t.Fatalf("normalize(valid) = %+v, %v", got, err)
	}
	for name, input := range map[string]SSHInput{
		"bad username":     {Host: valid.Host, Port: 22, User: "bad user", Auth: "password", Secret: "secret"},
		"missing secret":   {Host: valid.Host, Port: 22, User: "root", Auth: "password"},
		"unsupported auth": {Host: valid.Host, Port: 22, User: "root", Auth: "keyboard", Secret: "secret"},
		"password NUL":     {Host: valid.Host, Port: 22, User: "root", Auth: "password", Secret: "bad\x00secret"},
		"sudo newline":     {Host: valid.Host, Port: 22, User: "root", Auth: "key", Secret: "key", SudoPassword: "bad\npassword"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := input.normalize(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("normalize() error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestFingerprintValidation(t *testing.T) {
	if !validFingerprint("SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA") {
		t.Fatal("valid SHA-256 fingerprint was rejected")
	}
	for _, value := range []string{"", "MD5:abc", "SHA256:short", "SHA256:!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!"} {
		if validFingerprint(value) {
			t.Errorf("validFingerprint(%q) = true", value)
		}
	}
}

func TestKeyedHashJSONProtectsCredentialRequests(t *testing.T) {
	input := InstallInput{
		ProbeID:     "00000000-0000-4000-8000-000000000001",
		Fingerprint: "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		SSH:         SSHInput{Host: "node.example.test", Port: 22, User: "root", Auth: "password", Secret: "low-entropy-password"},
	}
	one, err := keyedHashJSON([]byte("0123456789abcdef0123456789abcdef"), input)
	if err != nil {
		t.Fatal(err)
	}
	two, err := keyedHashJSON([]byte("fedcba9876543210fedcba9876543210"), input)
	if err != nil {
		t.Fatal(err)
	}
	changed := input
	changed.SSH.Secret = "different-password"
	three, err := keyedHashJSON([]byte("0123456789abcdef0123456789abcdef"), changed)
	if err != nil {
		t.Fatal(err)
	}
	if one == two || one == three {
		t.Fatal("the keyed request digest did not vary with its key or request")
	}
	if strings.Contains(one, input.SSH.Secret) || strings.Contains(one, "node.example.test") {
		t.Fatal("request digest contains request content")
	}
}

func TestProgressParsingAndFailureMessages(t *testing.T) {
	if stage, pct, ok := parseProgress("STP|verify|35"); !ok || stage != "verify" || pct != 35 {
		t.Fatalf("parseProgress() = %q, %v, %v", stage, pct, ok)
	}
	for _, line := range []string{"", "STP|unknown|1", "STP|verify|101", "STP|verify|nope", "STP|verify|NaN", "STP|verify|+Inf", "noise"} {
		if _, _, ok := parseProgress(line); ok {
			t.Errorf("parseProgress(%q) accepted invalid input", line)
		}
	}
	for _, stage := range []string{"connect", "detect", "download", "verify", "user", "enroll", "start", "heartbeat", "cleanup", "other"} {
		if generatedMessage(stage) == "" {
			t.Errorf("generatedMessage(%q) is empty", stage)
		}
	}
	for _, err := range []error{ErrHostKeyChange, ErrAuth, ErrSudo, ErrUnsupported, ErrDisconnect, ErrRemoteFailure} {
		message := messageForFailure(err, "download")
		if message == "" || strings.Contains(message, "ssh-secret") {
			t.Errorf("messageForFailure(%v) = %q", err, message)
		}
	}
}

func TestValidID(t *testing.T) {
	if !validID("00000000-0000-4000-8000-000000000043") {
		t.Fatal("valid UUID was rejected")
	}
	for _, value := range []string{"", "x0000000-0000-4000-8000-000000000043", "00000000-0000-0000-0000-00000000004g"} {
		if validID(value) {
			t.Errorf("validID(%q) = true", value)
		}
	}
}
