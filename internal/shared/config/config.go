// Package config loads the application configuration from environment
// variables. Every variable has a documented default in .env.example, so a
// node or the center can start with sensible values and no secret is embedded
// in the binary or committed to the repository.
package config

import (
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

// AgentConfig is everything a single node needs to run. A node knows only its
// own configuration and never what it is attached to.
type AgentConfig struct {
	// Name is the human-readable node name shown in the command center.
	Name string
	// ExpectedDownloadMbps and ExpectedUploadMbps are the speeds the node is
	// compared against, in megabits per second.
	ExpectedDownloadMbps int
	ExpectedUploadMbps   int
	// Interval is how often the node runs a speed test.
	Interval time.Duration
	// CenterURL is the command-center URL the node reports its results to.
	CenterURL string
}

// CenterConfig is everything the command center needs to run.
type CenterConfig struct {
	// HTTPAddr is the listen address for the web UI and API.
	HTTPAddr string
	// UIDir optionally selects a development directory instead of embedded assets.
	UIDir string
	// Database holds the PostgreSQL connection settings.
	Database DatabaseConfig
}

// DatabaseConfig holds the PostgreSQL connection settings.
type DatabaseConfig struct {
	Host     string
	Port     int
	Name     string
	User     string
	Password string
	SSLMode  string
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

// LoadAgent reads and validates the node configuration from the environment.
func LoadAgent() (AgentConfig, error) {
	download, err := getInt("SPEEDTEST_NODE_EXPECTED_DOWNLOAD_MBPS", 0)
	if err != nil {
		return AgentConfig{}, err
	}
	upload, err := getInt("SPEEDTEST_NODE_EXPECTED_UPLOAD_MBPS", 0)
	if err != nil {
		return AgentConfig{}, err
	}
	interval, err := getDuration("SPEEDTEST_NODE_INTERVAL", 30*time.Minute)
	if err != nil {
		return AgentConfig{}, err
	}

	cfg := AgentConfig{
		Name:                 strings.TrimSpace(getString("SPEEDTEST_NODE_NAME", "node")),
		ExpectedDownloadMbps: download,
		ExpectedUploadMbps:   upload,
		Interval:             interval,
		CenterURL:            getString("SPEEDTEST_CENTER_URL", "http://127.0.0.1:8080"),
	}
	if err := cfg.validate(); err != nil {
		return AgentConfig{}, err
	}
	return cfg, nil
}

func (c AgentConfig) validate() error {
	if c.Name == "" {
		return fmt.Errorf("SPEEDTEST_NODE_NAME must not be blank")
	}
	if c.ExpectedDownloadMbps < 0 {
		return fmt.Errorf("SPEEDTEST_NODE_EXPECTED_DOWNLOAD_MBPS must be >= 0, got %d", c.ExpectedDownloadMbps)
	}
	if c.ExpectedUploadMbps < 0 {
		return fmt.Errorf("SPEEDTEST_NODE_EXPECTED_UPLOAD_MBPS must be >= 0, got %d", c.ExpectedUploadMbps)
	}
	if c.Interval <= 0 {
		return fmt.Errorf("SPEEDTEST_NODE_INTERVAL must be greater than 0, got %s", c.Interval)
	}
	return validateAbsoluteHTTPURL("SPEEDTEST_CENTER_URL", c.CenterURL)
}

// LoadCenter reads and validates the command-center configuration from the
// environment.
func LoadCenter() (CenterConfig, error) {
	port, err := getInt("SPEEDTEST_DB_PORT", 5432)
	if err != nil {
		return CenterConfig{}, err
	}

	cfg := CenterConfig{
		HTTPAddr: getString("SPEEDTEST_HTTP_ADDR", "127.0.0.1:8080"),
		UIDir:    getString("SPEEDTEST_UI_DIR", ""),
		Database: DatabaseConfig{
			Host:     getString("SPEEDTEST_DB_HOST", "127.0.0.1"),
			Port:     port,
			Name:     getString("SPEEDTEST_DB_NAME", "speedtest"),
			User:     getString("SPEEDTEST_DB_USER", "speedtest"),
			Password: getString("SPEEDTEST_DB_PASSWORD", ""),
			SSLMode:  getString("SPEEDTEST_DB_SSLMODE", "disable"),
		},
	}
	if err := cfg.validate(); err != nil {
		return CenterConfig{}, err
	}
	return cfg, nil
}

func (c CenterConfig) validate() error {
	if err := validateHostPort("SPEEDTEST_HTTP_ADDR", c.HTTPAddr); err != nil {
		return err
	}
	if c.Database.Port < 1 || c.Database.Port > 65535 {
		return fmt.Errorf("SPEEDTEST_DB_PORT must be between 1 and 65535, got %d", c.Database.Port)
	}
	if !validSSLModes[c.Database.SSLMode] {
		return fmt.Errorf("SPEEDTEST_DB_SSLMODE must be one of disable, allow, prefer, require, verify-ca, verify-full, got %q", c.Database.SSLMode)
	}
	return nil
}

// validateAbsoluteHTTPURL checks that raw is an absolute http or https URL.
func validateAbsoluteHTTPURL(key, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s must be an absolute http or https URL, got %q: %w", key, raw, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s must be an absolute http or https URL, got %q", key, raw)
	}
	return nil
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
		return 0, fmt.Errorf("%s must be a duration, got %q: %w", key, v, err)
	}
	return d, nil
}
