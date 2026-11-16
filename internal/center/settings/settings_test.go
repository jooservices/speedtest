package settings

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func validInput() SettingsInput {
	return SettingsInput{
		Theme:          "dark",
		Language:       "en",
		Range:          "7d",
		Units:          "auto",
		Timezone:       "browser",
		Hour12:         false,
		Decimals:       1,
		RetentionDays:  365,
		GeoProvider:    "geolite2",
		GeoFallback:    "ip-api",
		GeoAutoUpdate:  true,
		Engine:         "ookla",
		DefaultThreads: 4,
		ServerMode:     "auto",
		MaxConcurrent:  1,
		Stagger:        true,
		QuietStart:     "",
		QuietEnd:       "",
		CenterURL:      "https://speedtest.example.com",
		Notify: NotificationSettingsWrite{
			Email:    ChannelWrite{On: true, To: "ops@example.com"},
			Telegram: ChannelWrite{On: false, To: ""},
			Slack:    ChannelWrite{On: false, To: ""},
			Webhook:  ChannelWrite{On: false, To: ""},
		},
	}
}

func TestValidateAcceptsDefaults(t *testing.T) {
	if err := Validate(validInput()); err != nil {
		t.Fatalf("Validate(valid) = %v, want nil", err)
	}
}

func TestValidateAcceptsSupportedLanguages(t *testing.T) {
	for _, language := range []string{"en", "vi"} {
		input := validInput()
		input.Language = language
		if err := Validate(input); err != nil {
			t.Fatalf("Validate(language=%q) = %v, want nil", language, err)
		}
	}
}

func TestValidateRejectsInvalidFields(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*SettingsInput)
		field  string
	}{
		{name: "theme", mutate: func(i *SettingsInput) { i.Theme = "blue" }, field: "theme"},
		{name: "language fr", mutate: func(i *SettingsInput) { i.Language = "fr" }, field: "language"},
		{name: "language empty", mutate: func(i *SettingsInput) { i.Language = "" }, field: "language"},
		{name: "language uppercase", mutate: func(i *SettingsInput) { i.Language = "VI" }, field: "language"},
		{name: "range", mutate: func(i *SettingsInput) { i.Range = "12h" }, field: "range"},
		{name: "units", mutate: func(i *SettingsInput) { i.Units = "Kbps" }, field: "units"},
		{name: "timezone empty", mutate: func(i *SettingsInput) { i.Timezone = "" }, field: "timezone"},
		{name: "timezone invalid", mutate: func(i *SettingsInput) { i.Timezone = "Mars/Olympus" }, field: "timezone"},
		{name: "decimals high", mutate: func(i *SettingsInput) { i.Decimals = 3 }, field: "decimals"},
		{name: "decimals low", mutate: func(i *SettingsInput) { i.Decimals = -1 }, field: "decimals"},
		{name: "retention", mutate: func(i *SettingsInput) { i.RetentionDays = 7 }, field: "retentionDays"},
		{name: "geo provider", mutate: func(i *SettingsInput) { i.GeoProvider = "google" }, field: "geoProvider"},
		{name: "geo fallback", mutate: func(i *SettingsInput) { i.GeoFallback = "geolite2" }, field: "geoFallback"},
		{name: "engine", mutate: func(i *SettingsInput) { i.Engine = "fast.com" }, field: "engine"},
		{name: "threads high", mutate: func(i *SettingsInput) { i.DefaultThreads = 65 }, field: "defaultThreads"},
		{name: "threads low", mutate: func(i *SettingsInput) { i.DefaultThreads = 0 }, field: "defaultThreads"},
		{name: "server mode", mutate: func(i *SettingsInput) { i.ServerMode = "manual" }, field: "serverMode"},
		{name: "max concurrent", mutate: func(i *SettingsInput) { i.MaxConcurrent = 6 }, field: "maxConcurrent"},
		{name: "center url", mutate: func(i *SettingsInput) { i.CenterURL = "not a url" }, field: "centerUrl"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := validInput()
			tt.mutate(&input)
			err := Validate(input)
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("Validate() error = %v, want *ValidationError", err)
			}
			if _, ok := ve.Fields[tt.field]; !ok {
				t.Fatalf("ValidationError.Fields = %v, want key %q", ve.Fields, tt.field)
			}
		})
	}
}

