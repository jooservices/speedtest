package installer

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSudoCommandKeepsPayloadSeparate(t *testing.T) {
	for _, mode := range []string{"password", "cached", "nopasswd", "denied"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			sudo := `#!/bin/sh
if [ "$1" = "-S" ]; then
 if [ "$TEST_SUDO_MODE" = denied ]; then exit 1; fi
 if [ "$TEST_SUDO_MODE" = password ]; then
  IFS= read -r supplied
  [ "$supplied" = "test-password" ] || exit 1
 fi
 exit 0
fi
[ "$1" = "-n" ] || exit 2
shift
exec "$@"
`
			if err := os.WriteFile(filepath.Join(root, "sudo"), []byte(sudo), 0700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("/bin/sh", "-c", sudoCommand("sudo -S -p '' /bin/sh -s"))
			cmd.Env = append(os.Environ(), "PATH="+root+":"+os.Getenv("PATH"), "TEST_SUDO_MODE="+mode)
			cmd.Stdin = strings.NewReader("test-password\nprintf 'payload executed'\n")
			output, err := cmd.CombinedOutput()
			if mode == "denied" {
				if err == nil || len(output) != 0 {
					t.Fatalf("denied sudo executed payload: %s, %v", output, err)
				}
			} else if err != nil || string(output) != "payload executed" {
				t.Fatalf("payload corrupted: %s, %v", output, err)
			}
		})
	}
}

// TestRemoteScriptLifecycle executes the shipped shell scripts against a
// disposable filesystem and fake Linux tools, including checksum failures and
// restoration of a running installation.
func TestRemoteScriptLifecycle(t *testing.T) {
	for _, arch := range []string{"x86_64", "aarch64", "upgrade", "commit", "unsupported", "bad-checksum"} {
		t.Run(arch, func(t *testing.T) {
			runRemoteScriptLifecycleCase(t, arch)
		})
	}
}

type remoteScriptFixture struct {
	arch, root, agent, config, work, id string
	env                                 []string
}

func runRemoteScriptLifecycleCase(t *testing.T, arch string) {
	t.Helper()
	fixture := prepareRemoteScriptFixture(t, arch)
	cmd, output := startRemoteInstaller(t, fixture)
	if arch == "unsupported" || arch == "bad-checksum" {
		assertRemoteInstallRejected(t, fixture, cmd)
		return
	}

	completeRemoteInstall(t, fixture, cmd, output)
	if arch == "commit" {
		assertRemoteCommit(t, fixture)
		return
	}

	runRemoteRollback(t, fixture)
	if arch == "upgrade" {
		assertRemoteUpgradeRestored(t, fixture)
		return
	}
	assertRemoteRollbackClean(t, fixture)
}

func prepareRemoteScriptFixture(t *testing.T, arch string) remoteScriptFixture {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "tools")
	const id = "00000000-0000-4000-8000-000000000043"
	fixture := remoteScriptFixture{
		arch: arch, root: root,
		agent: root + "/usr/local/bin/speedtest-agent", config: root + "/etc/speedtest-agent",
		work: root + "/var/tmp/speedtest-installer-" + id, id: id,
	}
	installRemoteFixtureTools(t, bin)
	writeRemoteFixtureRelease(t, fixture)
	prepareRemoteFixtureFiles(t, fixture)
	if arch == "upgrade" {
		prepareRemoteUpgradeState(t, fixture)
	}
	fixture.env = remoteFixtureEnvironment(arch, root, bin)
	return fixture
}

