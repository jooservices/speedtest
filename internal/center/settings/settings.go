// Package settings stores and applies owner preferences and integration
// configuration for the command center. Public preferences are stored as JSON;
// recoverable integration secrets are encrypted at rest with AES-GCM using the
// application key and are reported to callers only as "configured" flags.
//
// Defaults match the UI preview described in docs/ui-decisions.md and
// docs/api.md. Nodes never read these settings; a node knows only its own
// configuration.
package settings

import (
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"

	// Embed the IANA time zone database so timezone validation works on hosts
	// without a system zoneinfo directory.
	_ "time/tzdata"
)

// Enrollment key values are reported only as a configured flag; the key itself
// is disclosed once at issuance.
const (
	// Enrollment token prefix shown to operators, matching the preview.
	enrollmentPrefix = "st_enroll_"
)

// Setting keys in the PostgreSQL settings table.
const (
	keyDisplay   = "display"
	keyEnrolment = "enrolment"
	keyGeoSecret = "geolite2.license"
)

// Notification channel secret keys.
const (
	keyEmailSecret    = "notify.email.secret"
	keyTelegramSecret = "notify.telegram.secret"
	keySlackSecret    = "notify.slack.secret"
	keyWebhookSecret  = "notify.webhook.secret"
)

// ChannelConfig is the read model for one notification channel. It never
// exposes stored secrets; a caller learns only whether a secret is configured.
type ChannelConfig struct {
	On         bool   `json:"on"`
	To         string `json:"to"`
	Configured bool   `json:"configured"`
}

// NotificationSettings holds the read model for all notification channels.
type NotificationSettings struct {
	Email    ChannelConfig `json:"email"`
	Telegram ChannelConfig `json:"telegram"`
	Slack    ChannelConfig `json:"slack"`
	Webhook  ChannelConfig `json:"webhook"`
}

// Settings is the public read model returned by the settings API. Credentials
// are represented by configured flags, never by masked placeholder values.
type Settings struct {
	Theme               string               `json:"theme"`
	Range               string               `json:"range"`
	Units               string               `json:"units"`
	Timezone            string               `json:"timezone"`
	Hour12              bool                 `json:"hour12"`
	Decimals            int                  `json:"decimals"`
	RetentionDays       int                  `json:"retentionDays"`
	GeoProvider         string               `json:"geoProvider"`
	GeoFallback         string               `json:"geoFallback"`
	GeoAutoUpdate       bool                 `json:"geoAutoUpdate"`
	Engine              string               `json:"engine"`
	DefaultThreads      int                  `json:"defaultThreads"`
	ServerMode          string               `json:"serverMode"`
	MaxConcurrent       int                  `json:"maxConcurrent"`
	Stagger             bool                 `json:"stagger"`
	QuietStart          string               `json:"quietStart"`
	QuietEnd            string               `json:"quietEnd"`
	CenterURL           string               `json:"centerUrl"`
	GeoKeyConfigured    bool                 `json:"geoKeyConfigured"`
	EnrollKeyConfigured bool                 `json:"enrollKeyConfigured"`
	Notify              NotificationSettings `json:"notify"`
}

// ChannelWrite is the write model for one notification channel. A nil Secret
// preserves the stored value; a non-nil Secret replaces it.
type ChannelWrite struct {
	On     bool    `json:"on"`
	To     string  `json:"to"`
	Secret *string `json:"secret,omitempty"`
}

// NotificationSettingsWrite holds the write model for all channels.
type NotificationSettingsWrite struct {
	Email    ChannelWrite `json:"email"`
	Telegram ChannelWrite `json:"telegram"`
	Slack    ChannelWrite `json:"slack"`
	Webhook  ChannelWrite `json:"webhook"`
}

