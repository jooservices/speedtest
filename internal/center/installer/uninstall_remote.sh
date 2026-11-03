#!/bin/sh
# Removes the Speedtest agent from this machine. It deletes only the agent's own service, binary, directories and account.
# AGENT_ROOT is empty on a real machine; tests set it to a scratch directory.
set -eu
ROOT="${AGENT_ROOT:-}"
echo "STP|stop|15"
if [ -z "$ROOT" ]; then systemctl disable --now speedtest-agent >/dev/null 2>&1 || true; fi
echo "STP|files|50"
rm -f "$ROOT/etc/systemd/system/speedtest-agent.service" "$ROOT/usr/local/bin/speedtest-agent" "$ROOT/usr/local/bin/speedtest-agent.prev"
rm -rf "$ROOT/etc/speedtest-agent" "$ROOT/var/lib/speedtest-agent" "$ROOT/var/lib/speedtest-agent-update"
echo "STP|account|80"
if [ -z "$ROOT" ]; then
	if id speedtest-agent >/dev/null 2>&1; then userdel speedtest-agent >/dev/null 2>&1 || true; fi
	systemctl daemon-reload >/dev/null 2>&1 || true
fi
echo "STP|removed|100"