func installRemoteFixtureTools(t *testing.T, bin string) {
	t.Helper()
	tool := func(name, body string) {
		writeInstallerFixtureFile(t, filepath.Join(bin, name), "#!/bin/sh\nset -eu\n"+body, 0700)
	}
	tool("uname", `if [ "$1" = -s ]; then echo Linux; else echo "$TEST_ARCH"; fi`)
	tool("id", `test -f "$TEST_ROOT/user"`)
	tool("useradd", `touch "$TEST_ROOT/user"`)
	tool("userdel", `rm -f "$TEST_ROOT/user"`)
	tool("chown", "exit 0")
	tool("install", `set -- "$@"
args=
# The scripts use fixed, whitespace-free fixture paths.
while [ "$#" -gt 0 ]; do
 case "$1" in -o|-g) shift; shift;; *) args="$args '$1'"; shift;; esac
done
eval "exec /usr/bin/install $args"`)
	tool("systemctl", `case "$1" in
is-active) test -f "$TEST_ROOT/active";;
is-enabled) test -f "$TEST_ROOT/enabled";;
stop) rm -f "$TEST_ROOT/active";;
disable) rm -f "$TEST_ROOT/active" "$TEST_ROOT/enabled";;
enable) touch "$TEST_ROOT/enabled";;
restart|start) touch "$TEST_ROOT/active";;
daemon-reload) :;;
*) exit 1;;
esac`)
	tool("curl", `while [ "$1" != "-o" ]; do shift; done
out=$2; url=$3
case "$url" in
*/SHA256SUMS) cp "$TEST_ROOT/release/SHA256SUMS" "$out";;
*/speedtest-agent.service) cp "$TEST_ROOT/release/speedtest-agent.service" "$out";;
*) cp "$TEST_ROOT/release/agent" "$out";;
esac`)
	tool("runuser", `shift; shift; shift; exec "$@"`)
}

func writeRemoteFixtureRelease(t *testing.T, fixture remoteScriptFixture) {
	t.Helper()
	writeInstallerFixtureFile(t, fixture.root+"/release/agent", "#!/bin/sh\nread -r token\nprintf '%s' test-credential > "+fixture.config+"/credential\n", 0700)
	writeInstallerFixtureFile(t, fixture.root+"/release/speedtest-agent.service", "new-unit", 0600)
	sum := exec.Command("sha256sum", fixture.root+"/release/agent", fixture.root+"/release/speedtest-agent.service")
	output, err := sum.Output()
	if err != nil {
		t.Fatal(err)
	}
	sums := strings.ReplaceAll(string(output), fixture.root+"/release/agent", "speedtest-agent-linux-amd64")
	sums = strings.ReplaceAll(sums, fixture.root+"/release/speedtest-agent.service", "speedtest-agent.service")
	sums = remoteFixtureChecksums(fixture.arch, sums)
	writeInstallerFixtureFile(t, fixture.root+"/release/SHA256SUMS", sums, 0600)
}

func remoteFixtureChecksums(arch, sums string) string {
	if arch == "aarch64" {
		sums = strings.ReplaceAll(sums, "amd64", "arm64")
	}
	if arch == "bad-checksum" {
		return strings.Repeat("0", 64) + "  speedtest-agent-linux-amd64\n"
	}
	return sums
}

func prepareRemoteFixtureFiles(t *testing.T, fixture remoteScriptFixture) {
	t.Helper()
	for _, dir := range []string{fixture.root + "/usr/local/bin", fixture.root + "/etc/systemd/system"} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeInstallerFixtureFile(t, fixture.work+"/config.env", "INSTALL_ID="+fixture.id+"\nCENTER_URL_B64="+encode("https://center.test")+"\nNODE_NAME_B64="+encode("fixture")+"\n", 0600)
}

func prepareRemoteUpgradeState(t *testing.T, fixture remoteScriptFixture) {
	t.Helper()
	writeInstallerFixtureFile(t, fixture.agent, "old-binary", 0755)
	writeInstallerFixtureFile(t, fixture.config+"/credential", "old-credential", 0600)
	writeInstallerFixtureFile(t, fixture.root+"/etc/systemd/system/speedtest-agent.service", "old-unit", 0600)
	for _, name := range []string{"user", "active", "enabled"} {
		writeInstallerFixtureFile(t, fixture.root+"/"+name, "", 0600)
	}
}

func remoteFixtureEnvironment(arch, root, bin string) []string {
	env := append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "TEST_ROOT="+root, "TEST_ARCH="+arch)
	if arch == "bad-checksum" || arch == "upgrade" || arch == "commit" {
		env = append(env, "TEST_ARCH=x86_64")
	}
	return env
}

func writeInstallerFixtureFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func mapInstallerScript(script, root string) string {
	script = strings.ReplaceAll(script, "$BACKUP/usr", "$BACKUP/backup-usr")
	script = strings.ReplaceAll(script, "$BACKUP/etc", "$BACKUP/backup-etc")
	for _, path := range []string{"/var/tmp", "/usr/local/bin", "/etc/systemd", "/etc/speedtest-agent", "/var/lib/speedtest-agent"} {
		script = strings.ReplaceAll(script, path, root+path)
	}
	script = strings.ReplaceAll(script, "$BACKUP/backup-usr", "$BACKUP/usr")
	script = strings.ReplaceAll(script, "$BACKUP/backup-etc", "$BACKUP/etc")
	return script
}

