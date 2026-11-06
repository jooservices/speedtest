package server

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

type rateBucket struct {
	tokens float64
	last   time.Time
	seen   time.Time
}

// nodeRateLimiter applies a process-local token bucket per authenticated node.
type nodeRateLimiter struct {
	mu        sync.Mutex
	buckets   map[string]rateBucket
	perMinute int
	burst     int
	now       func() time.Time
}

func newOwnerRateLimiter() *nodeRateLimiter {
	return newNodeRateLimiter(ownerRequestsPerMinute, ownerBurst)
}

func newNodeRateLimiter(perMinute, burst int) *nodeRateLimiter {
	if perMinute < 1 {
		perMinute = defaultAgentRequestsPerMinute
	}
	if burst < 1 {
		burst = defaultAgentBurst
	}
	return &nodeRateLimiter{buckets: make(map[string]rateBucket), perMinute: perMinute, burst: burst, now: time.Now}
}

func (l *nodeRateLimiter) allow(key string) (bool, time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buckets) >= 100000 || len(l.buckets)%256 == 0 {
		for candidate, bucket := range l.buckets {
			if now.Sub(bucket.seen) > time.Hour {
				delete(l.buckets, candidate)
			}
		}
	}
	bucket, exists := l.buckets[key]
	if !exists {
		if len(l.buckets) >= 100000 {
			return false, time.Second
		}
		bucket = rateBucket{tokens: float64(l.burst), last: now}
	}
	elapsed := now.Sub(bucket.last).Seconds()
	if elapsed > 0 {
		bucket.tokens += elapsed * float64(l.perMinute) / 60
		if bucket.tokens > float64(l.burst) {
			bucket.tokens = float64(l.burst)
		}
		bucket.last = now
	}
	bucket.seen = now
	if bucket.tokens < 1 {
		l.buckets[key] = bucket
		wait := time.Duration((1 - bucket.tokens) * 60 / float64(l.perMinute) * float64(time.Second))
		if wait < time.Second {
			wait = time.Second
		}
		return false, wait
	}
	bucket.tokens--
	l.buckets[key] = bucket
	return true, 0
}

func allowRate(limiter *nodeRateLimiter, key string, w http.ResponseWriter, r *http.Request) bool {
	allowed, retry := limiter.allow(key)
	if allowed {
		return true
	}
	seconds := int((retry + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", fmt.Sprint(seconds))
	writeAPIProblem(w, r, http.StatusTooManyRequests, "Rate limited", "Rate limited.")
	return false
}
