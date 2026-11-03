package report

import (
	"errors"
	"os"
	"strconv"
	"time"
)

// DefaultDir is where the node keeps its result spool when SPEEDTEST_SPOOL_DIR
// is not set.
const DefaultDir = "/etc/speedtest-agent/spool"

// SpoolConfigFromEnv reads the spool directory and caps exported from .env by
// the launcher. Unset variables keep the documented defaults.
func SpoolConfigFromEnv() (SpoolConfig, error) {
	c := SpoolConfig{
		Dir:        os.Getenv("SPEEDTEST_SPOOL_DIR"),
		MaxResults: DefaultMaxResults,
		MaxBytes:   DefaultMaxBytes,
		MaxAge:     DefaultMaxAge,
	}
	if c.Dir == "" {
		c.Dir = DefaultDir
	}
	if raw := os.Getenv("SPEEDTEST_SPOOL_MAX_RESULTS"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v <= 0 {
			return SpoolConfig{}, errors.New("SPEEDTEST_SPOOL_MAX_RESULTS must be a positive integer")
		}
		c.MaxResults = v
	}
	if raw := os.Getenv("SPEEDTEST_SPOOL_MAX_BYTES"); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v <= 0 {
			return SpoolConfig{}, errors.New("SPEEDTEST_SPOOL_MAX_BYTES must be a positive integer")
		}
		c.MaxBytes = v
	}
	if raw := os.Getenv("SPEEDTEST_SPOOL_MAX_AGE"); raw != "" {
		v, err := time.ParseDuration(raw)
		if err != nil || v <= 0 {
			return SpoolConfig{}, errors.New("SPEEDTEST_SPOOL_MAX_AGE must be a positive duration")
		}
		c.MaxAge = v
	}
	return c, nil
}
