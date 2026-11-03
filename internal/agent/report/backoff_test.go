package report

import (
	"testing"
	"time"
)

func TestBackoffDoublesWithoutJitter(t *testing.T) {
	b := DefaultBackoff()
	b.Jitter = 0
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	for i, w := range want {
		if got := b.Next(); got != w {
			t.Fatalf("Next() #%d = %v, want %v", i, got, w)
		}
	}
}

func TestBackoffCapsAtMax(t *testing.T) {
	b := Backoff{Initial: time.Second, Max: 3 * time.Second, Factor: 10, Jitter: 0}
	for i := 0; i < 5; i++ {
		if got := b.Next(); got > 3*time.Second {
			t.Fatalf("Next() #%d = %v, want capped at 3s", i, got)
		}
	}
	if got := b.Next(); got != 3*time.Second {
		t.Fatalf("Next() = %v, want exactly 3s once capped", got)
	}
}

func TestBackoffJitterStaysWithinBounds(t *testing.T) {
	b := Backoff{Initial: 4 * time.Second, Max: time.Minute, Factor: 2, Jitter: 0.5}
	base := 4 * time.Second
	for i := 0; i < 200; i++ {
		got := b.Next()
		if got < base/2 || got > base {
			t.Fatalf("Next() #%d = %v, want within [%v, %v]", i, got, base/2, base)
		}
		if base < time.Minute {
			base *= 2
			if base > time.Minute {
				base = time.Minute
			}
		}
	}
}

func TestBackoffReset(t *testing.T) {
	b := DefaultBackoff()
	b.Jitter = 0
	b.Next()
	b.Next()
	b.Reset()
	if got := b.Next(); got != time.Second {
		t.Fatalf("Next() after Reset = %v, want 1s", got)
	}
}

func TestBackoffZeroDefaults(t *testing.T) {
	var b Backoff
	b.Jitter = 0
	if got := b.Next(); got != time.Second {
		t.Fatalf("Next() = %v, want default 1s", got)
	}
}

func TestBackoffInvalidJitterUsesDefaults(t *testing.T) {
	b := Backoff{Jitter: 2}
	d := b.Next()
	if d < 500*time.Millisecond || d > time.Second {
		t.Fatal(d)
	}
	for i := 0; i < 1100; i++ {
		d = b.Next()
		if d < 0 || d > 5*time.Minute {
			t.Fatal(d)
		}
	}
}
