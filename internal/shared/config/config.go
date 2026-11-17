// Package config loads the application configuration from environment
// variables. Every variable has a documented default in .env.example, so a
// command center can start with sensible values and no secret is embedded in
// the binary or committed to the repository.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// validSSLModes lists the PostgreSQL sslmode values accepted by
// SPEEDTEST_DB_SSLMODE.
var validSSLModes = map[string]bool{
	"disable":     true,
	"allow":       true,
	"prefer":      true,
	"require":     true,
	"verify-ca":   true,
	"verify-full": true,
}

// CenterConfig is everything the command center needs to run.
type CenterConfig struct {
	// HTTPAddr is the listen address for the web UI and API.
	HTTPAddr    string
	SMTPAddr    string
	SMTPFrom    string
	SMTPUser    string
	TelegramURL string
	// AppEnv controls environment-gated operations such as default seeding.
	AppEnv string
	// UIDir optionally selects a development directory instead of embedded assets.
	UIDir string
	// SessionTTL is the lifetime of an owner session.
	SessionTTL time.Duration
	// LoginMaxAttempts is the maximum login requests per IP and account per window.
	LoginMaxAttempts int
	// LoginWindow is the sliding login rate-limit window.
	LoginWindow time.Duration
	// TrustedProxyCIDRs permits client IP recovery from X-Forwarded-For only
	// when the direct peer is inside one of these configured networks.
	TrustedProxyCIDRs string
	// Database holds the PostgreSQL connection settings.
	Database DatabaseConfig
	// AppKey protects short-lived idempotent API response replays at rest.
	AppKey []byte
	// AgentRequestsPerMinute is the per-node sustained agent API rate.
	AgentRequestsPerMinute int
	// APITokenRequestsPerMinute is the sustained rate for each owner bearer token.
	APITokenRequestsPerMinute int
	// PublicURL is the address agents use to reach the center; it is written into
	// the served installer script. ReleaseDir holds the files built by `make release`.
	// Both must be set to enable GET /install.sh and GET /download/<artifact>.
	PublicURL  string
	ReleaseDir string
}

// DatabaseConfig holds the PostgreSQL connection settings.
type DatabaseConfig struct {
	Host     string
	Port     int
	Name     string
	User     string
	Password string
	SSLMode  string
	// MaxConns bounds the connection pool (SPEEDTEST_DB_MAX_CONNS, default 10).
	MaxConns int
}

// DSN returns a PostgreSQL connection string for the database. The password is
// escaped so it never breaks the URL, but the returned value itself must not
// be logged.
func (d DatabaseConfig) DSN() string {
	return fmt.Sprintf(
		"postgres://%s:%s@%s:%d/%s?sslmode=%s",
		url.QueryEscape(d.User),
		url.QueryEscape(d.Password),
		d.Host,
		d.Port,
		d.Name,
		d.SSLMode,
	)
}

// LoadCenter reads and validates the command-center configuration from the
// environment.
func LoadCenter() (CenterConfig, error) {
	port, err := getInt("SPEEDTEST_DB_PORT", 5432)
	if err != nil {
		return CenterConfig{}, err
	}
	maxConns, err := getInt("SPEEDTEST_DB_MAX_CONNS", 10)
	if err != nil {
		return CenterConfig{}, err
	}
	rateLimit, err := getInt("SPEEDTEST_AGENT_REQUESTS_PER_MINUTE", 120)
	if err != nil {
		return CenterConfig{}, err
	}
	apiTokenRateLimit, err := getInt("SPEEDTEST_API_TOKEN_REQUESTS_PER_MINUTE", 120)
	if err != nil {
		return CenterConfig{}, err
	}
	appKey, err := decodeAppKey(os.Getenv("APP_KEY"))
	if err != nil {
		return CenterConfig{}, err
	}
	sessionTTL, err := getDuration("SPEEDTEST_SESSION_TTL", 12*time.Hour)
	if err != nil {
		return CenterConfig{}, err
	}
	loginMaxAttempts, err := getInt("SPEEDTEST_LOGIN_MAX_ATTEMPTS", 5)
	if err != nil {
		return CenterConfig{}, err
	}
	loginWindow, err := getDuration("SPEEDTEST_LOGIN_WINDOW", 15*time.Minute)
	if err != nil {
		return CenterConfig{}, err
	}

	cfg := CenterConfig{
		HTTPAddr:                  getString("SPEEDTEST_HTTP_ADDR", "127.0.0.1:8080"),
		AppEnv:                    AppEnv(),
		UIDir:                     getString("SPEEDTEST_UI_DIR", ""),
		SessionTTL:                sessionTTL,
		LoginMaxAttempts:          loginMaxAttempts,
		LoginWindow:               loginWindow,
		TrustedProxyCIDRs:         getString("SPEEDTEST_TRUSTED_PROXY_CIDRS", ""),
		Database:                  centerDatabase(port, maxConns),
		AppKey:                    appKey,
		AgentRequestsPerMinute:    rateLimit,
		APITokenRequestsPerMinute: apiTokenRateLimit,
		SMTPAddr:                  getString("SPEEDTEST_SMTP_ADDR", "127.0.0.1:11025"),
		SMTPFrom:                  getString("SPEEDTEST_SMTP_FROM", "speedtest@example.com"),
		SMTPUser:                  getString("SPEEDTEST_SMTP_USER", ""),
		TelegramURL:               getString("SPEEDTEST_TELEGRAM_URL", "https://api.telegram.org"),
		PublicURL:                 strings.TrimSpace(getString("SPEEDTEST_PUBLIC_URL", "")),
		ReleaseDir:                strings.TrimSpace(getString("SPEEDTEST_RELEASE_DIR", "")),
	}
	if err := cfg.validate(); err != nil {
		return CenterConfig{}, err
	}
	return cfg, nil
}

