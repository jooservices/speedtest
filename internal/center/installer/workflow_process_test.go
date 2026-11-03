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
			root := t.TempDir()
			write := func(path, body string, mode os.FileMode) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), mode); err != nil {
					t.Fatal(err)
				}
			}
			bin := filepath.Join(root, "tools")
			tool := func(name, body string) { write(filepath.Join(bin, name), "#!/bin/sh\nset -eu\n"+body, 0700) }
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
			agent := root + "/usr/local/bin/speedtest-agent"
			config := root + "/etc/speedtest-agent"
			write(root+"/release/agent", "#!/bin/sh\nread -r token\nprintf '%s' test-credential > "+config+"/credential\n", 0700)
			write(root+"/release/speedtest-agent.service", "new-unit", 0600)
			sum := exec.Command("sha256sum", root+"/release/agent", root+"/release/speedtest-agent.service")
			output, err := sum.Output()
			if err != nil {
				t.Fatal(err)
			}
			sums := strings.ReplaceAll(string(output), root+"/release/agent", "speedtest-agent-linux-amd64")
			sums = strings.ReplaceAll(sums, root+"/release/speedtest-agent.service", "speedtest-agent.service")
			if arch == "aarch64" {
				sums = strings.ReplaceAll(sums, "amd64", "arm64")
			}
			if arch == "bad-checksum" {
				sums = strings.Repeat("0", 64) + "  speedtest-agent-linux-amd64\n"
			}
			write(root+"/release/SHA256SUMS", sums, 0600)
			for _, dir := range []string{root + "/usr/local/bin", root + "/etc/systemd/system"} {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			const id = "00000000-0000-4000-8000-000000000043"
			work := root + "/var/tmp/speedtest-installer-" + id
			write(work+"/config.env", "INSTALL_ID="+id+"\nCENTER_URL_B64="+encode("https://center.test")+"\nNODE_NAME_B64="+encode("fixture")+"\n", 0600)
			if arch == "upgrade" {
				write(agent, "old-binary", 0755)
				write(config+"/credential", "old-credential", 0600)
				write(root+"/etc/systemd/system/speedtest-agent.service", "old-unit", 0600)
				for _, name := range []string{"user", "active", "enabled"} {
					write(root+"/"+name, "", 0600)
				}
			}
			mapped := func(script string) string {
				script = strings.ReplaceAll(script, "$BACKUP/usr", "$BACKUP/backup-usr")
				script = strings.ReplaceAll(script, "$BACKUP/etc", "$BACKUP/backup-etc")
				for _, path := range []string{"/var/tmp", "/usr/local/bin", "/etc/systemd", "/etc/speedtest-agent", "/var/lib/speedtest-agent"} {
					script = strings.ReplaceAll(script, path, root+path)
				}
				script = strings.ReplaceAll(script, "$BACKUP/backup-usr", "$BACKUP/usr")
				script = strings.ReplaceAll(script, "$BACKUP/backup-etc", "$BACKUP/etc")
				return script
			}
			env := append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "TEST_ROOT="+root, "TEST_ARCH="+arch)
			if arch == "bad-checksum" || arch == "upgrade" || arch == "commit" {
				env = append(env, "TEST_ARCH=x86_64")
			}
			// The real FIFO is fed by a separate process, just as the SSH side channel.
			cmd := exec.Command("/bin/sh", "-s", "--", work+"/config.env")
			cmd.Env = env
			cmd.Stdin = strings.NewReader(mapped(installRemoteScript))
			var out strings.Builder
			cmd.Stdout = &out
			cmd.Stderr = &out
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			if arch != "unsupported" && arch != "bad-checksum" {
				// A shell writer blocks until the installer reads the FIFO.
				writer := exec.Command("/bin/sh", "-c", "while [ ! -p \"$1/token.pipe\" ]; do sleep 0.01; done; printf '%s\\n' test-token > \"$1/token.pipe\"", "writer", work)
				if err := writer.Start(); err != nil {
					t.Fatal(err)
				}
				if err := cmd.Wait(); err != nil {
					_ = writer.Process.Kill()
					t.Fatalf("install: %v: %s", err, out.String())
				}
				if err := writer.Wait(); err != nil {
					t.Fatal(err)
				}
				if data, err := os.ReadFile(agent); err != nil || !strings.Contains(string(data), "test-credential") {
					t.Fatalf("binary missing: %v", err)
				}
				if stat, err := os.Stat(config + "/credential"); err != nil || stat.Mode().Perm() != 0600 {
					t.Fatalf("credential mode: %v", err)
				}

				if arch == "commit" {
					commit := exec.Command("/bin/sh", "-s", "--", id)
					commit.Env = env
					commit.Stdin = strings.NewReader(mapped(commitRemoteScript))
					if data, err := commit.CombinedOutput(); err != nil {
						t.Fatalf("commit: %v %s", err, data)
					}
					if _, err := os.Stat(work); !os.IsNotExist(err) {
						t.Fatal("commit left temporary state")
					}
					if _, err := os.Stat(agent); err != nil {
						t.Fatal("commit removed installed binary")
					}
					return
				}
				// Roll back and verify every newly created resource is removed.
				rollback := exec.Command("/bin/sh", "-s", "--", id)
				rollback.Env = env
				rollback.Stdin = strings.NewReader(mapped(rollbackRemoteScript))
				if data, err := rollback.CombinedOutput(); err != nil {
					t.Fatalf("rollback: %v: %s", err, data)
				}

				if arch == "upgrade" {
					for path, want := range map[string]string{agent: "old-binary", config + "/credential": "old-credential", root + "/etc/systemd/system/speedtest-agent.service": "old-unit"} {
						data, err := os.ReadFile(path)
						if err != nil || string(data) != want {
							t.Fatalf("upgrade rollback did not restore %s: %v", path, err)
						}
					}
					for _, name := range []string{"user", "active", "enabled"} {
						if _, err := os.Stat(root + "/" + name); err != nil {
							t.Fatalf("upgrade rollback removed prior %s", name)
						}
					}
					if _, err := os.Stat(work); !os.IsNotExist(err) {
						t.Fatal("upgrade rollback retained temporary state")
					}
					return
				}
				for _, path := range []string{agent, config, work, root + "/user", root + "/active", root + "/enabled", root + "/etc/systemd/system/speedtest-agent.service"} {
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Errorf("rollback left %s: %v", path, err)
					}
				}
			} else {
				if err := cmd.Wait(); err == nil {
					t.Fatal("invalid platform/checksum accepted")
				}
				if _, err := os.Stat(agent); !os.IsNotExist(err) {
					t.Fatal("failure installed a binary")
				}
				if _, err := os.Stat(work); !os.IsNotExist(err) {
					t.Fatal("failure left temporary files")
				}
			}
		})
	}
}
