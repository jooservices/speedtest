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
	provider := testFallback{name: "ipwho", geo: &testGeo{location: Location{Country: "US"}}}
	var outcomes []string
	wrapped := budgetProviders([]Fallback{provider}, broker, func(_, result string) { outcomes = append(outcomes, result) })[0]
	if wrapped.Source() != "ipwho" {
		t.Fatalf("wrapped provider source = %q", wrapped.Source())
	}
	for range 4 {
		if _, err := wrapped.Lookup(context.Background(), "8.8.8.8"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := wrapped.Lookup(context.Background(), "8.8.8.8"); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("background lookup at reserve = %v", err)
	}
	if _, err := wrapped.(interface {
		LookupFor(context.Context, string, budget.Purpose) (Location, error)
	}).LookupFor(context.Background(), "8.8.8.8", budget.Interactive); err != nil {
		t.Fatalf("interactive reserved request = %v", err)
	}
	if provider.geo.calls.Load() != 5 || len(outcomes) != 1 || outcomes[0] != "budget_exhausted" {
		t.Fatalf("provider calls=%d outcomes=%v", provider.geo.calls.Load(), outcomes)
	}
}

func TestBudgetFallbackErrorsAndDisabledBroker(t *testing.T) {
	provider := testFallback{name: "ipwho", geo: &testGeo{location: Location{Country: "US"}}}
	if got := budgetProviders([]Fallback{provider}, nil, nil); len(got) != 1 || got[0] != provider {
		t.Fatal("nil broker unexpectedly wrapped providers")
	}
	store := &policyBudgetStore{err: errors.New("store unavailable")}
	broker, err := budget.New(map[string]budget.Limit{"ipwho": {PerDay: 10}}, store, 0.2)
	if err != nil {
		t.Fatal(err)
	}
	var outcome string
	wrapped := budgetProviders([]Fallback{provider}, broker, func(_, value string) { outcome = value })[0]
	if _, err := wrapped.Lookup(context.Background(), "8.8.8.8"); err == nil || outcome != "error" {
		t.Fatalf("store error = %v outcome=%q", err, outcome)
	}
}