func startRemoteInstaller(t *testing.T, fixture remoteScriptFixture) (*exec.Cmd, *strings.Builder) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-s", "--", fixture.work+"/config.env")
	cmd.Env = fixture.env
	cmd.Stdin = strings.NewReader(mapInstallerScript(installRemoteScript, fixture.root))
	var output strings.Builder
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd, &output
}

func completeRemoteInstall(t *testing.T, fixture remoteScriptFixture, cmd *exec.Cmd, output *strings.Builder) {
	t.Helper()
	// The real FIFO is fed by a separate process, just as the SSH side channel.
	writer := exec.Command("/bin/sh", "-c", "while [ ! -p \"$1/token.pipe\" ]; do sleep 0.01; done; printf '%s\\n' test-token > \"$1/token.pipe\"", "writer", fixture.work)
	if err := writer.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		_ = writer.Process.Kill()
		t.Fatalf("install: %v: %s", err, output.String())
	}
	if err := writer.Wait(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(fixture.agent); err != nil || !strings.Contains(string(data), "test-credential") {
		t.Fatalf("binary missing: %v", err)
	}
	if stat, err := os.Stat(fixture.config + "/credential"); err != nil || stat.Mode().Perm() != 0600 {
		t.Fatalf("credential mode: %v", err)
	}
}

func runRemoteScript(t *testing.T, fixture remoteScriptFixture, script, arg string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-s", "--", arg)
	cmd.Env = fixture.env
	cmd.Stdin = strings.NewReader(mapInstallerScript(script, fixture.root))
	return cmd.CombinedOutput()
}

func assertRemoteCommit(t *testing.T, fixture remoteScriptFixture) {
	t.Helper()
	if output, err := runRemoteScript(t, fixture, commitRemoteScript, fixture.id); err != nil {
		t.Fatalf("commit: %v %s", err, output)
	}
	if _, err := os.Stat(fixture.work); !os.IsNotExist(err) {
		t.Fatal("commit left temporary state")
	}
	if _, err := os.Stat(fixture.agent); err != nil {
		t.Fatal("commit removed installed binary")
	}
}

func runRemoteRollback(t *testing.T, fixture remoteScriptFixture) {
	t.Helper()
	if output, err := runRemoteScript(t, fixture, rollbackRemoteScript, fixture.id); err != nil {
		t.Fatalf("rollback: %v: %s", err, output)
	}
}

func assertRemoteUpgradeRestored(t *testing.T, fixture remoteScriptFixture) {
	t.Helper()
	for path, want := range map[string]string{
		fixture.agent: "old-binary", fixture.config + "/credential": "old-credential",
		fixture.root + "/etc/systemd/system/speedtest-agent.service": "old-unit",
	} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != want {
			t.Fatalf("upgrade rollback did not restore %s: %v", path, err)
		}
	}
	for _, name := range []string{"user", "active", "enabled"} {
		if _, err := os.Stat(fixture.root + "/" + name); err != nil {
			t.Fatalf("upgrade rollback removed prior %s", name)
		}
	}
	if _, err := os.Stat(fixture.work); !os.IsNotExist(err) {
		t.Fatal("upgrade rollback retained temporary state")
	}
}

func assertRemoteRollbackClean(t *testing.T, fixture remoteScriptFixture) {
	t.Helper()
	for _, path := range []string{
		fixture.agent, fixture.config, fixture.work, fixture.root + "/user", fixture.root + "/active",
		fixture.root + "/enabled", fixture.root + "/etc/systemd/system/speedtest-agent.service",
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("rollback left %s: %v", path, err)
		}
	}
}

func assertRemoteInstallRejected(t *testing.T, fixture remoteScriptFixture, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Wait(); err == nil {
		t.Fatal("invalid platform/checksum accepted")
	}
	if _, err := os.Stat(fixture.agent); !os.IsNotExist(err) {
		t.Fatal("failure installed a binary")
	}
	if _, err := os.Stat(fixture.work); !os.IsNotExist(err) {
		t.Fatal("failure left temporary files")
	}
}
