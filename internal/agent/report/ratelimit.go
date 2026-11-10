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

// RateLimitState is the durable, per-engine backoff state stored with results.
type RateLimitState struct {
	Deadline time.Time
	Attempt  int
	Recorded bool
}

// RateLimitStateUpdate is committed in the same spool transaction as a result.
// A nil State deletes that engine's stored state.
type RateLimitStateUpdate struct {
	Engine string
	State  *RateLimitState
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
	if cfg.Max < cfg.Initial {
		return RateLimitBackoffConfig{}, fmt.Errorf("SPEEDTEST_RATE_LIMIT_BACKOFF_MAX must be a duration >= initial")
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
	by  map[string]RateLimitState
}

// NewRateLimitGate builds a gate. A nil now uses the wall clock.
func NewRateLimitGate(cfg RateLimitBackoffConfig, now func() time.Time) *RateLimitGate {
	if cfg.Initial <= 0 {
		cfg.Initial = time.Minute
	}
	if cfg.Max < cfg.Initial {
		cfg.Max = cfg.Initial
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
		by:  map[string]RateLimitState{},
	}
}

// Blocked reports whether scheduled measurements for engine should be skipped.
func (g *RateLimitGate) Blocked(engine string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	state, ok := g.by[engineKey(engine)]
	return ok && g.now().Before(state.Deadline)
}

// Deadline returns the active engine deadline, if one has been recorded.
func (g *RateLimitGate) Deadline(engine string) (time.Time, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	state, ok := g.by[engineKey(engine)]
	return state.Deadline, ok
}

// Restore replaces the in-memory state with records loaded from the durable spool.
func (g *RateLimitGate) Restore(states map[string]RateLimitState) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.by = make(map[string]RateLimitState, len(states))
	for engine, state := range states {
		g.by[engineKey(engine)] = state
	}
}

// PlanFailure computes a rate-limit transition without mutating the gate.
func (g *RateLimitGate) PlanFailure(engine, class string) (bool, *RateLimitState) {
	if class != failureclass.RateLimited {
		return true, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	key := engineKey(engine)
	state, ok := g.by[key]
	now := g.now()
	if ok && now.Before(state.Deadline) && state.Recorded {
		return false, nil
	}
	attempt := 0
	if ok {
		attempt = state.Attempt
	}
	next := RateLimitState{Deadline: now.Add(g.delay(attempt)), Attempt: min(attempt+1, 1024), Recorded: true}
	return true, &next
}

// CommitFailure applies a transition only after its result and state were spooled atomically.
func (g *RateLimitGate) CommitFailure(engine string, state RateLimitState) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.by[engineKey(engine)] = state
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
	record, state := g.PlanFailure(engine, class)
	if !record || state == nil {
		return record
	}
	g.CommitFailure(engine, *state)
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
