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
	if view.Customized || !view.Enabled || view.Key != "test.success" || !view.NodeScoped {
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
