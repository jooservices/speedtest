// Package config loads a node's own configuration from a YAML file. A node
// knows only its own configuration (name, expected speeds and interval) and
// never what it is attached to. Invalid values fail fast with a clear message.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultPath is where the node looks for its configuration file when
// SPEEDTEST_AGENT_CONFIG is not set.
const DefaultPath = "/etc/speedtest-agent/config.yaml"

// Config is everything a single node needs to run.
type Config struct {
	// Name is the human-readable node name shown in the command center.
	Name string
	// ExpectedDownloadMbps and ExpectedUploadMbps are the speeds the node is
	// compared against, in megabits per second.
	ExpectedDownloadMbps int
	ExpectedUploadMbps   int
	// Interval is how often the node runs a speed test.
	Interval time.Duration
}

// fileConfig is the YAML shape. Pointer fields tell a missing key apart from
// an explicit zero value, so a file can override only some of the defaults.
type fileConfig struct {
	Name                 *string `yaml:"name"`
	ExpectedDownloadMbps *int    `yaml:"expected_download_mbps"`
	ExpectedUploadMbps   *int    `yaml:"expected_upload_mbps"`
	Interval             *string `yaml:"interval"`
}

// Load reads the configuration file at path. A missing file is allowed and
// yields the documented defaults; a malformed file or invalid value is an
// error.
func Load(path string) (Config, error) {
	cfg := Config{
		Name:                 "node",
		ExpectedDownloadMbps: 0,
		ExpectedUploadMbps:   0,
		Interval:             30 * time.Minute,
	}

	if err := applyFile(path, &cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// applyFile overlays the YAML file onto cfg. A missing file is ignored; any
// other read error or a parse error is returned.
func applyFile(path string, cfg *Config) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read config %s: %w", path, err)
	}

	var f fileConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("parse config %s: %w", path, err)
	}

	if f.Name != nil {
		cfg.Name = strings.TrimSpace(*f.Name)
	}
	if f.ExpectedDownloadMbps != nil {
		cfg.ExpectedDownloadMbps = *f.ExpectedDownloadMbps
	}
	if f.ExpectedUploadMbps != nil {
		cfg.ExpectedUploadMbps = *f.ExpectedUploadMbps
	}
	if f.Interval != nil {
		d, err := parseDuration("interval", *f.Interval)
		if err != nil {
			return err
		}
		cfg.Interval = d
	}
	return nil
}

func (c Config) validate() error {
	if c.Name == "" {
		return errors.New("name must not be blank")
	}
	if c.ExpectedDownloadMbps < 0 {
		return fmt.Errorf("expected_download_mbps must be >= 0, got %d", c.ExpectedDownloadMbps)
	}
	if c.ExpectedUploadMbps < 0 {
		return fmt.Errorf("expected_upload_mbps must be >= 0, got %d", c.ExpectedUploadMbps)
	}
	if c.Interval <= 0 {
		return fmt.Errorf("interval must be greater than 0, got %s", c.Interval)
	}
	return nil
}

// lookup returns the non-empty value of key, reporting whether it was set.
func parseDuration(key, v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration, got %q: %w", key, v, err)
	}
	return d, nil
}
