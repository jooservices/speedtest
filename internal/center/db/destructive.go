package db

import "errors"

// CheckDestructiveAllowed refuses an operation that drops or rewrites data (reset, test fixtures) unless
// the environment is local or test. It knows nothing about where the database lives or how it was made.
func CheckDestructiveAllowed(appEnv string) error {
	if appEnv == "local" || appEnv == "test" {
		return nil
	}
	return errors.New("destructive database operations are allowed only when APP_ENV is local or test")
}
