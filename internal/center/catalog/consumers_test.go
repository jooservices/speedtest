package catalog_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/jooservices/speedtest/internal/center/alerts"
	"github.com/jooservices/speedtest/internal/center/catalog"
	"github.com/jooservices/speedtest/internal/center/notify"
	"github.com/jooservices/speedtest/internal/center/settings"
)

func TestTypedChannelProjectionsMatchCatalog(t *testing.T) {
	var want []string
	for _, channel := range catalog.Channels() {
		want = append(want, channel.Name)
	}
	slices.Sort(want)
	for _, value := range []any{settings.NotificationSettings{}, settings.NotificationSettingsWrite{}} {
		kind := reflect.TypeOf(value)
		var got []string
		for i := 0; i < kind.NumField(); i++ {
			got = append(got, kind.Field(i).Tag.Get("json"))
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Fatalf("%s channels %v != catalog %v", kind.Name(), got, want)
		}
	}
	for _, channel := range want {
		raw, err := json.Marshal(map[string]any{channel: settings.ChannelConfig{On: true, To: "projection fixture", Configured: true}})
		if err != nil {
			t.Fatal(err)
		}
		var view settings.Settings
		if err := json.Unmarshal(raw, &view.Notify); err != nil {
			t.Fatal(err)
		}
		got, ok := notify.Channel(view, channel)
		if !ok || !got.On || !got.Configured || got.To != "projection fixture" {
			t.Fatalf("notify projection %s: %+v %v", channel, got, ok)
		}
	}
	if _, ok := notify.Channel(settings.Settings{}, "unknown"); ok {
		t.Fatal("unknown channel accepted")
	}
}

func TestAlertValidationUsesCatalog(t *testing.T) {
	for _, metric := range catalog.Metrics() {
		for _, op := range catalog.Operators() {
			for _, channel := range catalog.Channels() {
				in := alerts.RuleInput{Name: "Catalog rule", Metric: metric.Name, Op: op.Name, Value: 1, ForTests: 1, AllNodes: true, Severity: "warn", Channels: []string{channel.Name}}
				_, err := alerts.NormalizeRule(in)
				want := catalog.ValidThreshold(metric.Name, op.Name, 1, 1)
				if (err == nil) != want {
					t.Fatalf("%s %s via %s: %v", metric.Name, op.Name, channel.Name, err)
				}
			}
		}
	}
}
