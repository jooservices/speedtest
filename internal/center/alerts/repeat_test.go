package alerts

import (
	"testing"
	"time"
)

func TestShouldRepeatNotificationFallsBackToTheBreachOnset(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	hourAgo, twoHoursAgo, minuteAgo := now.Add(-time.Hour), now.Add(-2*time.Hour), now.Add(-time.Minute)
	for name, tc := range map[string]struct {
		last, onset *time.Time
		want        bool
	}{
		"last delivery is old enough":              {&twoHoursAgo, &twoHoursAgo, true},
		"last delivery is recent":                  {&minuteAgo, &twoHoursAgo, false},
		"nothing delivered, breach is old enough":  {nil, &twoHoursAgo, true},
		"nothing delivered, breach is recent":      {nil, &minuteAgo, false},
		"nothing delivered and no onset known":     {nil, nil, false},
		"exactly at the repeat interval is due":    {&hourAgo, nil, true},
		"the last delivery wins over an old onset": {&minuteAgo, &twoHoursAgo, false},
	} {
		if got := shouldRepeatNotification(now, tc.last, tc.onset, 3600); got != tc.want {
			t.Errorf("%s: got %t want %t", name, got, tc.want)
		}
	}
}
