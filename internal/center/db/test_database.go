package db

import (
	"errors"
	"regexp"
)

var testDatabaseName = regexp.MustCompile(`^speedtest_wt[a-z0-9]{1,12}$`)

// CheckTestDatabase rejects database names outside Speedtest's test namespace.
// Callers must also verify the connected database identity and ownership marker.
func CheckTestDatabase(name string) error {
	if name == "speedtest_test" || testDatabaseName.MatchString(name) {
		return nil
	}
	return errors.New("refusing database outside the Speedtest test namespace")
}
