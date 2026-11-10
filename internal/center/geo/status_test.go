package geo

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestProviderStatusExposesConfigurationWithoutSecrets(t *testing.T) {
	config := ProviderConfig{IPInfoEnabled: true, IPInfoToken: "private-ipinfo-token", IPGeoEnabled: true, IPGeoKey: "private-geo-key", IPWhoRequestsPerDay: 10}
	store := &policyBudgetStore{used: 3}
	broker, err := NewBudgetBroker(config, store)
	if err != nil {
		t.Fatal(err)
	}
	config.Budget = broker
	providers, err := config.Status(context.Background())
	if err != nil || len(providers) != 8 {
		t.Fatalf("Status() = %d providers, %v", len(providers), err)
	}
	body, err := json.Marshal(providers)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), config.IPInfoToken) || strings.Contains(string(body), config.IPGeoKey) {
		t.Fatalf("provider status exposed a credential: %s", body)
	}
	for _, item := range providers {
		if item.Name == "ipwho" && (item.RequestsToday != 3 || item.DailyBudget != 10) {
			t.Fatalf("ipwho usage status = %+v", item)
		}
		if item.Name == "ipinfo-lite" && (!item.Enabled || !item.KeyConfigured) {
			t.Fatalf("IPinfo status = %+v", item)
		}
		if item.Name == "ipgeolocation" && (!item.Enabled || !item.KeyConfigured) {
			t.Fatalf("ipgeolocation status = %+v", item)
		}
	}
	store.err = context.DeadlineExceeded
	if _, err := config.Status(context.Background()); err == nil {
		t.Fatal("status hid a quota store failure")
	}
}
