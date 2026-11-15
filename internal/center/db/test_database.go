package db

import (
	"errors"
	"regexp"
)

// A worktree database is either the legacy speedtest_wt<id> or the wt_<slug> database that scripts/db/tree creates for a ticket.
var testDatabaseName = regexp.MustCompile(`^(speedtest_wt[a-z0-9]{1,12}|wt_[a-z0-9_]{2,35})$`)

// CheckTestDatabase rejects database names outside Speedtest's test namespace.
// Callers must also verify the connected database identity and ownership marker.
func CheckTestDatabase(name string) error {
	if name == "speedtest_test" || testDatabaseName.MatchString(name) {
		return nil
	}
	return errors.New("refusing database outside the Speedtest test namespace")
}
