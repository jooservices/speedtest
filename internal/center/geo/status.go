package geo

import (
	"context"
	"sort"
	"strings"
)

// ProviderStatus contains operator-safe provider state; credential values are never returned.
type ProviderStatus struct {
	Name          string `json:"name"`
	Enabled       bool   `json:"enabled"`
	KeyConfigured bool   `json:"keyConfigured"`
	RequestsToday int    `json:"requestsToday"`
	DailyBudget   int    `json:"dailyBudget"`
}

// Status reports configured online providers and their durable UTC-day usage.
func (c ProviderConfig) Status(ctx context.Context) ([]ProviderStatus, error) {
	definitions := []ProviderStatus{
		{Name: "ipinfo-lite", Enabled: c.IPInfoEnabled, KeyConfigured: strings.TrimSpace(c.IPInfoToken) != ""},
		{Name: "ip-api", Enabled: c.IPAPIEnabled},
		{Name: "ipwho", Enabled: c.IPWhoEnabled},
		{Name: "freeipapi", Enabled: c.FreeIPAPIEnabled},
		{Name: "geojs", Enabled: c.GeoJSEnabled},
		{Name: "ipapi-co", Enabled: c.IPAPICoEnabled},
		{Name: "ipgeolocation", Enabled: c.IPGeoEnabled, KeyConfigured: strings.TrimSpace(c.IPGeoKey) != ""},
		{Name: "ripestat", Enabled: c.RIPEStatEnabled},
	}
	if c.Budget != nil {
		for i := range definitions {
			usage, err := c.Budget.Usage(ctx, definitions[i].Name)
			if err != nil {
				return nil, err
			}
			definitions[i].RequestsToday = usage.RequestsToday
			definitions[i].DailyBudget = usage.DailyBudget
		}
	}
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].Name < definitions[j].Name })
	return definitions, nil
}