// SettingsInput is the write model for PUT /api/settings. It replaces all
// non-secret preferences; optional secrets replace only when supplied.
type SettingsInput struct {
	Theme          string                    `json:"theme"`
	Range          string                    `json:"range"`
	Units          string                    `json:"units"`
	Timezone       string                    `json:"timezone"`
	Hour12         bool                      `json:"hour12"`
	Decimals       int                       `json:"decimals"`
	RetentionDays  int                       `json:"retentionDays"`
	GeoProvider    string                    `json:"geoProvider"`
	GeoFallback    string                    `json:"geoFallback"`
	GeoAutoUpdate  bool                      `json:"geoAutoUpdate"`
	Engine         string                    `json:"engine"`
	DefaultThreads int                       `json:"defaultThreads"`
	ServerMode     string                    `json:"serverMode"`
	MaxConcurrent  int                       `json:"maxConcurrent"`
	Stagger        bool                      `json:"stagger"`
	QuietStart     string                    `json:"quietStart"`
	QuietEnd       string                    `json:"quietEnd"`
	CenterURL      string                    `json:"centerUrl"`
	GeoKey         *string                   `json:"geoKey,omitempty"`
	Notify         NotificationSettingsWrite `json:"notify"`
}

// IssuedKey is a newly generated enrolment key, shown once at issuance.
type IssuedKey struct {
	EnrollToken string `json:"enrollToken"`
	CreatedAt   string `json:"createdAt"`
}

// Defaults returns the settings that match the UI preview. centerURL is the
// configured public origin from deployment configuration (empty until set).
func Defaults(centerURL string) Settings {
	return Settings{
		Theme:          "dark",
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
		CenterURL:      centerURL,
		Notify: NotificationSettings{
			Email: ChannelConfig{On: true, To: "ops@example.com"},
		},
	}
}

// ValidationError reports invalid fields keyed by their JSON path.
type ValidationError struct {
	Fields map[string][]string
}

func (e *ValidationError) Error() string { return "settings validation failed" }

// Validate checks SettingsInput against the documented schema, returning a
// *ValidationError with field-level messages when any value is invalid.
func Validate(input SettingsInput) error {
	fields := map[string][]string{}
	add := func(field, message string) {
		fields[field] = append(fields[field], message)
	}

	switch input.Theme {
	case "dark", "light":
	default:
		add("theme", "must be dark or light")
	}
	switch input.Range {
	case "6h", "24h", "7d", "30d":
	default:
		add("range", "must be one of 6h, 24h, 7d, 30d")
	}
	switch input.Units {
	case "auto", "Mbps", "Gbps", "MBps":
	default:
		add("units", "must be one of auto, Mbps, Gbps, MBps")
	}
	if err := validTimezone(input.Timezone); err != nil {
		add("timezone", err.Error())
	}
	if input.Decimals < 0 || input.Decimals > 2 {
		add("decimals", "must be between 0 and 2")
	}
	switch input.RetentionDays {
	case 0, 30, 90, 365, 1095:
	default:
		add("retentionDays", "must be one of 0, 30, 90, 365, 1095")
	}
	switch input.GeoProvider {
	case "geolite2", "ipinfo", "ip-api":
	default:
		add("geoProvider", "must be one of geolite2, ipinfo, ip-api")
	}
	switch input.GeoFallback {
	case "none", "ip-api", "ipinfo":
	default:
		add("geoFallback", "must be one of none, ip-api, ipinfo")
	}
	switch input.Engine {
	case "ookla", "librespeed", "cloudflare":
	default:
		add("engine", "must be one of ookla, librespeed, cloudflare")
	}
	if input.DefaultThreads < 1 || input.DefaultThreads > 64 {
		add("defaultThreads", "must be between 1 and 64")
	}
	switch input.ServerMode {
	case "auto", "pinned":
	default:
		add("serverMode", "must be auto or pinned")
	}
	if input.MaxConcurrent < 1 || input.MaxConcurrent > 5 {
		add("maxConcurrent", "must be between 1 and 5")
	}
	if field, message, ok := validQuietHours(input.QuietStart, input.QuietEnd); !ok {
		add(field, message)
	}
	if err := validCenterURL(input.CenterURL); err != nil {
		add("centerUrl", err.Error())
	}
	if err := validSecret("geoKey", input.GeoKey); err != nil {
		add("geoKey", err.Error())
	}
	validateChannel(fields, "notify.email", input.Notify.Email, emailChannel)
	validateChannel(fields, "notify.telegram", input.Notify.Telegram, telegramChannel)
	validateChannel(fields, "notify.slack", input.Notify.Slack, webhookChannel)
	validateChannel(fields, "notify.webhook", input.Notify.Webhook, webhookChannel)

	if len(fields) > 0 {
		return &ValidationError{Fields: fields}
	}
	return nil
}

