package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	startedAt = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	realELF   []byte
	elfOnce   sync.Once
)

// executable returns the bytes of a real ELF executable for this machine: the test binary itself.
func executable(t *testing.T) []byte {
	t.Helper()
	elfOnce.Do(func() {
		path, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		if realELF, err = os.ReadFile(path); err != nil {
			t.Fatal(err)
		}
	})
	return realELF
}

func digest(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

type env struct {
	paths  Paths
	now    time.Time
	server *httptest.Server
	body   []byte
	status int
	rel    *Release
	srcErr error
	mu     sync.Mutex
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{paths: Paths{Binary: filepath.Join(dir, "bin", "speedtest-agent"), StateDir: filepath.Join(dir, "state"), InboxDir: filepath.Join(dir, "inbox")},
		now: startedAt, status: http.StatusOK}
	for _, d := range []string{filepath.Dir(e.paths.Binary), e.paths.InboxDir, e.paths.StateDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(e.paths.Binary, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.status != http.StatusOK {
			w.WriteHeader(e.status)
			return
		}
		_, _ = w.Write(e.body)
	}))
	t.Cleanup(e.server.Close)
	e.advertise(executable(t), "2.0.0")
	return e
}

func (e *env) advertise(body []byte, version string) {
	e.body = body
	e.rel = &Release{Version: version, URL: e.server.URL + "/agent", SHA256: digest(body)}
}

func (e *env) applier() *Applier {
	return &Applier{Paths: e.paths, Version: "1.0.0", HTTP: e.server.Client(), Now: func() time.Time { return e.now },
		Source: func(context.Context) (*Release, error) { return e.rel, e.srcErr }}
}

func (e *env) agent(version string, restarted *int) *Agent {
	return &Agent{Paths: e.paths, Version: version, Now: func() time.Time { return e.now }, Restart: func() {
		if restarted != nil {
			*restarted++
		}
	}}
}

func (e *env) installed(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(e.paths.Binary)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) == "old-binary" {
		return "old"
	}
	if digest(b) == digest(executable(t)) {
		return "new"
	}
	return "other"
}

func (e *env) request(t *testing.T, version string) {
	t.Helper()
	if err := writeJSON(e.paths.request(), request{Version: version}, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSuccessfulUpdateIsConfirmedByAHeartbeat(t *testing.T) {
	e := newEnv(t)
	restarts := 0
	if !e.agent("1.0.0", &restarts).Offer(true, e.rel) || restarts != 1 {
		t.Fatal("an advertised newer release with auto-update on must request a restart")
	}
	if err := e.applier().Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	if e.installed(t) != "new" {
		t.Fatal("the binary was not replaced")
	}
	if prev, _ := os.ReadFile(e.paths.Previous()); string(prev) != "old-binary" {
		t.Fatal("the previous binary must be kept next to the new one")
	}
	if _, err := os.Stat(e.paths.request()); !os.IsNotExist(err) {
		t.Fatal("the request must be consumed")
	}
	if info, _ := os.Stat(e.paths.Binary); info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v", info.Mode())
	}
	state := loadState(e.paths)
	if state.Status != statePending || state.Version != "2.0.0" || state.Previous != "1.0.0" || !state.Deadline.Equal(startedAt.Add(ConfirmWindow)) {
		t.Fatalf("state = %+v", state)
	}

	// The restarted agent is the new version. Before it confirms nothing is final, and a restart inside the window keeps it.
	e.now = e.now.Add(30 * time.Second)
	if err := e.applier().Apply(t.Context()); err != nil || e.installed(t) != "new" {
		t.Fatalf("a restart inside the window must not roll back: %v", err)
	}
	newAgent := e.agent("2.0.0", nil)
	if newAgent.Expired() {
		t.Fatal("not expired inside the window")
	}
	newAgent.Confirm()
	if err := e.applier().Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.paths.Previous()); !os.IsNotExist(err) {
		t.Fatal("the previous binary is removed once the update is confirmed")
	}
	result := e.agent("2.0.0", nil).Result()
	if result == nil || result.Status != StatusSucceeded || result.Version != "2.0.0" || result.Error != nil || result.TS != "2026-11-03T12:00:30Z" {
		t.Fatalf("result = %+v", result)
	}
	if loadState(e.paths).Status != stateIdle {
		t.Fatal("state must return to idle")
	}
}

