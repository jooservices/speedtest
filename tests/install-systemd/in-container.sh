#!/bin/sh
set -eu

fail() {
	printf 'FAIL: %s\n' "$*" >&2
	exit 1
}

python3 -m http.server 18080 --bind 127.0.0.1 --directory /test/www >/tmp/install-systemd-http.log 2>&1 &
server_pid=$!
trap 'kill "$server_pid" 2>/dev/null || true' EXIT

attempt=0
until curl -fsS http://127.0.0.1:18080/download/SHA256SUMS >/dev/null 2>&1; do
	attempt=$((attempt + 1))
	[ "$attempt" -lt 50 ] || fail 'fake download server did not start'
	sleep 0.1
done

attempt=0
until systemctl is-active --quiet default.target; do
	attempt=$((attempt + 1))
	[ "$attempt" -lt 100 ] || fail 'systemd did not reach default.target'
	sleep 0.1
done

if ! env SPEEDTEST_LIBRESPEED_INSTALL_URL=http://127.0.0.1:18080/download \
	sh /test/install.sh --token systemd-test-token --name systemd-test >/tmp/install-systemd-install.log 2>&1; then
	tail -n 20 /tmp/install-systemd-install.log >&2
	fail 'installer failed under systemd'
fi
[ -f /etc/systemd/system/speedtest-agent.service ] || fail 'agent unit was not installed'
systemctl is-enabled --quiet speedtest-agent || fail 'agent unit was not enabled'
systemctl is-active --quiet speedtest-agent || {
	systemctl status speedtest-agent --no-pager >&2 || true
	fail 'agent unit was not active'
}
printf 'ok: installer installed, enabled, and started speedtest-agent under systemd\n'

if ! sh /test/uninstall.sh >/tmp/install-systemd-uninstall.log 2>&1; then
	tail -n 20 /tmp/install-systemd-uninstall.log >&2
	fail 'uninstall path failed under systemd'
fi
[ ! -e /etc/systemd/system/speedtest-agent.service ] || fail 'agent unit remained after uninstall'
[ ! -e /usr/local/bin/speedtest-agent ] || fail 'agent binary remained after uninstall'
if systemctl is-active --quiet speedtest-agent; then fail 'agent service remained active after uninstall'; fi
if systemctl is-enabled --quiet speedtest-agent; then fail 'agent service remained enabled after uninstall'; fi
printf 'PASS: uninstall removed the unit and binary and stopped/disabled the service\n'
