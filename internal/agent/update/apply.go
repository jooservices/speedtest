package update

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// defaultMaxBinary bounds a download so a hostile server cannot fill the disk.
const defaultMaxBinary = 200 << 20

// Applier is the privileged side, run before each start of the service.
type Applier struct {
	Paths Paths
	// Version is the version of the binary running this step, used to skip a request for the version already installed.
	Version string
	// Source fetches the release the center advertises; nothing the agent wrote is used for the download.
	Source func(context.Context) (*Release, error)
	// HTTP downloads the binary. It defaults to a client with a two minute timeout.
	HTTP   *http.Client
	Logger *slog.Logger
	Now    func() time.Time
	// MaxBytes bounds a download; zero means 200 MiB.
	MaxBytes int64
}

func (a *Applier) now() time.Time {
	if a.Now != nil {
		return a.Now().UTC()
	}
	return time.Now().UTC()
}

func (a *Applier) logger() *slog.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return slog.Default()
}

func (a *Applier) client() *http.Client {
	if a.HTTP != nil {
		return a.HTTP
	}
	return &http.Client{Timeout: 2 * time.Minute}
}

// Apply settles an update in progress and installs a requested one. It never returns an error for an update
// that merely failed: that is recorded in the result and the old binary keeps running.
func (a *Applier) Apply(ctx context.Context) error {
	if err := os.MkdirAll(a.Paths.StateDir, 0o755); err != nil {
		return wrap("prepare update state", err)
	}
	state := loadState(a.Paths)
	if state.Status == statePending {
		if err := a.settle(&state); err != nil {
			return err
		}
		if state.Status == statePending {
			return nil
		}
	}
	data, err := readSmall(a.Paths.request())
	if err != nil {
		return nil
	}
	defer func() { _ = os.Remove(a.Paths.request()) }()
	var req request
	if err := json.Unmarshal(data, &req); err != nil || !versionPattern.MatchString(req.Version) {
		return nil
	}
	if err := a.install(ctx, req.Version, &state); err != nil {
		a.logger().Warn("agent update failed; keeping the installed version", "version", req.Version, "error", err.Error())
		return a.record(Result{Version: req.Version, Status: StatusFailed, TS: fmtTime(a.now()), Error: errString(err)})
	}
	return nil
}

// settle confirms an update whose new version reported a heartbeat, or restores the previous binary when the
// confirmation window has passed.
func (a *Applier) settle(state *State) error {
	if data, err := readSmall(a.Paths.confirm()); err == nil && strings.TrimSpace(string(data)) == state.Version {
		_ = os.Remove(a.Paths.Previous())
		_ = os.Remove(a.Paths.confirm())
		state.Status, state.Previous, state.Deadline = stateIdle, "", time.Time{}
		if err := writeJSON(a.Paths.state(), state, 0o644); err != nil {
			return wrap("save update state", err)
		}
		return a.record(Result{Version: state.Version, Status: StatusSucceeded, TS: fmtTime(a.now())})
	}
	if a.now().Before(state.Deadline) {
		return nil
	}
	version := state.Version
	cause := fmt.Errorf("version %s did not complete a heartbeat within %s", version, ConfirmWindow)
	if err := os.Rename(a.Paths.Previous(), a.Paths.Binary); err != nil {
		cause = fmt.Errorf("%w; the previous binary could not be restored: %v", cause, err)
	}
	state.Failed = append(state.Failed, version)
	state.Status, state.Version, state.Previous, state.Deadline = stateIdle, "", "", time.Time{}
	if err := writeJSON(a.Paths.state(), state, 0o644); err != nil {
		return wrap("save update state", err)
	}
	_ = os.Remove(a.Paths.confirm())
	a.logger().Error("agent update rolled back", "version", version, "error", cause.Error())
	return a.record(Result{Version: version, Status: StatusRolledBack, TS: fmtTime(a.now()), Error: errString(cause)})
}

func (a *Applier) record(r Result) error {
	if err := writeJSON(a.Paths.result(), r, 0o644); err != nil {
		return wrap("save update result", err)
	}
	return nil
}

// install downloads, verifies and swaps in the requested version.
func (a *Applier) install(ctx context.Context, version string, state *State) error {
	if a.Source == nil {
		return errors.New("no release source configured")
	}
	rel, err := a.Source(ctx)
	if err != nil {
		return wrap("fetch release", err)
	}
	if rel == nil {
		return errors.New("the center no longer advertises a release")
	}
	if err := rel.Validate(); err != nil {
		return err
	}
	switch {
	case rel.Version != version:
		return fmt.Errorf("the advertised release is %s, not the requested %s", rel.Version, version)
	case rel.Version == a.Version:
		return errors.New("that version is already installed")
	case state.hasFailed(rel.Version):
		return errors.New("that version already failed and is not retried")
	}
	staged, err := a.download(ctx, *rel)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(staged) }()
	if err := checkExecutable(staged); err != nil {
		return err
	}
	previous := a.Paths.Previous()
	_ = os.Remove(previous)
	if err := os.Link(a.Paths.Binary, previous); err != nil {
		return wrap("keep the previous binary", err)
	}
	if err := os.Rename(staged, a.Paths.Binary); err != nil {
		_ = os.Remove(previous)
		return wrap("install the new binary", err)
	}
	state.Status, state.Version, state.Previous = statePending, rel.Version, a.Version
	state.Deadline = a.now().Add(ConfirmWindow)
	if err := writeJSON(a.Paths.state(), state, 0o644); err != nil {
		_ = os.Rename(previous, a.Paths.Binary)
		return wrap("save update state", err)
	}
	_ = os.Remove(a.Paths.confirm())
	a.logger().Info("agent update installed; waiting for the first heartbeat", "version", rel.Version, "deadline", fmtTime(state.Deadline))
	return nil
}

// download fetches the release into the binary's own directory (so the final rename is atomic) and verifies
// its SHA-256 before returning the file name. A mismatch deletes the file.
func (a *Applier) download(ctx context.Context, rel Release) (string, error) {
	u, err := url.Parse(rel.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return "", errors.New("the release URL must be an https URL without credentials")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := a.client().Do(req)
	if err != nil {
		return "", wrap("download", errors.New("request failed"))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download: unexpected status %d", resp.StatusCode)
	}
	tmp, err := os.CreateTemp(filepath.Dir(a.Paths.Binary), ".speedtest-agent-new-*")
	if err != nil {
		return "", wrap("stage the download", err)
	}
	name := tmp.Name()
	fail := func(err error) (string, error) {
		_ = tmp.Close()
		_ = os.Remove(name)
		return "", err
	}
	limit := a.MaxBytes
	if limit <= 0 {
		limit = defaultMaxBinary
	}
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, sum), io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return fail(wrap("download", errors.New("interrupted")))
	}
	if n > limit {
		return fail(errors.New("the download is larger than allowed"))
	}
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum.Sum(nil))), []byte(rel.SHA256)) != 1 {
		return fail(errors.New("checksum mismatch; the download was discarded"))
	}
	if err := tmp.Chmod(0o755); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// checkExecutable makes sure the file is an ELF executable for this machine, so a wrong-architecture
// or non-binary download is refused before it replaces a working agent.
func checkExecutable(path string) error {
	f, err := elf.Open(path)
	if err != nil {
		return errors.New("the download is not a Linux executable")
	}
	defer func() { _ = f.Close() }()
	want := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[runtime.GOARCH]
	if want == 0 || f.Machine != want || (f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN) {
		return fmt.Errorf("the download is not built for %s", runtime.GOARCH)
	}
	return nil
}