func TestTamperedDownloadIsRejectedAndNothingChanges(t *testing.T) {
	e := newEnv(t)
	tampered := append([]byte{}, executable(t)...)
	tampered[len(tampered)-1] ^= 0xff
	e.body = tampered // the advertised checksum is of the genuine binary
	e.request(t, "2.0.0")
	if err := e.applier().Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	if e.installed(t) != "old" {
		t.Fatal("a tampered binary must never replace the installed one")
	}
	result := loadResult(e.paths)
	if result == nil || result.Status != StatusFailed || result.Error == nil || !strings.Contains(*result.Error, "checksum mismatch") {
		t.Fatalf("result = %+v", result)
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(e.paths.Binary), ".speedtest-agent-new-*"))
	if len(leftovers) != 0 {
		t.Fatalf("staged files left behind: %v", leftovers)
	}
	if _, err := os.Stat(e.paths.Previous()); !os.IsNotExist(err) {
		t.Fatal("no previous binary is kept for an update that was not installed")
	}
}

func TestUnusableDownloadsAreRefused(t *testing.T) {
	notELF := []byte("#!/bin/sh\necho hi\n")
	for name, tc := range map[string]struct {
		prepare func(*env)
		want    string
	}{
		"not an executable": {func(e *env) { e.advertise(notELF, "2.0.0") }, "not a Linux executable"},
		"http url":          {func(e *env) { e.rel.URL = strings.Replace(e.rel.URL, "https://", "http://", 1) }, "https URL"},
		"url credentials":   {func(e *env) { e.rel.URL = strings.Replace(e.rel.URL, "https://", "https://user:pw@", 1) }, "https URL"},
		"server error":      {func(e *env) { e.status = http.StatusBadGateway }, "unexpected status 502"},
		"different version": {func(e *env) { e.rel.Version = "3.0.0" }, "not the requested"},
		"no release":        {func(e *env) { e.rel = nil }, "no longer advertises"},
		"bad checksum text": {func(e *env) { e.rel.SHA256 = "ABC" }, "sha256"},
		"already installed": {func(e *env) { e.rel.Version = "1.0.0" }, "not the requested"},
		"source failure":    {func(e *env) { e.srcErr = errors.New("center unreachable") }, "fetch release"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			tc.prepare(e)
			e.request(t, "2.0.0")
			if err := e.applier().Apply(t.Context()); err != nil {
				t.Fatal(err)
			}
			result := loadResult(e.paths)
			if e.installed(t) != "old" || result == nil || result.Status != StatusFailed || result.Error == nil || !strings.Contains(*result.Error, tc.want) {
				t.Fatalf("installed %s result %+v, want an error containing %q", e.installed(t), result, tc.want)
			}
		})
	}
}

func TestNoSourceConfiguredFails(t *testing.T) {
	e := newEnv(t)
	e.request(t, "2.0.0")
	a := e.applier()
	a.Source = nil
	if err := a.Apply(t.Context()); err != nil || loadResult(e.paths).Status != StatusFailed {
		t.Fatalf("apply = %v result %+v", err, loadResult(e.paths))
	}
}

func TestFailedFirstHeartbeatRestoresThePreviousBinary(t *testing.T) {
	e := newEnv(t)
	e.request(t, "2.0.0")
	if err := e.applier().Apply(t.Context()); err != nil || e.installed(t) != "new" {
		t.Fatalf("install: %v", err)
	}
	// The new version never confirms. Past the window, the next start restores the old binary.
	e.now = e.now.Add(ConfirmWindow + time.Second)
	if !e.agent("2.0.0", nil).Expired() {
		t.Fatal("the new version must report that it expired")
	}
	if err := e.applier().Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	if e.installed(t) != "old" {
		t.Fatal("the previous binary must be restored")
	}
	if _, err := os.Stat(e.paths.Previous()); !os.IsNotExist(err) {
		t.Fatal("the restored previous file is moved back, not copied")
	}
	result := loadResult(e.paths)
	if result.Status != StatusRolledBack || result.Version != "2.0.0" || result.Error == nil || !strings.Contains(*result.Error, "heartbeat") {
		t.Fatalf("result = %+v", result)
	}
	// The rolled back version is never offered again, even after the retry delay.
	e.now = e.now.Add(48 * time.Hour)
	if e.agent("1.0.0", nil).Offer(true, e.rel) {
		t.Fatal("a version that was rolled back must not be requested again")
	}
	if loadState(e.paths).Status != stateIdle {
		t.Fatal("state returns to idle after a rollback")
	}
}

