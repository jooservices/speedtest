package auth

import (
	"strings"
	"sync"
	"time"
)

const maxRateLimitKeys = 10000

type rateBucket struct {
	attempts []time.Time
}

// RateLimiter tracks a bounded sliding window independently by peer IP and
// normalized account email. It is safe for concurrent handler use.
type RateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	buckets map[string]rateBucket
	checks  uint64
}

func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{limit: limit, window: window, buckets: make(map[string]rateBucket)}
}

// Allow records a login attempt if both its IP and account buckets are below
// their limit. retryAfter is the longer wait required by either bucket.
func (l *RateLimiter) Allow(ip, email string, now time.Time) (allowed bool, retryAfter time.Duration) {
	if l == nil || l.limit < 1 || l.window <= 0 {
		return true, 0
	}
	keys := []string{"ip:" + ip, "account:" + strings.ToLower(strings.TrimSpace(email))}
	cutoff := now.Add(-l.window)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.checks++
	if l.checks%64 == 0 || len(l.buckets) >= maxRateLimitKeys {
		l.prune(cutoff)
	}
	var wait time.Duration
	for _, key := range keys {
		bucket := l.withinWindow(l.buckets[key], cutoff)
		l.buckets[key] = bucket
		if len(bucket.attempts) >= l.limit {
			candidate := bucket.attempts[0].Add(l.window).Sub(now)
			if candidate > wait {
				wait = candidate
			}
		}
	}
	if wait > 0 {
		return false, wait
	}
	for _, key := range keys {
		bucket := l.buckets[key]
		bucket.attempts = append(bucket.attempts, now)
		l.buckets[key] = bucket
	}
	if len(l.buckets) > maxRateLimitKeys {
		l.evictOldest()
	}
	return true, 0
}

func (l *RateLimiter) withinWindow(bucket rateBucket, cutoff time.Time) rateBucket {
	first := 0
	for first < len(bucket.attempts) && !bucket.attempts[first].After(cutoff) {
		first++
	}
	if first == len(bucket.attempts) {
		return rateBucket{}
	}
	if first > 0 {
		bucket.attempts = append([]time.Time(nil), bucket.attempts[first:]...)
	}
	return bucket
}

func (l *RateLimiter) prune(cutoff time.Time) {
	for key, bucket := range l.buckets {
		bucket = l.withinWindow(bucket, cutoff)
		if len(bucket.attempts) == 0 {
			delete(l.buckets, key)
			continue
		}
		l.buckets[key] = bucket
	}
	if len(l.buckets) > maxRateLimitKeys {
		l.evictOldest()
	}
}

func (l *RateLimiter) evictOldest() {
	for len(l.buckets) > maxRateLimitKeys {
		oldestKey := ""
		var oldest time.Time
		for key, bucket := range l.buckets {
			if len(bucket.attempts) > 0 && (oldest.IsZero() || bucket.attempts[0].Before(oldest)) {
				oldestKey, oldest = key, bucket.attempts[0]
			}
		}
		if oldestKey == "" {
			break
		}
		delete(l.buckets, oldestKey)
	}
}