func TestValidateQuietHours(t *testing.T) {
	cases := []struct {
		name       string
		start, end string
		field      string
	}{
		{name: "both empty ok", start: "", end: "", field: ""},
		{name: "valid pair ok", start: "01:30", end: "06:00", field: ""},
		{name: "cross midnight ok", start: "23:00", end: "01:00", field: ""},
		{name: "only start", start: "01:00", end: "", field: "quietEnd"},
		{name: "only end", start: "", end: "01:00", field: "quietEnd"},
		{name: "bad start", start: "25:00", end: "06:00", field: "quietStart"},
		{name: "bad end", start: "01:00", end: "6:00", field: "quietEnd"},
		{name: "equal", start: "06:00", end: "06:00", field: "quietEnd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := validInput()
			input.QuietStart = tc.start
			input.QuietEnd = tc.end
			err := Validate(input)
			if tc.field == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("Validate() error = %v, want *ValidationError", err)
			}
			if _, ok := ve.Fields[tc.field]; !ok {
				t.Fatalf("Fields = %v, want key %q", ve.Fields, tc.field)
			}
		})
	}
}

func TestValidateTimezones(t *testing.T) {
	for _, name := range []string{"browser", "UTC", "Asia/Ho_Chi_Minh", "America/New_York", "Europe/London"} {
		input := validInput()
		input.Timezone = name
		if err := Validate(input); err != nil {
			t.Errorf("Validate(timezone=%q) = %v, want nil", name, err)
		}
	}
}

func TestValidateCenterURL(t *testing.T) {
	valid := []string{"", "https://speedtest.example.com", "http://127.0.0.1:8080", "http://localhost"}
	for _, raw := range valid {
		input := validInput()
		input.CenterURL = raw
		if err := Validate(input); err != nil {
			t.Errorf("Validate(centerUrl=%q) = %v, want nil", raw, err)
		}
	}
	for _, raw := range []string{"http://speedtest.example.com", "ftp://host.example.com", "//missing-scheme", "https://"} {
		input := validInput()
		input.CenterURL = raw
		var ve *ValidationError
		if err := Validate(input); !errors.As(err, &ve) {
			t.Errorf("Validate(centerUrl=%q) error = %v, want validation error", raw, err)
		}
	}
}

func TestValidateNotificationChannels(t *testing.T) {
	t.Run("email", func(t *testing.T) {
		input := validInput()
		input.Notify.Email.To = "not-an-email"
		var ve *ValidationError
		if err := Validate(input); !errors.As(err, &ve) {
			t.Fatalf("error = %v, want validation error", err)
		}
		if _, ok := ve.Fields["notify.email.to"]; !ok {
			t.Fatalf("Fields = %v, want notify.email.to", ve.Fields)
		}
	})
	t.Run("telegram chat id", func(t *testing.T) {
		input := validInput()
		input.Notify.Telegram.To = "abc"
		var ve *ValidationError
		if err := Validate(input); !errors.As(err, &ve) {
			t.Fatalf("error = %v, want validation error", err)
		}
		if _, ok := ve.Fields["notify.telegram.to"]; !ok {
			t.Fatalf("Fields = %v, want notify.telegram.to", ve.Fields)
		}
	})
	t.Run("slack to must be empty", func(t *testing.T) {
		input := validInput()
		input.Notify.Slack.To = "https://hooks.slack.com/secret"
		var ve *ValidationError
		if err := Validate(input); !errors.As(err, &ve) {
			t.Fatalf("error = %v, want validation error", err)
		}
		if _, ok := ve.Fields["notify.slack.to"]; !ok {
			t.Fatalf("Fields = %v, want notify.slack.to", ve.Fields)
		}
	})
	t.Run("webhook secret must be url", func(t *testing.T) {
		input := validInput()
		secret := "not-a-url"
		input.Notify.Webhook.Secret = &secret
		var ve *ValidationError
		if err := Validate(input); !errors.As(err, &ve) {
			t.Fatalf("error = %v, want validation error", err)
		}
		if _, ok := ve.Fields["notify.webhook.secret"]; !ok {
			t.Fatalf("Fields = %v, want notify.webhook.secret", ve.Fields)
		}
	})
	t.Run("empty secret rejected", func(t *testing.T) {
		input := validInput()
		secret := ""
		input.GeoKey = &secret
		var ve *ValidationError
		if err := Validate(input); !errors.As(err, &ve) {
			t.Fatalf("error = %v, want validation error", err)
		}
		if _, ok := ve.Fields["geoKey"]; !ok {
			t.Fatalf("Fields = %v, want geoKey", ve.Fields)
		}
	})
}