func TestRollbackReportsAMissingPreviousBinary(t *testing.T) {
	e := newEnv(t)
	e.request(t, "2.0.0")
	_ = e.applier().Apply(t.Context())
	_ = os.Remove(e.paths.Previous())
	e.now = e.now.Add(time.Hour)
	if err := e.applier().Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r := loadResult(e.paths); r.Status != StatusRolledBack || !strings.Contains(*r.Error, "could not be restored") {
		t.Fatalf("result = %+v", r)
	}
}

func TestOfferDecisions(t *testing.T) {
	e := newEnv(t)
	good := *e.rel
	for name, tc := range map[string]struct {
		auto bool
		rel  *Release
		want bool
	}{
		"auto-update off":   {false, &good, false},
		"no release":        {true, nil, false},
		"same version":      {true, &Release{Version: "1.0.0", URL: good.URL, SHA256: good.SHA256}, false},
		"invalid version":   {true, &Release{Version: "../x", URL: good.URL, SHA256: good.SHA256}, false},
		"invalid checksum":  {true, &Release{Version: "2.0.0", URL: good.URL, SHA256: "zz"}, false},
		"missing url":       {true, &Release{Version: "2.0.0", SHA256: good.SHA256}, false},
		"newer and enabled": {true, &good, true},
	} {
		restarts := 0
		got := e.agent("1.0.0", &restarts).Offer(tc.auto, tc.rel)
		_ = os.Remove(e.paths.request())
		if got != tc.want || (restarts == 1) != tc.want {
			t.Errorf("%s: offered %v (restarts %d), want %v", name, got, restarts, tc.want)
		}
	}
}

func TestOfferHoldsBackWhileBusyOrRecentlyFailed(t *testing.T) {
	e := newEnv(t)
	a := e.agent("1.0.0", nil)
	if !a.Offer(true, e.rel) || a.Offer(true, e.rel) {
		t.Fatal("a queued request must not be queued twice")
	}
	_ = os.Remove(e.paths.request())

	if err := writeJSON(e.paths.state(), State{Status: statePending, Version: "2.0.0", Deadline: startedAt.Add(time.Minute)}, 0o644); err != nil {
		t.Fatal(err)
	}
	if a.Offer(true, e.rel) {
		t.Fatal("no new request while one update waits for confirmation")
	}
	_ = os.Remove(e.paths.state())

	failure := "download failed"
	if err := writeJSON(e.paths.result(), Result{Version: "2.0.0", Status: StatusFailed, TS: fmtTime(startedAt.Add(-time.Hour)), Error: &failure}, 0o644); err != nil {
		t.Fatal(err)
	}
	if a.Offer(true, e.rel) {
		t.Fatal("a recently failed version is not retried straight away")
	}
	e.now = e.now.Add(RetryAfter)
	if !a.Offer(true, e.rel) {
		t.Fatal("a transient failure is retried after the delay")
	}
}

func TestOwnerCommandWorksWithoutAutoUpdate(t *testing.T) {
	e := newEnv(t)
	restarts := 0
	a := e.agent("1.0.0", &restarts)
	if err := a.Command("2.0.0", e.rel); err != nil || restarts != 1 {
		t.Fatalf("command: %v restarts %d", err, restarts)
	}
	_ = os.Remove(e.paths.request())
	if a.Command("9.9.9", e.rel) == nil {
		t.Fatal("a command for a version that is not advertised must fail")
	}
	if a.Command("2.0.0", nil) == nil {
		t.Fatal("a command without an advertised release must fail")
	}
	if e.agent("2.0.0", nil).Command("2.0.0", e.rel) == nil {
		t.Fatal("updating to the running version must fail")
	}
}