// channelKind describes how a notification destination is validated.
type channelKind int

const (
	emailChannel channelKind = iota
	telegramChannel
	webhookChannel
)

func validateChannel(fields map[string][]string, path string, ch ChannelWrite, kind channelKind) {
	if len(ch.To) > 320 {
		fields[path+".to"] = append(fields[path+".to"], "must be at most 320 characters")
	}
	switch kind {
	case emailChannel:
		if ch.To != "" && !validEmail(ch.To) {
			fields[path+".to"] = append(fields[path+".to"], "must be a valid email address")
		}
	case telegramChannel:
		if ch.To != "" && !telegramChatID.MatchString(ch.To) {
			fields[path+".to"] = append(fields[path+".to"], "must be a Telegram chat ID")
		}
	case webhookChannel:
		if ch.To != "" {
			fields[path+".to"] = append(fields[path+".to"], "must be empty; supply the webhook URL as secret")
		}
		if err := validSecret(path+".secret", ch.Secret); err != nil {
			fields[path+".secret"] = append(fields[path+".secret"], err.Error())
			return
		}
		if ch.Secret != nil && !validHTTPURL(*ch.Secret) {
			fields[path+".secret"] = append(fields[path+".secret"], "must be a valid http or https URL")
		}
		return
	}
	if err := validSecret(path+".secret", ch.Secret); err != nil {
		fields[path+".secret"] = append(fields[path+".secret"], err.Error())
	}
}

var telegramChatID = regexp.MustCompile(`^-?\d{1,32}$`)

func validEmail(raw string) bool {
	addr, err := mail.ParseAddress(raw)
	return err == nil && addr.Address == raw && strings.Contains(raw, "@")
}

// validSecret enforces the write-only Secret constraints: non-empty and at most
// 8192 bytes when supplied.
func validSecret(field string, secret *string) error {
	if secret == nil {
		return nil
	}
	if *secret == "" {
		return errors.New("must not be empty")
	}
	if len(*secret) > 8192 {
		return errors.New("must be at most 8192 characters")
	}
	return nil
}

func validHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// validTimezone accepts "browser" (a UI preference) or a valid IANA time zone.
func validTimezone(name string) error {
	if name == "" {
		return errors.New("is required")
	}
	if name == "browser" {
		return nil
	}
	if _, err := time.LoadLocation(name); err != nil {
		return fmt.Errorf("must be browser or a valid IANA time zone, got %q", name)
	}
	return nil
}

var quietHour = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

// validQuietHours returns the invalid field and message, or ok when both
// endpoints are empty or both are valid distinct HH:mm values.
func validQuietHours(start, end string) (field, message string, ok bool) {
	switch {
	case start == "" && end == "":
		return "", "", true
	case start == "" || end == "":
		return "quietEnd", "quietStart and quietEnd must both be empty or both be set", false
	case !quietHour.MatchString(start):
		return "quietStart", "must be empty or HH:mm", false
	case !quietHour.MatchString(end):
		return "quietEnd", "must be empty or HH:mm", false
	case start == end:
		return "quietEnd", "quietStart and quietEnd must differ", false
	default:
		return "", "", true
	}
}

// validCenterURL requires an absolute http(s) URL; plain HTTP is permitted only
// for loopback/test addresses. An empty value means not yet configured.
func validCenterURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return errors.New("must be an absolute http or https URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("must be http or https")
	}
	if u.Scheme == "http" && !loopbackHost(u.Hostname()) {
		return errors.New("http is only allowed for loopback or test addresses")
	}
	return nil
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
