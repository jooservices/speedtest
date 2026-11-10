package geo

import (
	"context"

	"github.com/jooservices/speedtest/internal/center/geo/budget"
)

const backgroundReserve = 0.2

// NewBudgetBroker builds the documented per-provider quota policy over a durable store.
func NewBudgetBroker(cfg ProviderConfig, store budget.Store) (*budget.Broker, error) {
	limits := map[string]budget.Limit{
		"ip-api":        {PerMinute: cfg.IPAPIRequestsPerMinute, PerDay: cfg.IPAPIRequestsPerDay},
		"ipinfo-lite":   {PerMinute: cfg.IPInfoRequestsPerMinute, PerDay: cfg.IPInfoRequestsPerDay},
		"ipwho":         {PerMinute: cfg.IPWhoRequestsPerMinute, PerDay: cfg.IPWhoRequestsPerDay},
		"ipapi-co":      {PerMinute: cfg.IPAPICoRequestsPerMinute, PerDay: cfg.IPAPICoRequestsPerDay},
		"ipgeolocation": {PerMinute: cfg.IPGeoRequestsPerMinute, PerDay: cfg.IPGeoRequestsPerDay},
		"freeipapi":     {PerMinute: cfg.FreeIPAPIRequestsPerMinute, PerDay: cfg.FreeIPAPIRequestsPerDay},
		"geojs":         {PerMinute: cfg.GeoJSRequestsPerMinute, PerDay: cfg.GeoJSRequestsPerDay},
		"ripestat":      {PerMinute: cfg.RIPEStatRequestsPerMinute, PerDay: cfg.RIPEStatRequestsPerDay},
	}
	return budget.New(limits, store, backgroundReserve)
}

type budgetedFallback struct {
	provider Fallback
	broker   *budget.Broker
	observe  func(string, string)
}

func (p budgetedFallback) Source() string { return p.provider.Source() }

func (p budgetedFallback) Lookup(ctx context.Context, ip string) (Location, error) {
	return p.LookupFor(ctx, ip, budget.Background)
}

func (p budgetedFallback) LookupFor(ctx context.Context, ip string, purpose budget.Purpose) (Location, error) {
	allowed, err := p.broker.Allow(ctx, p.provider.Source(), purpose)
	if err != nil {
		if p.observe != nil {
			p.observe(p.provider.Source(), "error")
		}
		return Location{}, err
	}
	if !allowed {
		if p.observe != nil {
			p.observe(p.provider.Source(), "budget_exhausted")
		}
		return Location{}, ErrBudgetExhausted
	}
	return p.provider.Lookup(ctx, ip)
}

func budgetProviders(providers []Fallback, broker *budget.Broker, observe func(string, string)) []Fallback {
	if broker == nil {
		return providers
	}
	wrapped := make([]Fallback, len(providers))
	for i, provider := range providers {
		wrapped[i] = budgetedFallback{provider: provider, broker: broker, observe: observe}
	}
	return wrapped
}