func TestInboxSymlinksAndGarbageAreIgnored(t *testing.T) {
	e := newEnv(t)
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte(`{"version":"2.0.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, e.paths.request()); err != nil {
		t.Fatal(err)
	}
	if err := e.applier().Apply(t.Context()); err != nil || e.installed(t) != "old" || loadResult(e.paths) != nil {
		t.Fatalf("a symlinked request must be ignored: %v", err)
	}
	_ = os.Remove(e.paths.request())
	if err := os.WriteFile(e.paths.request(), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.applier().Apply(t.Context()); err != nil || e.installed(t) != "old" {
		t.Fatalf("garbage request: %v", err)
	}
	if err := os.WriteFile(e.paths.request(), []byte(`{"version":"../../etc"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.applier().Apply(t.Context()); err != nil || loadResult(e.paths) != nil {
		t.Fatalf("invalid version request: %v", err)
	}
	// A confirmation symlink never confirms anything.
	if err := writeJSON(e.paths.state(), State{Status: statePending, Version: "2.0.0", Deadline: startedAt.Add(time.Minute)}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("2.0.0"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, e.paths.confirm()); err != nil {
		t.Fatal(err)
	}
	if e.agent("2.0.0", nil).Expired() {
		t.Fatal("inside the window")
	}
	e.now = e.now.Add(time.Hour)
	if !e.agent("2.0.0", nil).Expired() {
		t.Fatal("a symlinked confirmation must not count")
	}
}

func TestConfirmOnlyForThePendingVersion(t *testing.T) {
	e := newEnv(t)
	e.agent("1.0.0", nil).Confirm()
	if _, err := os.Stat(e.paths.confirm()); !os.IsNotExist(err) {
		t.Fatal("nothing is pending, so nothing is confirmed")
	}
	if err := writeJSON(e.paths.state(), State{Status: statePending, Version: "2.0.0", Deadline: startedAt.Add(time.Minute)}, 0o644); err != nil {
		t.Fatal(err)
	}
	e.agent("1.0.0", nil).Confirm()
	if _, err := os.Stat(e.paths.confirm()); !os.IsNotExist(err) {
		t.Fatal("only the version waiting for confirmation may confirm it")
	}
	e.agent("2.0.0", nil).Confirm()
	if data, _ := os.ReadFile(e.paths.confirm()); string(data) != "2.0.0" {
		t.Fatalf("confirmation = %q", data)
	}
	if e.agent("2.0.0", nil).Expired() {
		t.Fatal("a confirmed version never expires")
	}
}

func TestDownloadLimitsAndRoundTrip(t *testing.T) {
	e := newEnv(t)
	_ = e
	if p := DefaultPaths(); p.Binary == "" || p.StateDir == "" || p.InboxDir == "" || p.Previous() != p.Binary+".prev" {
		t.Fatalf("default paths = %+v", p)
	}
	t.Setenv("SPEEDTEST_AGENT_BINARY", "/x/agent")
	t.Setenv("SPEEDTEST_AGENT_UPDATE_DIR", "/x/state")
	t.Setenv("SPEEDTEST_AGENT_INBOX_DIR", "/x/inbox")
	if p := DefaultPaths(); p.Binary != "/x/agent" || p.StateDir != "/x/state" || p.InboxDir != "/x/inbox" {
		t.Fatalf("environment overrides = %+v", p)
	}
	if (&Applier{}).client().Timeout == 0 || (&Applier{}).logger() == nil || (&Agent{}).logger() == nil || (&Agent{}).now().IsZero() || (&Applier{}).now().IsZero() {
		t.Fatal("zero values must work")
	}
}

func TestDownloadFailureModes(t *testing.T) {
	t.Run("too large", func(t *testing.T) {
		e := newEnv(t)
		e.request(t, "2.0.0")
		a := e.applier()
		a.MaxBytes = 1024
		_ = a.Apply(t.Context())
		if r := loadResult(e.paths); r == nil || !strings.Contains(*r.Error, "larger than allowed") || e.installed(t) != "old" {
			t.Fatalf("result = %+v", r)
		}
	})
	t.Run("connection cut mid-download", func(t *testing.T) {
		e := newEnv(t)
		cut := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "100000")
			_, _ = w.Write([]byte("partial"))
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
		}))
		defer cut.Close()
		e.rel.URL = cut.URL + "/agent"
		e.request(t, "2.0.0")
		a := e.applier()
		a.HTTP = cut.Client()
		_ = a.Apply(t.Context())
		if r := loadResult(e.paths); r == nil || !strings.Contains(*r.Error, "interrupted") || e.installed(t) != "old" {
			t.Fatalf("result = %+v", r)
		}
	})
	t.Run("server unreachable never leaks the url", func(t *testing.T) {
		e := newEnv(t)
		e.rel.URL = "https://127.0.0.1:1/agent?token=secret"
		e.request(t, "2.0.0")
		_ = e.applier().Apply(t.Context())
		r := loadResult(e.paths)
		if r == nil || strings.Contains(*r.Error, "secret") || strings.Contains(*r.Error, "127.0.0.1") {
			t.Fatalf("result = %+v", r)
		}
	})
	t.Run("wrong architecture", func(t *testing.T) {
		e := newEnv(t)
		wrong := append([]byte{}, executable(t)...)
		wrong[18], wrong[19] = 0xf3, 0x00 // EM_RISCV
		e.advertise(wrong, "2.0.0")
		e.request(t, "2.0.0")
		_ = e.applier().Apply(t.Context())
		if r := loadResult(e.paths); r == nil || !strings.Contains(*r.Error, "not built for") || e.installed(t) != "old" {
			t.Fatalf("result = %+v", r)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		e := newEnv(t)
		e.request(t, "2.0.0")
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_ = e.applier().Apply(ctx)
		if r := loadResult(e.paths); r == nil || r.Status != StatusFailed || e.installed(t) != "old" {
			t.Fatalf("result = %+v", r)
		}
	})
	t.Run("binary directory not writable", func(t *testing.T) {
		e := newEnv(t)
		e.request(t, "2.0.0")
		dir := filepath.Dir(e.paths.Binary)
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		_ = e.applier().Apply(t.Context())
		if r := loadResult(e.paths); r == nil || !strings.Contains(*r.Error, "stage the download") || e.installed(t) != "old" {
			t.Fatalf("result = %+v", r)
		}
	})
	t.Run("previous cannot be kept", func(t *testing.T) {
		e := newEnv(t)
		e.request(t, "2.0.0")
		if err := os.Remove(e.paths.Binary); err != nil { // nothing to keep
			t.Fatal(err)
		}
		_ = e.applier().Apply(t.Context())
		if r := loadResult(e.paths); r == nil || !strings.Contains(*r.Error, "previous binary") {
			t.Fatalf("result = %+v", r)
		}
		leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(e.paths.Binary), ".speedtest-agent-new-*"))
		if len(leftovers) != 0 {
			t.Fatalf("staged files left behind: %v", leftovers)
		}
	})
}

