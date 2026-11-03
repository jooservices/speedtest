// Package update implements the agent's self-update.
//
// The work is split by privilege. The running agent is unprivileged: it only decides that an update is
// wanted and leaves a request in its inbox, then exits so systemd restarts the service. A privileged
// ExecStartPre step (Apply) then fetches the advertised release itself, verifies its checksum and ELF
// header, replaces the binary atomically while keeping the previous one, and later either confirms the
// update or restores the previous binary. Nothing the unprivileged agent writes is trusted beyond the
// request for a version: the release URL and checksum are always taken from the command center.
package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

const (
	// ConfirmWindow is how long a new version has to complete a heartbeat before it is rolled back.
	ConfirmWindow = 2 * time.Minute
	// RetryAfter is how long a failed or rolled-back version is not tried again.
	RetryAfter = 6 * time.Hour

	// Result statuses, as in the AgentUpdateResult contract.
	StatusSucceeded  = "succeeded"
	StatusFailed     = "failed"
	StatusRolledBack = "rolled_back"

	statePending = "pending_confirm"
	stateIdle    = "idle"

	maxSmallFile = 8 << 10
)

// Release is the release the command center advertises in the agent configuration.
type Release struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
}

var (
	versionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	sha256Pattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Validate checks that the advertised release is well formed.
func (r Release) Validate() error {
	switch {
	case !versionPattern.MatchString(r.Version):
		return errors.New("release version is invalid")
	case !sha256Pattern.MatchString(r.SHA256):
		return errors.New("release sha256 must be 64 lowercase hex digits")
	case r.URL == "":
		return errors.New("release url is required")
	}
	return nil
}

// Result is the outcome of the latest update attempt, reported in the heartbeat.
type Result struct {
	Version string  `json:"version"`
	Status  string  `json:"status"`
	TS      string  `json:"ts"`
	Error   *string `json:"error"`
}

// State is the privileged side's record of an update in progress and of versions that must not be retried.
type State struct {
	Status   string    `json:"status"`
	Version  string    `json:"version,omitempty"`
	Previous string    `json:"previous,omitempty"`
	Deadline time.Time `json:"deadline,omitempty"`
	Failed   []string  `json:"failed,omitempty"`
}

func (s State) hasFailed(version string) bool {
	for _, v := range s.Failed {
		if v == version {
			return true
		}
	}
	return false
}

// Paths locate the files the update uses.
type Paths struct {
	// Binary is the installed agent executable; Binary+".prev" keeps the previous version.
	Binary string
	// StateDir is root-owned: state.json and result.json live here and are world-readable.
	StateDir string
	// InboxDir is writable by the agent: request.json and confirmed live here and are never trusted blindly.
	InboxDir string
}

// DefaultPaths are the locations used by the installed service; environment variables override them.
func DefaultPaths() Paths {
	get := func(key, def string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return def
	}
	return Paths{
		Binary:   get("SPEEDTEST_AGENT_BINARY", "/usr/local/bin/speedtest-agent"),
		StateDir: get("SPEEDTEST_AGENT_UPDATE_DIR", "/var/lib/speedtest-agent-update"),
		InboxDir: get("SPEEDTEST_AGENT_INBOX_DIR", "/var/lib/speedtest-agent/inbox"),
	}
}

func (p Paths) state() string   { return filepath.Join(p.StateDir, "state.json") }
func (p Paths) result() string  { return filepath.Join(p.StateDir, "result.json") }
func (p Paths) request() string { return filepath.Join(p.InboxDir, "request.json") }
func (p Paths) confirm() string { return filepath.Join(p.InboxDir, "confirmed") }

// Previous is where the replaced binary is kept until the new one is confirmed.
func (p Paths) Previous() string { return p.Binary + ".prev" }

// readSmall reads a small regular file without following symbolic links, so an agent-owned directory
// cannot be used to make the privileged step read something else.
func readSmall(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	return io.ReadAll(io.LimitReader(f, maxSmallFile))
}

func readJSON(path string, v any) error {
	data, err := readSmall(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// writeAtomic writes a file with a temporary name and renames it into place.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Chmod(mode)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}

func writeJSON(path string, v any, mode os.FileMode) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeAtomic(path, data, mode)
}

func loadState(p Paths) State {
	var s State
	if err := readJSON(p.state(), &s); err != nil || (s.Status != statePending && s.Status != stateIdle) {
		return State{Status: stateIdle}
	}
	return s
}

func loadResult(p Paths) *Result {
	var r Result
	if err := readJSON(p.result(), &r); err != nil || r.Version == "" {
		return nil
	}
	return &r
}

func errString(err error) *string {
	if err == nil {
		return nil
	}
	s := err.Error()
	return &s
}

// request is what the agent leaves in its inbox.
type request struct {
	Version     string `json:"version"`
	RequestedAt string `json:"requestedAt"`
}

func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

var errNoUpdate = errors.New("no update")

func wrap(what string, err error) error { return fmt.Errorf("%s: %w", what, err) }
