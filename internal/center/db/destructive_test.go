package db

import "testing"

func TestCheckDestructiveAllowed(t *testing.T) {
	for _, env := range []string{"local", "test"} {
		if err := CheckDestructiveAllowed(env); err != nil {
			t.Fatalf("APP_ENV=%q refused: %v", env, err)
		}
	}
	for _, env := range []string{"production", "staging", "", "Local", "prod"} {
		if err := CheckDestructiveAllowed(env); err == nil {
			t.Fatalf("APP_ENV=%q accepted", env)
		}
	}
}