func TestStateDirectoryProblemsAreReported(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	e := newEnv(t)
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	a := e.applier()
	a.Paths.StateDir = filepath.Join(blocked, "state")
	if err := a.Apply(t.Context()); err == nil {
		t.Fatal("an unusable state directory must be reported")
	}
	// Result and state cannot be saved into a read-only directory.
	e = newEnv(t)
	e.request(t, "2.0.0")
	if err := os.Chmod(e.paths.StateDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(e.paths.StateDir, 0o755) })
	if err := e.applier().Apply(t.Context()); err == nil {
		t.Fatal("a result that cannot be saved must be reported")
	}
	if e.installed(t) != "old" {
		t.Fatal("when the state cannot be saved the install is undone")
	}
}

func TestAgentCannotWriteItsInbox(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	e := newEnv(t)
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	a := e.agent("1.0.0", nil)
	a.Paths.InboxDir = filepath.Join(blocked, "inbox")
	if a.Offer(true, e.rel) {
		t.Fatal("no restart when the request cannot be written")
	}
	if err := writeJSON(a.Paths.state(), State{Status: statePending, Version: "1.0.0", Deadline: startedAt.Add(time.Hour)}, 0o644); err != nil {
		t.Fatal(err)
	}
	a.Confirm() // must not panic and must not claim success
	if err := os.Chmod(e.paths.InboxDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(e.paths.InboxDir, 0o755) })
	b := e.agent("1.0.0", nil)
	b.Confirm()
	if _, err := os.Stat(b.Paths.confirm()); !os.IsNotExist(err) {
		t.Fatal("confirmation must not exist when it cannot be written")
	}
}

func TestStateAndResultFilesAreValidated(t *testing.T) {
	e := newEnv(t)
	for _, content := range []string{"not json", `{"status":"weird"}`, `{"status":"pending_confirm","version":"2.0.0"`} {
		if err := os.WriteFile(e.paths.state(), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if loadState(e.paths).Status != stateIdle {
			t.Fatalf("%q must read as idle", content)
		}
	}
	if err := os.WriteFile(e.paths.result(), []byte(`{"status":"succeeded"}`), 0o644); err != nil || loadResult(e.paths) != nil {
		t.Fatal("a result without a version is ignored")
	}
	if _, err := readSmall(e.paths.StateDir); err == nil {
		t.Fatal("a directory is not a small file")
	}
	if errString(nil) != nil || *errString(errors.New("x")) != "x" {
		t.Fatal("errString")
	}
	if err := writeAtomic(filepath.Join(e.paths.StateDir, "no", "such", "x"), nil, 0o644); err == nil {
		t.Fatal("writeAtomic into a missing directory must fail")
	}
	if err := writeJSON(filepath.Join(e.paths.StateDir, "x"), make(chan int), 0o644); err == nil {
		t.Fatal("unencodable values must fail")
	}
}
