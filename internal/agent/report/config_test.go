package report

import (
	"testing"
	"time"
)

func TestSpoolEnvironment(t *testing.T) {
	keys := []string{"SPEEDTEST_SPOOL_DIR", "SPEEDTEST_SPOOL_MAX_RESULTS", "SPEEDTEST_SPOOL_MAX_BYTES", "SPEEDTEST_SPOOL_MAX_AGE"}
	for _, k := range keys {
		t.Setenv(k, "")
	}
	c, err := SpoolConfigFromEnv()
	if err != nil || c.Dir != DefaultDir || c.MaxBytes != DefaultMaxBytes || c.MaxAge != DefaultMaxAge || c.MaxResults != DefaultMaxResults {
		t.Fatal(c, err)
	}
	t.Setenv(keys[0], "private-spool")
	t.Setenv(keys[1], "5")
	t.Setenv(keys[2], "4096")
	t.Setenv(keys[3], "2h")
	c, err = SpoolConfigFromEnv()
	if err != nil || c.Dir != "private-spool" || c.MaxResults != 5 || c.MaxBytes != 4096 || c.MaxAge != 2*time.Hour {
		t.Fatal(c, err)
	}
	for _, k := range keys[1:] {
		t.Run(k, func(t *testing.T) {
			for _, v := range []string{"garbage", "0", "-1"} {
				t.Setenv(k, v)
				if _, err := SpoolConfigFromEnv(); err == nil {
					t.Fatal(k, v)
				}
			}
		})
	}
}
