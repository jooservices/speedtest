package report

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestNewIDIsVersion4AndUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id, err := newID()
		if err != nil {
			t.Fatalf("newID() error = %v", err)
		}
		if !validUUIDv4(id) {
			t.Fatalf("newID() = %q, want a version 4 UUID", id)
		}
		if seen[id] {
			t.Fatalf("newID() returned duplicate %q", id)
		}
		seen[id] = true
	}
}

func validUUIDv4(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		case 14:
			if c != '4' {
				return false
			}
		case 19:
			if !strings.ContainsRune("89ab", c) {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdef", c) {
				return false
			}
		}
	}
	return true
}

func TestNewIDEntropyFailure(t *testing.T) {
	id, err := newIDFrom(strings.NewReader("short"))
	if id != "" || !errors.Is(err, io.ErrUnexpectedEOF) || !strings.Contains(err.Error(), "generate result UUID") {
		t.Fatalf("id=%q err=%v", id, err)
	}
}
