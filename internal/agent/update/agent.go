package update

import (
	"errors"
	"log/slog"
	"os"
	"strings"
	"time"
)

// Agent is the unprivileged side: it decides whether an update is wanted and asks for a restart.
type Agent struct {
	Paths   Paths
	Version string
	Logger  *slog.Logger
	Now     func() time.Time
	// Restart asks the process to exit cleanly so the service manager starts the new version.
	Restart func()
}

func (a *Agent) now() time.Time {
	if a.Now != nil {
		return a.Now().UTC()
	}
	return time.Now().UTC()
}

func (a *Agent) logger() *slog.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return slog.Default()
}

// Result returns the outcome of the latest update attempt, or nil when there has been none.
func (a *Agent) Result() *Result { return loadResult(a.Paths) }

// Offer is called with each configuration: it requests an update when the owner enabled auto-update and the
// center advertises a different, valid version that has not recently failed. It reports whether a restart
// was requested.
func (a *Agent) Offer(autoUpdate bool, rel *Release) bool {
	if !autoUpdate || rel == nil {
		return false
	}
	return a.request(*rel) == nil
}

// Command handles an owner's update_agent command for the given version; it works even with auto-update off.
func (a *Agent) Command(version string, rel *Release) error {
	if rel == nil {
		return errors.New("the center advertises no release")
	}
	if rel.Version != version {
		return errors.New("the requested version is not the advertised release")
	}
	return a.request(*rel)
}

func (a *Agent) request(rel Release) error {
	if err := rel.Validate(); err != nil {
		return err
	}
	if rel.Version == a.Version {
		return errNoUpdate
	}
	state := loadState(a.Paths)
	switch {
	case state.Status == statePending:
		return errors.New("an update is already waiting for confirmation")
	case state.hasFailed(rel.Version):
		return errors.New("this version already failed and is not retried")
	}
	if r := loadResult(a.Paths); r != nil && r.Version == rel.Version && r.Status != StatusSucceeded {
		if ts, err := time.Parse(time.RFC3339, r.TS); err == nil && a.now().Sub(ts) < RetryAfter {
			return errors.New("this version failed recently; retrying later")
		}
	}
	if _, err := readSmall(a.Paths.request()); err == nil {
		return errors.New("an update request is already queued")
	}
	if err := os.MkdirAll(a.Paths.InboxDir, 0o700); err != nil {
		return wrap("prepare update inbox", err)
	}
	if err := writeJSON(a.Paths.request(), request{Version: rel.Version, RequestedAt: fmtTime(a.now())}, 0o600); err != nil {
		return wrap("write update request", err)
	}
	a.logger().Info("agent update requested; restarting to apply it", "from", a.Version, "to", rel.Version)
	if a.Restart != nil {
		a.Restart()
	}
	return nil
}

// Confirm is called after a heartbeat succeeds. When this process is the version waiting for confirmation it
// leaves the marker that lets the privileged step keep the update.
func (a *Agent) Confirm() {
	state := loadState(a.Paths)
	if state.Status != statePending || state.Version != a.Version {
		return
	}
	if err := os.MkdirAll(a.Paths.InboxDir, 0o700); err != nil {
		a.logger().Warn("cannot confirm update", "error", err.Error())
		return
	}
	if err := writeAtomic(a.Paths.confirm(), []byte(a.Version), 0o600); err != nil {
		a.logger().Warn("cannot confirm update", "error", err.Error())
		return
	}
	a.logger().Info("agent update confirmed", "version", a.Version)
}

// Expired reports whether this process is a freshly installed version that failed to confirm in time. The
// caller should then exit with an error so the privileged step restores the previous binary.
func (a *Agent) Expired() bool {
	state := loadState(a.Paths)
	if state.Status != statePending || state.Version != a.Version {
		return false
	}
	if data, err := readSmall(a.Paths.confirm()); err == nil && strings.TrimSpace(string(data)) == a.Version {
		return false
	}
	return a.now().After(state.Deadline)
}
