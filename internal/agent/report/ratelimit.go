package report

import (
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/jooservices/speedtest/internal/shared/failureclass"
)

// RateLimitBackoffConfig controls exponential provider rate-limit backoff.
type RateLimitBackoffConfig struct {
	Initial time.Duration
	Max     time.Duration
	Factor  float64
	Jitter  float64
}

// DefaultRateLimitBackoff is 1m doubling to 1h with 20% jitter.
func DefaultRateLimitBackoff() RateLimitBackoffConfig {
	return RateLimitBackoffConfig{Initial: time.Minute, Max: time.Hour, Factor: 2, Jitter: 0.2}
}

// RateLimitBackoffConfigFromEnv reads SPEEDTEST_RATE_LIMIT_BACKOFF_* from the environment.
func RateLimitBackoffConfigFromEnv() (RateLimitBackoffConfig, error) {
	cfg := DefaultRateLimitBackoff()
	if raw := os.Getenv("SPEEDTEST_RATE_LIMIT_BACKOFF_INITIAL"); raw != "" {
		v, err := time.ParseDuration(raw)
		if err != nil || v <= 0 {
			return RateLimitBackoffConfig{}, fmt.Errorf("SPEEDTEST_RATE_LIMIT_BACKOFF_INITIAL must be a positive duration")
		}
		cfg.Initial = v
	}
	if raw := os.Getenv("SPEEDTEST_RATE_LIMIT_BACKOFF_MAX"); raw != "" {
		v, err := time.ParseDuration(raw)
		if err != nil || v < cfg.Initial {
			return RateLimitBackoffConfig{}, fmt.Errorf("SPEEDTEST_RATE_LIMIT_BACKOFF_MAX must be a duration >= initial")
		}
		cfg.Max = v
	}
	if raw := os.Getenv("SPEEDTEST_RATE_LIMIT_BACKOFF_FACTOR"); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || v < 1 || math.IsNaN(v) || math.IsInf(v, 0) {
			return RateLimitBackoffConfig{}, fmt.Errorf("SPEEDTEST_RATE_LIMIT_BACKOFF_FACTOR must be >= 1")
		}
		cfg.Factor = v
	}
	if raw := os.Getenv("SPEEDTEST_RATE_LIMIT_BACKOFF_JITTER"); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || v < 0 || v >= 1 || math.IsNaN(v) {
			return RateLimitBackoffConfig{}, fmt.Errorf("SPEEDTEST_RATE_LIMIT_BACKOFF_JITTER must be in [0,1)")
		}
		cfg.Jitter = v
	}
	return cfg, nil
}

// RateLimitGate tracks per-engine provider backoff windows and ensures at most
// one failed result is recorded while backing off.
type RateLimitGate struct {
	cfg RateLimitBackoffConfig
	now func() time.Time
	rng *rand.Rand
	mu  sync.Mutex
	by  map[string]*rateLimitState
}

type rateLimitState struct {
	until    time.Time
	attempt  int
	recorded bool
}

// NewRateLimitGate builds a gate. A nil now uses the wall clock.
func NewRateLimitGate(cfg RateLimitBackoffConfig, now func() time.Time) *RateLimitGate {
	if cfg.Initial <= 0 {
		cfg.Initial = time.Minute
	}
	if cfg.Max < cfg.Initial {
		cfg.Max = time.Hour
	}
	if cfg.Factor < 1 {
		cfg.Factor = 2
	}
	if cfg.Jitter < 0 || cfg.Jitter >= 1 {
		cfg.Jitter = 0.2
	}
	if now == nil {
		now = time.Now
	}
	return &RateLimitGate{
		cfg: cfg,
		now: now,
		rng: rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())),
		by:  map[string]*rateLimitState{},
	}
}

// Blocked reports whether scheduled measurements for engine should be skipped.
func (g *RateLimitGate) Blocked(engine string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	state := g.by[engineKey(engine)]
	return state != nil && g.now().Before(state.until)
}

// ObserveSuccess clears backoff for engine after a successful measurement.
func (g *RateLimitGate) ObserveSuccess(engine string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.by, engineKey(engine))
}

// ObserveFailure updates backoff for a failed result. record is true when the
// failure should be spooled (at most once per backoff window for rate limits).
func (g *RateLimitGate) ObserveFailure(engine, class string) bool {
	if class != failureclass.RateLimited {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	key := engineKey(engine)
	state := g.by[key]
	now := g.now()
	if state != nil && now.Before(state.until) {
		if state.recorded {
			return false
		}
		state.recorded = true
		return true
	}
	if state == nil {
		state = &rateLimitState{}
		g.by[key] = state
	}
	delay := g.delay(state.attempt)
	if state.attempt < 1024 {
		state.attempt++
	}
	state.until = now.Add(delay)
	state.recorded = true
	return true
}

func (g *RateLimitGate) delay(attempt int) time.Duration {
	base := float64(g.cfg.Initial) * math.Pow(g.cfg.Factor, float64(attempt))
	if base > float64(g.cfg.Max) {
		base = float64(g.cfg.Max)
	}
	delay := time.Duration(base)
	if g.cfg.Jitter > 0 && delay > 0 {
		lower := float64(delay) * (1 - g.cfg.Jitter)
		delay = time.Duration(lower + g.rng.Float64()*float64(delay)*g.cfg.Jitter)
	}
	return delay
}

func engineKey(engine string) string {
	if engine == "" {
		return "ookla"
	}
	return engine
}
