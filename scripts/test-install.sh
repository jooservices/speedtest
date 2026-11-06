#!/bin/sh
# Exercises internal/center/install/install.sh against a fake download server in a
# temporary root: success, tampered download, unsupported architecture, bad input.
set -eu

here="$(cd "$(dirname "$0")/.." && pwd)"
script="$here/internal/center/install/install.sh"
work="$(mktemp -d)"
server_pid=""
cleanup() {
	if [ -n "$server_pid" ]; then kill "$server_pid" 2>/dev/null || true; fi
	rm -rf "$work"
}
trap cleanup EXIT

fail() {
	printf 'FAIL: %s\n' "$*" >&2
	exit 1
}

# Fake release directory: a "binary" that records how it was called.
rel="$work/release"
mkdir -p "$rel"
cat >"$rel/speedtest-agent-linux-amd64" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >"$(dirname "$SPEEDTEST_AGENT_CREDENTIAL")/enroll-args"
printf 'fake-credential' >"$SPEEDTEST_AGENT_CREDENTIAL"
EOF
cp "$rel/speedtest-agent-linux-amd64" "$rel/speedtest-agent-linux-arm64"
printf '[Unit]\nDescription=fake\n' >"$rel/speedtest-agent.service"
(cd "$rel" && sha256sum speedtest-agent-linux-amd64 speedtest-agent-linux-arm64 speedtest-agent.service >SHA256SUMS)

# Serve it on a free loopback port.
port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"
mkdir -p "$work/www"
ln -s "$rel" "$work/www/download"
(cd "$work/www" && exec python3 -m http.server "$port" --bind 127.0.0.1 >"$work/server.log" 2>&1) &
server_pid=$!
i=0
until python3 -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:$port/download/SHA256SUMS')" 2>/dev/null; do
	i=$((i + 1))
	[ "$i" -lt 50 ] || fail "fake download server did not start"
	sleep 0.1
done
url="http://127.0.0.1:$port"
sed "s|__CENTER_URL__|$url|" "$script" >"$work/install.sh"

run_install() { # extra env first via env(1); args follow
	env SPEEDTEST_INSTALL_ROOT="$work/root" "$@"
}

# 1. Success on amd64.
rm -rf "$work/root"
run_install SPEEDTEST_INSTALL_UNAME_M=x86_64 sh "$work/install.sh" --token tok123 --name "vm 1" >"$work/out1.log" 2>&1 || {
	cat "$work/out1.log" >&2
	fail "install failed"
}
[ -x "$work/root/usr/local/bin/speedtest-agent" ] || fail "binary not installed or not executable"
[ -f "$work/root/etc/systemd/system/speedtest-agent.service" ] || fail "service unit not installed"
args="$(cat "$work/root/etc/speedtest-agent/enroll-args")"
[ "$args" = "enroll --center $url --token tok123 --name vm 1" ] || fail "unexpected enrol arguments: $args"
[ -f "$work/root/etc/speedtest-agent/credential" ] || fail "credential not written"
grep -q "^SPEEDTEST_AGENT_CENTER=" "$work/root/etc/speedtest-agent/.env" || fail "center address not written for the service"
grep -q "^name: " "$work/root/etc/speedtest-agent/config.yaml" || fail "node name not written"
grep -q "Checksum verified" "$work/out1.log" || fail "no checksum confirmation printed"
printf 'ok: install on amd64\n'

# 2. arm64 picks the arm64 binary.
rm -rf "$work/root"
run_install SPEEDTEST_INSTALL_UNAME_M=aarch64 sh "$work/install.sh" --token t --name n >/dev/null 2>&1 || fail "arm64 install failed"
printf 'ok: install on arm64\n'

# 3. A tampered download is refused and nothing is installed.
rm -rf "$work/root"
printf 'tampered' >>"$rel/speedtest-agent-linux-amd64"
if run_install SPEEDTEST_INSTALL_UNAME_M=x86_64 sh "$work/install.sh" --token t --name n >"$work/out3.log" 2>&1; then
	fail "tampered binary was installed"
fi
grep -q "checksum mismatch" "$work/out3.log" || fail "no checksum mismatch message"
[ ! -e "$work/root/usr/local/bin/speedtest-agent" ] || fail "binary left behind after a checksum mismatch"
printf 'ok: tampered download refused\n'

# 4. Unsupported architecture.
if run_install SPEEDTEST_INSTALL_UNAME_M=riscv64 sh "$work/install.sh" --token t --name n >"$work/out4.log" 2>&1; then
	fail "unsupported architecture accepted"
fi
grep -q "unsupported architecture" "$work/out4.log" || fail "no unsupported architecture message"
printf 'ok: unsupported architecture refused\n'

# 5. Missing token and invalid name.
run_install sh "$work/install.sh" --name n >/dev/null 2>&1 && fail "missing token accepted"
run_install sh "$work/install.sh" --token t --name 'bad;name' >/dev/null 2>&1 && fail "unsafe node name accepted"
printf 'ok: input validation\n'

# 6. Not root and no test root: refuses.
if [ "$(id -u)" -ne 0 ]; then
	env -u SPEEDTEST_INSTALL_ROOT sh "$work/install.sh" --token t --name n >"$work/out6.log" 2>&1 && fail "non-root install accepted"
	grep -q "run as root" "$work/out6.log" || fail "no root hint"
	printf 'ok: refuses to run without root\n'
fi
printf 'PASS: install script\n'
