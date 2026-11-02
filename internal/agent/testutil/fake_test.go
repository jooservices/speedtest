package testutil

import (
	"context"
	"testing"
)

func TestRunFakeSpeedtest(t *testing.T) {
	for _, scenario := range []string{"success", "slow", "loss", "crash"} {
		out, err := RunFakeSpeedtest(t, context.Background(), scenario)
		if scenario == "crash" {
			if err == nil {
				t.Fatal("crash succeeded")
			}
			continue
		}
		if err != nil || len(out) == 0 {
			t.Fatalf("%s: %s %v", scenario, out, err)
		}
	}
}
