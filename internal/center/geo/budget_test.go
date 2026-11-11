package geo

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/geo/budget"
)

type policyBudgetStore struct {
	used int
	err  error
}

func (s *policyBudgetStore) Used(context.Context, string, time.Time) (int, error) {
	return s.used, s.err
}
func (s *policyBudgetStore) Reserve(_ context.Context, _ string, _, _, _ time.Time, daily, minute int) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	if (daily > 0 && s.used >= daily) || (minute > 0 && s.used >= minute) {
		return false, nil
	}
	s.used++
	return true, nil
}
func (*policyBudgetStore) BlockUntil(context.Context, string, time.Time, time.Time) error { return nil }

func TestBudgetBrokerReservesBackgroundQuotaAndWrapsFallback(t *testing.T) {
	cfg := ProviderConfig{IPAPIRequestsPerDay: 50, IPWhoRequestsPerDay: 5, IPWhoRequestsPerMinute: 30}
	store := &policyBudgetStore{}
	broker, err := NewBudgetBroker(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	if remaining, err := broker.Remaining(context.Background(), "ipwho", budget.Background); err != nil || remaining != 4 {
		t.Fatalf("background budget remaining = (%d,%v), want (4,nil)", remaining, err)
	}
	if remaining, err := broker.Remaining(context.Background(), "ip-api", budget.Background); err != nil || remaining != 40 {
		t.Fatalf("ip-api background reserve = (%d,%v), want (40,nil)", remaining, err)
	}
	provider := &purposeFallback{name: "ipwho", location: Location{Country: "US"}}
	wrapped := budgetProviders([]Fallback{provider}, broker)[0]
	if wrapped.Source() != "ipwho" {
		t.Fatalf("wrapped provider source = %q", wrapped.Source())
	}
	for range 4 {
		allowed, err := broker.Allow(context.Background(), "ipwho", budget.Background)
		if err != nil || !allowed {
			t.Fatalf("background reservation = (%v,%v)", allowed, err)
		}
	}
	if allowed, err := broker.Allow(context.Background(), "ipwho", budget.Background); err != nil || allowed {
		t.Fatalf("background reservation at protected share = (%v,%v)", allowed, err)
	}
	if _, err := wrapped.(interface {
		LookupFor(context.Context, string, budget.Purpose) (Location, error)
	}).LookupFor(context.Background(), "8.8.8.8", budget.Interactive); err != nil {
		t.Fatalf("interactive lookup = %v", err)
	}
	if provider.calls != 1 || provider.lastPurpose != budget.Interactive {
		t.Fatalf("provider calls=%d purpose=%v", provider.calls, provider.lastPurpose)
	}
}

func TestBudgetedFallbackLookupDefaultsToBackgroundPurpose(t *testing.T) {
	provider := &purposeFallback{name: "ipwho", location: Location{Country: "US"}}
	wrapped := budgetedFallback{provider: provider}
	location, err := wrapped.Lookup(context.Background(), "8.8.8.8")
	if err != nil || location.Country != "US" || provider.calls != 1 || provider.lastPurpose != budget.Background {
		t.Fatalf("default-purpose lookup = (%+v,%v), calls=%d purpose=%v", location, err, provider.calls, provider.lastPurpose)
	}
}

type purposeFallback struct {
	name        string
	location    Location
	calls       int
	lastPurpose budget.Purpose
}

func (p *purposeFallback) Source() string { return p.name }
func (p *purposeFallback) Lookup(ctx context.Context, _ string) (Location, error) {
	p.calls++
	p.lastPurpose = providerPurpose(ctx)
	return p.location, nil
}
func (p *purposeFallback) LookupFor(_ context.Context, _ string, purpose budget.Purpose) (Location, error) {
	p.calls++
	p.lastPurpose = purpose
	return p.location, nil
}

func TestBudgetFallbackErrorsAndDisabledBroker(t *testing.T) {
	provider := testFallback{name: "ipwho", geo: &testGeo{location: Location{Country: "US"}}}
	if got := budgetProviders([]Fallback{provider}, nil); len(got) != 1 || got[0] != provider {
		t.Fatal("nil broker unexpectedly wrapped providers")
	}
	store := &policyBudgetStore{err: errors.New("store unavailable")}
	broker, err := budget.New(map[string]budget.Limit{"ipwho": {PerDay: 10}}, store, 0.2)
	if err != nil {
		t.Fatal(err)
	}
	if allowed, err := broker.Allow(context.Background(), "ipwho", budget.Interactive); err == nil || allowed {
		t.Fatalf("store error reservation = (%v,%v)", allowed, err)
	}
}
