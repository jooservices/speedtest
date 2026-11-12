package geo

import (
	"testing"
	"time"
)

func TestBackfillConfigAndNextSchedule(t *testing.T) {
	for _, key := range []string{"SPEEDTEST_GEO_BACKFILL_MAX_PER_NIGHT", "SPEEDTEST_GEO_BACKFILL_BATCH_SIZE", "SPEEDTEST_GEO_BACKFILL_PAUSE", "SPEEDTEST_GEO_BACKFILL_HOUR_UTC"} {
		t.Setenv(key, "")
	}
	cfg, err := BackfillConfigFromEnv()
	if err != nil || cfg.MaxPerRun != 100 || cfg.BatchSize != 100 || cfg.Pause != 10*time.Second || cfg.HourUTC != 2 {
		t.Fatalf("default config = %+v, %v", cfg, err)
	}
	for key, value := range map[string]string{
		"SPEEDTEST_GEO_BACKFILL_MAX_PER_NIGHT": "0",
		"SPEEDTEST_GEO_BACKFILL_BATCH_SIZE":    "101",
		"SPEEDTEST_GEO_BACKFILL_PAUSE":         "-1s",
		"SPEEDTEST_GEO_BACKFILL_HOUR_UTC":      "24",
	} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, value)
			if _, err := BackfillConfigFromEnv(); err == nil {
				t.Fatal("invalid value accepted")
			}
		})
	}
	now := time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC)
	if got := nextBackfillAt(now, 2); !got.Equal(now) {
		t.Fatalf("schedule at boundary = %v, want %v", got, now)
	}
	if got := nextBackfillAt(now.Add(time.Second), 2); !got.Equal(now.AddDate(0, 0, 1)) {
		t.Fatalf("next daily schedule = %v", got)
	}
}

func TestReadEnvIntReportsInvalidValue(t *testing.T) {
	t.Setenv("SPEEDTEST_GEO_BACKFILL_MAX_PER_NIGHT", "x")
	if _, err := readEnvInt("SPEEDTEST_GEO_BACKFILL_MAX_PER_NIGHT", 100); err == nil {
		t.Fatal("invalid integer accepted")
	}
}
