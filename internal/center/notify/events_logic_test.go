package notify

import (
	"testing"

	"github.com/jooservices/speedtest/internal/center/catalog"
)

func TestMergeAndValidateEventWrite(t *testing.T) {
	base, ok := catalog.LookupEvent("test.success")
	if !ok {
		t.Fatal("missing catalog event")
	}
	view := mergeEvent(base, eventOverride{})
	if view.Customized || !view.Enabled || view.Key != "test.success" || !view.NodeScoped || view.ISPScope == nil {
		t.Fatalf("defaults: %+v", view)
	}
	view = mergeEvent(base, eventOverride{Present: true, Enabled: false, Severity: "warn", Channels: []string{"email"}})
	if !view.Customized || view.Enabled || view.Severity != "warn" || len(view.Channels) != 1 {
		t.Fatalf("override: %+v", view)
	}
	if err := validateEventWrite("crit", []string{"telegram", "email"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		sev string
		ch  []string
	}{
		{"bad", []string{"telegram"}},
		{"info", nil},
		{"info", []string{}},
		{"info", []string{"telegram", "telegram"}},
		{"info", []string{"nope"}},
		{"info", []string{"email", "telegram", "slack", "webhook", "email"}},
	} {
		if err := validateEventWrite(tc.sev, tc.ch); err != ErrInvalidEvent {
			t.Fatalf("%v %v: %v", tc.sev, tc.ch, err)
		}
	}
}

func TestQualityThresholdsIncludeIntegerConsecutiveCount(t *testing.T) {
	base, _ := catalog.LookupEvent("quality.ping_high")
	for _, value := range []float64{1, 2, 100} {
		if err := validateRuntimePatch(base, EventPatch{Thresholds: map[string]float64{"for_tests": value}}); err != nil {
			t.Errorf("valid consecutive count %v rejected: %v", value, err)
		}
	}
	for _, value := range []float64{0, 1.5, 101} {
		if err := validateRuntimePatch(base, EventPatch{Thresholds: map[string]float64{"for_tests": value}}); err == nil {
			t.Errorf("invalid consecutive count %v accepted", value)
		}
	}
	if err := validateRuntimePatch(base, EventPatch{Thresholds: map[string]float64{"unknown": 1}}); err == nil {
		t.Fatal("unknown quality threshold accepted")
	}
	loginBurst, _ := catalog.LookupEvent("security.login_burst")
	for _, value := range []float64{1, 5, 100} {
		if err := validateRuntimePatch(loginBurst, EventPatch{Thresholds: map[string]float64{"attempts": value}}); err != nil {
			t.Errorf("valid attempt threshold %v rejected: %v", value, err)
		}
	}
	for _, value := range []float64{0, 5.5, 101} {
		if err := validateRuntimePatch(loginBurst, EventPatch{Thresholds: map[string]float64{"attempts": value}}); err == nil {
			t.Errorf("invalid attempt threshold %v accepted", value)
		}
	}
}