func TestDefaultsMatchPreview(t *testing.T) {
	got := Defaults("")
	want := Settings{
		Theme:            "dark",
		Language:         "en",
		Range:            "7d",
		Units:            "auto",
		Timezone:         "browser",
		Hour12:           false,
		Decimals:         1,
		RetentionDays:    365,
		GeoProvider:      "geolite2",
		GeoFallback:      "ip-api",
		GeoAutoUpdate:    true,
		Engine:           "ookla",
		DefaultThreads:   4,
		ServerMode:       "auto",
		MaxConcurrent:    1,
		Stagger:          true,
		NotifyQuietStart: "23:00", NotifyQuietEnd: "07:00", NotifyQuietZone: "Asia/Bangkok", NotifyQuietCritical: true,
		Notify: NotificationSettings{
			Email: ChannelConfig{On: true, To: "ops@example.com"},
		},
	}
	if got != want {
		t.Fatalf("Defaults() = %+v, want %+v", got, want)
	}
}

// TestReadModelMasksSecrets verifies the read model never contains a secret
// value or field, only configured flags.
func TestReadModelMasksSecrets(t *testing.T) {
	view := Defaults("")
	view.GeoKeyConfigured = true
	view.EnrollKeyConfigured = true
	view.Notify.Webhook.Configured = true

	data, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	raw := string(data)
	for _, required := range []string{`"geoKeyConfigured":true`, `"enrollKeyConfigured":true`, `"configured":true`} {
		if !strings.Contains(raw, required) {
			t.Errorf("read model %s, want %s", raw, required)
		}
	}
	for _, forbidden := range []string{`"geoKey":`, `"enrollToken":`, `"secret":`} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("read model %s must not contain %q", raw, forbidden)
		}
	}
}

func TestValidateNotificationQuietHours(t *testing.T) {
	cases := []struct {
		name             string
		start, end, zone string
		wantField        string
	}{
		{name: "none", wantField: ""},
		{name: "night window", start: "22:00", end: "07:00", zone: "Asia/Ho_Chi_Minh", wantField: ""},
		{name: "only start", start: "22:00", wantField: "notifyQuietEnd"},
		{name: "bad start", start: "25:00", end: "07:00", wantField: "notifyQuietStart"},
		{name: "equal ends", start: "07:00", end: "07:00", wantField: "notifyQuietEnd"},
		{name: "unknown zone", start: "22:00", end: "07:00", zone: "Mars/Base", wantField: "notifyQuietTimezone"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := validInput()
			input.NotifyQuietStart, input.NotifyQuietEnd, input.NotifyQuietZone = tc.start, tc.end, tc.zone
			err := Validate(input)
			if tc.wantField == "" {
				if err != nil {
					t.Fatalf("Validate() = %v", err)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) || len(ve.Fields[tc.wantField]) == 0 {
				t.Fatalf("Validate() = %v, want an error on %s", err, tc.wantField)
			}
		})
	}
	if d := Defaults(""); !d.NotifyQuietCritical || d.NotifyQuietZone != "Asia/Bangkok" || d.NotifyQuietStart != "23:00" || d.NotifyQuietEnd != "07:00" {
		t.Fatalf("defaults: %+v", d)
	}
}

// TestInputOfRoundTripsTheSavedView checks that the merge base built by a PATCH is itself valid input and keeps
// the language of the saved view.
func TestInputOfRoundTripsTheSavedView(t *testing.T) {
	view := Defaults("")
	view.Language = "vi"
	input := InputOf(view)
	if input.Language != "vi" {
		t.Fatalf("InputOf language = %q, want vi", input.Language)
	}
	if err := Validate(input); err != nil {
		t.Fatalf("Validate(InputOf(defaults)) = %v, want nil", err)
	}
}