func (c CenterConfig) validate() error {
	if (c.PublicURL == "") != (c.ReleaseDir == "") {
		return errors.New("SPEEDTEST_PUBLIC_URL and SPEEDTEST_RELEASE_DIR must be set together")
	}
	if err := validateHostPort("SPEEDTEST_HTTP_ADDR", c.HTTPAddr); err != nil {
		return err
	}
	if c.Database.Port < 1 || c.Database.Port > 65535 {
		return fmt.Errorf("SPEEDTEST_DB_PORT must be between 1 and 65535, got %d", c.Database.Port)
	}
	if c.Database.MaxConns < 1 || c.Database.MaxConns > 100 {
		return fmt.Errorf("SPEEDTEST_DB_MAX_CONNS must be between 1 and 100, got %d", c.Database.MaxConns)
	}
	if !validSSLModes[c.Database.SSLMode] {
		return fmt.Errorf("SPEEDTEST_DB_SSLMODE must be one of disable, allow, prefer, require, verify-ca, verify-full, got %q", c.Database.SSLMode)
	}
	if c.AgentRequestsPerMinute < 1 || c.AgentRequestsPerMinute > 60000 {
		return fmt.Errorf("SPEEDTEST_AGENT_REQUESTS_PER_MINUTE must be between 1 and 60000, got %d", c.AgentRequestsPerMinute)
	}
	if c.APITokenRequestsPerMinute < 1 || c.APITokenRequestsPerMinute > 60000 {
		return fmt.Errorf("SPEEDTEST_API_TOKEN_REQUESTS_PER_MINUTE must be between 1 and 60000, got %d", c.APITokenRequestsPerMinute)
	}
	if c.AppEnv != "local" && c.AppEnv != "test" && c.AppEnv != "staging" && c.AppEnv != "production" {
		return fmt.Errorf("APP_ENV must be local, test, staging or production, got %q", c.AppEnv)
	}
	if c.SessionTTL <= 0 || c.SessionTTL > 30*24*time.Hour {
		return fmt.Errorf("SPEEDTEST_SESSION_TTL must be positive and at most 720h, got %s", c.SessionTTL)
	}
	if c.LoginMaxAttempts < 1 || c.LoginMaxAttempts > 100 {
		return fmt.Errorf("SPEEDTEST_LOGIN_MAX_ATTEMPTS must be between 1 and 100, got %d", c.LoginMaxAttempts)
	}
	if c.LoginWindow <= 0 || c.LoginWindow > 24*time.Hour {
		return fmt.Errorf("SPEEDTEST_LOGIN_WINDOW must be positive and at most 24h, got %s", c.LoginWindow)
	}
	if err := validateCIDRList(c.TrustedProxyCIDRs); err != nil {
		return err
	}
	return nil
}

func validateCIDRList(raw string) error {
	for _, item := range strings.Split(raw, ",") {
		cidr := strings.TrimSpace(item)
		if cidr == "" {
			continue
		}
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return errors.New("SPEEDTEST_TRUSTED_PROXY_CIDRS must be a comma-separated list of CIDRs")
		}
	}
	return nil
}

func decodeAppKey(value string) ([]byte, error) {
	if value == "" {
		return nil, nil
	}
	encoded := strings.TrimPrefix(value, "base64:")
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("APP_KEY must be base64 encoding of exactly 32 bytes")
	}
	return key, nil
}

// validateHostPort checks that raw is a host:port address with a numeric port.
// The host may be empty (for example ":8080").
func validateHostPort(key, raw string) error {
	_, port, err := net.SplitHostPort(raw)
	if err != nil {
		return fmt.Errorf("%s must be a valid host:port address (host may be empty), got %q: %w", key, raw, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%s must have a numeric port between 1 and 65535, got %q", key, raw)
	}
	return nil
}

func getString(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getInt(key string, def int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer, got %q: %w", key, v, err)
	}
	return n, nil
}

func getDuration(key string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be a Go duration, got %q: %w", key, v, err)
	}
	return d, nil
}

func centerDatabase(port, maxConns int) DatabaseConfig {
	return DatabaseConfig{
		Host:     getString("SPEEDTEST_DB_HOST", "127.0.0.1"),
		Port:     port,
		Name:     getString("SPEEDTEST_DB_NAME", "speedtest_test"),
		User:     getString("SPEEDTEST_DB_USER", "speedtest"),
		Password: getString("SPEEDTEST_DB_PASSWORD", ""),
		SSLMode:  getString("SPEEDTEST_DB_SSLMODE", "disable"),
		MaxConns: maxConns,
	}
}

// AppEnv is the one fact the program knows about where it runs: local, test, staging or production.
// Everything else (hosts, names, credentials) comes from the environment and is used as given.
func AppEnv() string {
	return strings.ToLower(strings.TrimSpace(getString("APP_ENV", "local")))
}
