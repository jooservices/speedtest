#!/bin/sh
set -eu
INSTALL_ID=$1
WORK_DIR="/var/tmp/speedtest-installer-$INSTALL_ID"
CONFIG_FILE="$WORK_DIR/config.env"
BACKUP="$WORK_DIR/backup"
if [ ! -f "$WORK_DIR/state" ]; then
	rm -rf "$WORK_DIR" "$CONFIG_FILE"
	exit 0
fi
# shellcheck source=/dev/null
. "$WORK_DIR/state"
systemctl disable --now speedtest-agent >/dev/null 2>&1 || true
if [ "$HAD_BINARY" -eq 1 ]; then
	install -D -m 0755 "$BACKUP/usr/local/bin/speedtest-agent" /usr/local/bin/speedtest-agent
else
	rm -f /usr/local/bin/speedtest-agent
fi
if [ "$HAD_UNIT" -eq 1 ]; then
	install -D -m 0644 "$BACKUP/etc/systemd/system/speedtest-agent.service" /etc/systemd/system/speedtest-agent.service
else
	rm -f /etc/systemd/system/speedtest-agent.service
fi
if [ "$HAD_CONFIG" -eq 1 ]; then
	rm -rf /etc/speedtest-agent
	cp -a "$BACKUP/etc/speedtest-agent" /etc/speedtest-agent
else
	rm -rf /etc/speedtest-agent
fi
if [ "$CREATED_USER" -eq 1 ]; then userdel -r speedtest-agent >/dev/null 2>&1 || true; fi
systemctl daemon-reload >/dev/null 2>&1 || true
if [ "$WAS_ENABLED" -eq 1 ]; then systemctl enable speedtest-agent >/dev/null 2>&1 || true; fi
if [ "$WAS_ACTIVE" -eq 1 ]; then systemctl start speedtest-agent >/dev/null 2>&1 || true; fi
rm -rf "$WORK_DIR" "$CONFIG_FILE"
