package geo

import (
	"context"

	"github.com/jooservices/speedtest/internal/center/geo/budget"
)

const backgroundReserve = 0.2

type providerPurposeKey struct{}

func withProviderPurpose(ctx context.Context, purpose budget.Purpose) context.Context {
	return context.WithValue(ctx, providerPurposeKey{}, purpose)
}

func providerPurpose(ctx context.Context) budget.Purpose {
	purpose, ok := ctx.Value(providerPurposeKey{}).(budget.Purpose)
	if !ok {
		return budget.Background
	}
	return purpose
}

// NewBudgetBroker builds the documented per-provider quota policy over a durable store.
func NewBudgetBroker(cfg ProviderConfig, store budget.Store) (*budget.Broker, error) {
	limits := map[string]budget.Limit{
		"ip-api":        {PerMinute: cfg.IPAPIRequestsPerMinute, PerDay: cfg.IPAPIRequestsPerDay},
		"ip-api-batch":  {PerMinute: 15, MinuteWindow: true},
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
}

func (p budgetedFallback) Source() string { return p.provider.Source() }

func (p budgetedFallback) Lookup(ctx context.Context, ip string) (Location, error) {
	return p.LookupFor(ctx, ip, budget.Background)
}

func (p budgetedFallback) LookupFor(ctx context.Context, ip string, purpose budget.Purpose) (Location, error) {
	return p.provider.Lookup(withProviderPurpose(ctx, purpose), ip)
}

func budgetProviders(providers []Fallback, broker *budget.Broker) []Fallback {
	if broker == nil {
		return providers
	}
	wrapped := make([]Fallback, len(providers))
	for i, provider := range providers {
		wrapped[i] = budgetedFallback{provider: provider}
	}
	return wrapped
}
