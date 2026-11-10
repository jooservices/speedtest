package budget

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"time"
)

// Purpose says who asks: Interactive calls may use the whole daily budget, Background calls only the part not reserved.
type Purpose int

const (
	Interactive Purpose = iota
	Background
)

// Limit is a provider's free quota; zero means "no limit of that kind".
type Limit struct {
	PerMinute, PerDay int
	MinuteWindow      bool
}

// Reservation is one quota dimension in an atomic multi-provider reservation.
type Reservation struct {
	Provider                string
	Day, Minute, Now        time.Time
	DailyLimit, MinuteLimit int
}

// MultiReserver atomically reserves multiple provider dimensions or none.
type MultiReserver interface {
	ReserveMany(context.Context, []Reservation) (bool, error)
}

// Usage is the safe operator view of one provider's persisted daily budget.
type Usage struct {
	RequestsToday int
	DailyBudget   int
}

// Store keeps the daily counts; Add increases the count of the UTC day by n and returns the new total.
type Store interface {
	Used(ctx context.Context, provider string, day time.Time) (int, error)
	Reserve(ctx context.Context, provider string, day, minute, now time.Time, dailyLimit, minuteLimit int) (bool, error)
	BlockUntil(ctx context.Context, provider string, now, until time.Time) error
}

// ErrUnknownProvider is returned for a provider that has no configured limit entry.
var ErrUnknownProvider = errors.New("unknown geolocation provider")

// Broker decides before each geolocation call whether it may be sent.
type Broker struct {
	limits  map[string]Limit
	store   Store
	reserve float64
	now     func() time.Time
}

// New validates the configuration; reserve is the fraction of each day's budget kept back from Background calls.
func New(limits map[string]Limit, store Store, reserve float64) (*Broker, error) {
	if store == nil {
		return nil, errors.New("geolocation budget store is required")
	}
	if !(reserve >= 0 && reserve < 1) {
		return nil, fmt.Errorf("geolocation background reserve %v must be in [0,1)", reserve)
	}
	for provider, limit := range limits {
		if limit.PerMinute < 0 || limit.PerDay < 0 {
			return nil, fmt.Errorf("geolocation limit for provider %q must not be negative", provider)
		}
	}
	return &Broker{
		limits:  maps.Clone(limits),
		store:   store,
		reserve: reserve,
		now:     time.Now,
	}, nil
}

// WithClock replaces the time source; it exists so tests can move time without sleeping.
func (b *Broker) WithClock(now func() time.Time) *Broker {
	b.now = now
	return b
}

// Allow decides whether one more request to provider may be sent now and, when it says yes, counts it.
// A refused call is never counted, and a store error counts nothing.
func (b *Broker) Allow(ctx context.Context, provider string, p Purpose) (bool, error) {
	limit, known := b.limits[provider]
	if !known {
		return false, ErrUnknownProvider
	}
	now := b.now().UTC()
	return b.store.Reserve(ctx, provider, reservationDay(now, limit), now.Truncate(time.Minute), now, b.dailyCeiling(limit.PerDay, p), limit.PerMinute)
}

// AllowTogether counts multiple related provider dimensions in one transaction.
// It fails closed when the configured store cannot make the reservation atomic.
func (b *Broker) AllowTogether(ctx context.Context, p Purpose, providers ...string) (bool, error) {
	store, ok := b.store.(MultiReserver)
	if !ok {
		return false, errors.New("geolocation budget store does not support atomic multi-reservations")
	}
	now := b.now().UTC()
	reservations := make([]Reservation, 0, len(providers))
	seen := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		if _, duplicate := seen[provider]; duplicate {
			return false, fmt.Errorf("duplicate geolocation provider reservation %q", provider)
		}
		seen[provider] = struct{}{}
		limit, known := b.limits[provider]
		if !known {
			return false, ErrUnknownProvider
		}
		reservations = append(reservations, Reservation{Provider: provider, Day: reservationDay(now, limit),
			Minute: now.Truncate(time.Minute), Now: now, DailyLimit: b.dailyCeiling(limit.PerDay, p), MinuteLimit: limit.PerMinute})
	}
	return store.ReserveMany(ctx, reservations)
}

// ObserveQuota persists a provider cooldown when its response exhausts the advertised window or returns 429.
func (b *Broker) ObserveQuota(ctx context.Context, provider string, remaining int, ttl time.Duration, status int) error {
	if _, known := b.limits[provider]; !known {
		return ErrUnknownProvider
	}
	if remaining != 0 && status != 429 {
		return nil
	}
	now := b.now().UTC()
	if ttl <= 0 {
		ttl = time.Minute
	}
	return b.store.BlockUntil(ctx, provider, now, now.Add(ttl))
}

// Remaining returns how many more requests today the provider may get for the purpose; -1 means unlimited.
func (b *Broker) Remaining(ctx context.Context, provider string, p Purpose) (int, error) {
	limit, known := b.limits[provider]
	if !known {
		return 0, ErrUnknownProvider
	}
	if limit.PerDay == 0 {
		return -1, nil
	}
	ceiling := b.dailyCeiling(limit.PerDay, p)
	if ceiling < 0 {
		return 0, nil
	}
	used, err := b.store.Used(ctx, provider, utcDay(b.now()))
	if err != nil {
		return 0, err
	}
	return max(ceiling-used, 0), nil
}

// Usage reports the durable count and configured daily cap without exposing store details.
func (b *Broker) Usage(ctx context.Context, provider string) (Usage, error) {
	limit, known := b.limits[provider]
	if !known {
		return Usage{}, ErrUnknownProvider
	}
	used, err := b.store.Used(ctx, provider, utcDay(b.now()))
	if err != nil {
		return Usage{}, err
	}
	return Usage{RequestsToday: used, DailyBudget: limit.PerDay}, nil
}

// takeMinuteSlot records now when fewer than perMinute requests happened in the last minute.
// The slot is taken before the store call so concurrent callers cannot all pass the same check.
// dailyCeiling is the most requests of the day the purpose may use. Anything other than Interactive
// gets the reduced share, so an unknown purpose fails towards the stricter limit.
func (b *Broker) dailyCeiling(perDay int, p Purpose) int {
	if perDay == 0 {
		return 0
	}
	if p == Interactive {
		return perDay
	}
	// The epsilon absorbs binary rounding, for example 0.29 * 100 is 28.999... and would lose a request.
	ceiling := int(math.Floor(float64(perDay)*(1-b.reserve) + 1e-9))
	if ceiling == 0 {
		return -1
	}
	return ceiling
}

func reservationDay(now time.Time, limit Limit) time.Time {
	if limit.MinuteWindow {
		return time.Date(1, time.January, 1, 0, 0, 0, 0, time.UTC)
	}
	return utcDay(now)
}

// utcDay returns midnight UTC of the day containing t, the key the daily counts are stored under.
func utcDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}
