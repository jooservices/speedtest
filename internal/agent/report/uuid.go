package report

import (
	"crypto/rand"
	"fmt"
	"io"
)

// newID returns a random RFC 4122 version 4 UUID string. The UUID is allocated
// before a result is spooled, which is what makes retries idempotent at the
// command center.
func newID() (string, error) {
	return newIDFrom(rand.Reader)
}

func newIDFrom(random io.Reader) (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(random, b[:]); err != nil {
		return "", fmt.Errorf("generate result UUID: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
