package auth

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestRateLimiterLimitsByIPAndAccountAndExpiresAttempts(t *testing.T) {
	start := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	limiter := NewRateLimiter(2, time.Minute)
	for _, attempt := range [][2]string{{"192.0.2.1", "Owner@example.com"}, {"192.0.2.1", "other@example.com"}} {
		if allowed, _ := limiter.Allow(attempt[0], attempt[1], start); !allowed {
			t.Fatal("a request below both limits was rejected")
		}
	}
	if allowed, wait := limiter.Allow("192.0.2.1", "third@example.com", start.Add(time.Second)); allowed || wait != 59*time.Second {
		t.Fatalf("same-IP request = %v, %s; want blocked for 59s", allowed, wait)
	}
	if allowed, _ := limiter.Allow("192.0.2.2", "OWNER@example.com", start.Add(time.Second)); !allowed {
		t.Fatal("a request from a new IP below the account limit was rejected")
	}
	if allowed, _ := limiter.Allow("192.0.2.3", "owner@example.com", start.Add(2*time.Second)); allowed {
		t.Fatal("case-insensitive account limit did not apply")
	}
	if allowed, _ := limiter.Allow("192.0.2.2", "owner@example.com", start.Add(62*time.Second)); !allowed {
		t.Fatal("expired attempts continued to block the request")
	}
}

func TestRateLimiterAllowsConcurrentCallsSafely(t *testing.T) {
	limiter := NewRateLimiter(1000, time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if allowed, _ := limiter.Allow("192.0.2.1", "owner@example.com", time.Now()); !allowed {
				t.Error("concurrent request unexpectedly exceeded the limit")
			}
		}()
	}
	wg.Wait()
}

func TestRateLimiterDisabled(t *testing.T) {
	if allowed, wait := NewRateLimiter(0, time.Minute).Allow("", "", time.Now()); !allowed || wait != 0 {
		t.Fatal("zero-limit limiter must fail open for an explicitly disabled configuration")
	}
	var limiter *RateLimiter
	if allowed, _ := limiter.Allow("", "", time.Now()); !allowed {
		t.Fatal("nil limiter must allow")
	}
}

func TestRateLimiterBoundsKeysAndPrunesExpiredAttempts(t *testing.T) {
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	limiter := NewRateLimiter(1, time.Minute)
	for index := 0; index < maxRateLimitKeys; index++ {
		limiter.buckets[fmt.Sprintf("ip:%d", index)] = rateBucket{attempts: []time.Time{now}}
	}
	if allowed, _ := limiter.Allow("new-ip", "new@example.com", now); !allowed {
		t.Fatal("new keys were unexpectedly rate limited")
	}
	if len(limiter.buckets) > maxRateLimitKeys {
		t.Fatalf("limiter retained %d keys, maximum is %d", len(limiter.buckets), maxRateLimitKeys)
	}

	limiter.buckets["expired"] = rateBucket{attempts: []time.Time{now.Add(-2 * time.Minute)}}
	limiter.prune(now.Add(-time.Minute))
	if _, exists := limiter.buckets["expired"]; exists {
		t.Fatal("prune retained a bucket whose attempts expired")
	}
	for index := 0; index <= maxRateLimitKeys; index++ {
		limiter.buckets[fmt.Sprintf("retained:%d", index)] = rateBucket{attempts: []time.Time{now}}
	}
	limiter.prune(now.Add(-time.Minute))
	if len(limiter.buckets) > maxRateLimitKeys {
		t.Fatalf("prune retained %d keys above the configured maximum", len(limiter.buckets))
	}

	limiter.buckets = make(map[string]rateBucket, maxRateLimitKeys+1)
	for index := 0; index <= maxRateLimitKeys; index++ {
		limiter.buckets[fmt.Sprintf("empty:%d", index)] = rateBucket{}
	}
	limiter.evictOldest()
	if len(limiter.buckets) != maxRateLimitKeys+1 {
		t.Fatal("evictOldest should stop when buckets have no timestamps to rank")
	}
}

func TestRateLimiterTrimsPartialWindowBucket(t *testing.T) {
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	limiter := NewRateLimiter(3, time.Minute)
	bucket := rateBucket{attempts: []time.Time{now.Add(-2 * time.Minute), now.Add(-30 * time.Second)}}
	trimmed := limiter.withinWindow(bucket, now.Add(-time.Minute))
	if len(trimmed.attempts) != 1 || !trimmed.attempts[0].Equal(now.Add(-30*time.Second)) {
		t.Fatalf("withinWindow() = %v, want only the recent attempt", trimmed.attempts)
	}
}
