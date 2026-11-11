// Package testenv applies values from a test environment file without replacing explicit process overrides.
package testenv

import (
	"os"
	"testing"
)

// Apply sets only environment values that the calling process has not already provided.
func Apply(t *testing.T, values map[string]string) {
	t.Helper()
	for key, value := range values {
		if _, exists := os.LookupEnv(key); !exists {
			t.Setenv(key, value)
		}
	}
}
