package report

import (
	"math"
	"math/rand/v2"
	"time"
)

// Backoff produces exponentially increasing delays with jitter for retrying
// failed result deliveries. It is not safe for concurrent use; the reporter
// serializes retries.
type Backoff struct {
	// Initial is the first delay; Max caps it. Factor multiplies the delay on
	// each attempt and Jitter randomizes it. See DefaultBackoff.
	Initial time.Duration
	Max     time.Duration
	Factor  float64
	Jitter  float64

	rng      *rand.Rand
	attempts int
}

// DefaultBackoff is the standard retry schedule: 1s doubling to a 5 minute cap
// with 50% jitter, so many nodes do not retry in lockstep.
func DefaultBackoff() Backoff {
	return Backoff{
		Initial: time.Second,
		Max:     5 * time.Minute,
		Factor:  2,
		Jitter:  0.5,
		rng:     rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())),
	}
}

// Next returns the delay before the next retry and advances the attempt count.
// Zero or negative fields are treated as the defaults so a zero Backoff still
// behaves sensibly.
func (b *Backoff) Next() time.Duration {
	if b.Initial <= 0 {
		b.Initial = time.Second
	}
	if b.Max <= 0 {
		b.Max = 5 * time.Minute
	}
	if b.Factor < 1 || math.IsNaN(b.Factor) || math.IsInf(b.Factor, 0) {
		b.Factor = 2
	}
	if b.Jitter < 0 || b.Jitter >= 1 || math.IsNaN(b.Jitter) {
		b.Jitter = 0.5
	}
	if b.rng == nil {
		b.rng = rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	}

	base := float64(b.Initial) * math.Pow(b.Factor, float64(b.attempts))
	if b.attempts < 1024 {
		b.attempts++
	}
	if base > float64(b.Max) {
		base = float64(b.Max)
	}
	delay := time.Duration(base)
	if b.Jitter > 0 && delay > 0 {
		// Uniform jitter: delay is drawn from [base*(1-jitter), base].
		lower := float64(delay) * (1 - b.Jitter)
		delay = time.Duration(lower + b.rng.Float64()*float64(delay)*b.Jitter)
	}
	return delay
}

// Reset clears the attempt count after a successful delivery.
func (b *Backoff) Reset() { b.attempts = 0 }
