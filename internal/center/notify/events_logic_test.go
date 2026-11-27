package notify

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jooservices/speedtest/internal/center/catalog"
	"github.com/jooservices/speedtest/internal/center/settings"
)

type digestPreferencesFixture struct {
	view   settings.Settings
	secret string
	err    error
}

func (p digestPreferencesFixture) NotificationConfig(context.Context, pgx.Tx, string) (settings.Settings, string, error) {
	return p.view, p.secret, p.err
}

func TestAC2DigestPublisherRejectsNonDigestEvent(t *testing.T) {
	if err := (&Store{}).PublishDigestEventTx(t.Context(), nil, "test.success", nil); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("PublishDigestEventTx() error = %v, want ErrInvalidEvent", err)
	}
}

func TestAC2DigestChannelSelectionUsesOnlyEnabledConfiguredDestinations(t *testing.T) {
	view := settings.Settings{}
	view.Notify.Email = settings.ChannelConfig{On: true, To: "owner@example.test"}
	view.Notify.Telegram = settings.ChannelConfig{On: true, To: "10001"}
	configured := digestPreferencesFixture{view: view, secret: "configured"}
	disabledView := view
	disabledView.Notify.Email.On = false
	readFailure := errors.New("settings read failed")
	for _, test := range []struct {
		name        string
		preferences digestPreferencesFixture
		channels    []string
		allowed     []string
		want        []string
		wantErr     error
	}{
		{name: "filters supported channels", preferences: configured, channels: []string{"telegram", "email", "slack"}, allowed: []string{"telegram", "email"}, want: []string{"telegram", "email"}},
		{name: "skips disabled destination", preferences: digestPreferencesFixture{view: disabledView, secret: "configured"}, channels: []string{"email", "telegram"}, allowed: []string{"telegram", "email"}, want: []string{"telegram"}},
		{name: "propagates settings failure", preferences: digestPreferencesFixture{err: readFailure}, channels: []string{"email"}, allowed: []string{"email"}, wantErr: readFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &Store{Settings: test.preferences}
			got, err := configuredChannels(t.Context(), store, nil, test.channels, test.allowed)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("configuredChannels() error = %v, want %v", err, test.wantErr)
				}
				return
			}
			if err != nil || !slices.Equal(got, test.want) {
				t.Fatalf("configuredChannels() = %v, %v; want %v", got, err, test.want)
			}
		})
	}
}

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
