#!/bin/sh
set -eu

CONFIG_FILE=$1
# shellcheck source=/dev/null
. "$CONFIG_FILE"
decode() { printf '%s' "$1" | base64 --decode; }
CENTER_URL=$(decode "$CENTER_URL_B64")
NODE_NAME=$(decode "$NODE_NAME_B64")
WORK_DIR="/var/tmp/speedtest-installer-$INSTALL_ID"
BACKUP="$WORK_DIR/backup"
PREPARED=0

restore() {
	result=$?
	trap - EXIT HUP INT TERM
	if [ "$PREPARED" -eq 1 ]; then
		exit "$result"
	fi
	if [ -f "$WORK_DIR/state" ]; then
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
		if [ "$CREATED_USER" -eq 1 ]; then
			userdel -r speedtest-agent >/dev/null 2>&1 || true
		fi
		systemctl daemon-reload >/dev/null 2>&1 || true
		if [ "$WAS_ENABLED" -eq 1 ]; then systemctl enable speedtest-agent >/dev/null 2>&1 || true; fi
		if [ "$WAS_ACTIVE" -eq 1 ]; then systemctl start speedtest-agent >/dev/null 2>&1 || true; fi
	fi
	rm -rf "$WORK_DIR" "$CONFIG_FILE"
	exit "$result"
}
trap restore EXIT HUP INT TERM

printf 'STP|detect|5\n'
[ "$(uname -s)" = "Linux" ] || { printf 'STP|unsupported|0\n'; exit 20; }
machine=$(uname -m)
case "$machine" in
	x86_64|amd64) arch=amd64 ;;
	aarch64|arm64) arch=arm64 ;;
	*) printf 'STP|unsupported|0\n'; exit 20 ;;
esac
command -v systemctl >/dev/null 2>&1 || exit 21
command -v install >/dev/null 2>&1 || exit 21

mkdir -p "$WORK_DIR" "$BACKUP/usr/local/bin" "$BACKUP/etc/systemd/system"
HAD_BINARY=0
HAD_UNIT=0
HAD_CONFIG=0
CREATED_USER=0
WAS_ACTIVE=0
WAS_ENABLED=0
if [ -e /usr/local/bin/speedtest-agent ]; then
	cp -a /usr/local/bin/speedtest-agent "$BACKUP/usr/local/bin/speedtest-agent"
	HAD_BINARY=1
fi
if [ -e /etc/systemd/system/speedtest-agent.service ]; then
	cp -a /etc/systemd/system/speedtest-agent.service "$BACKUP/etc/systemd/system/speedtest-agent.service"
	HAD_UNIT=1
fi
if [ -d /etc/speedtest-agent ]; then
	mkdir -p "$BACKUP/etc"
	cp -a /etc/speedtest-agent "$BACKUP/etc/speedtest-agent"
	HAD_CONFIG=1
fi
if id speedtest-agent >/dev/null 2>&1; then :; else CREATED_USER=1; fi
if systemctl is-active --quiet speedtest-agent; then WAS_ACTIVE=1; fi
if systemctl is-enabled --quiet speedtest-agent; then WAS_ENABLED=1; fi
printf 'HAD_BINARY=%s\nHAD_UNIT=%s\nHAD_CONFIG=%s\nCREATED_USER=%s\nWAS_ACTIVE=%s\nWAS_ENABLED=%s\n' \
	"$HAD_BINARY" "$HAD_UNIT" "$HAD_CONFIG" "$CREATED_USER" "$WAS_ACTIVE" "$WAS_ENABLED" > "$WORK_DIR/state"
chmod 0600 "$WORK_DIR/state"

binary="speedtest-agent-linux-$arch"
printf 'STP|download|20\n'
fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --max-time 120 -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -T 120 -O "$2" "$1"
	else
		return 1
	fi
}
fetch "$CENTER_URL/download/$binary" "$WORK_DIR/$binary"
fetch "$CENTER_URL/download/SHA256SUMS" "$WORK_DIR/SHA256SUMS"
fetch "$CENTER_URL/download/speedtest-agent.service" "$WORK_DIR/speedtest-agent.service"

printf 'STP|verify|35\n'
expected=$(awk -v f="$binary" '$2 == f || $2 == "*" f { print $1 }' "$WORK_DIR/SHA256SUMS")
[ -n "$expected" ] || exit 22
actual=$(sha256sum "$WORK_DIR/$binary" | awk '{ print $1 }')
[ "$expected" = "$actual" ] || exit 22
expected=$(awk '$2 == "speedtest-agent.service" || $2 == "*speedtest-agent.service" { print $1 }' "$WORK_DIR/SHA256SUMS")
[ -n "$expected" ] || exit 22
actual=$(sha256sum "$WORK_DIR/speedtest-agent.service" | awk '{ print $1 }')
[ "$expected" = "$actual" ] || exit 22

printf 'STP|user|50\n'
if [ "$CREATED_USER" -eq 1 ]; then
	useradd --system --home-dir /var/lib/speedtest-agent --create-home --shell /usr/sbin/nologin speedtest-agent
fi
install -d -m 0750 -o speedtest-agent -g speedtest-agent /etc/speedtest-agent
systemctl stop speedtest-agent >/dev/null 2>&1 || true
install -m 0755 "$WORK_DIR/$binary" /usr/local/bin/speedtest-agent
install -m 0644 "$WORK_DIR/speedtest-agent.service" /etc/systemd/system/speedtest-agent.service
printf 'SPEEDTEST_AGENT_CENTER=%s\n' "$CENTER_URL" > /etc/speedtest-agent/.env
chown speedtest-agent:speedtest-agent /etc/speedtest-agent/.env
chmod 0640 /etc/speedtest-agent/.env

mkfifo "$WORK_DIR/token.pipe"
chown speedtest-agent:speedtest-agent "$WORK_DIR/token.pipe"
printf 'STP|enroll|70\n'
TOKEN=$(cat "$WORK_DIR/token.pipe")
rm -f "$WORK_DIR/token.pipe"
	printf '%s\n' "$TOKEN" | runuser -u speedtest-agent -- /usr/local/bin/speedtest-agent enroll \
		--center "$CENTER_URL" --token-stdin --repair --name "$NODE_NAME"
unset TOKEN
chown speedtest-agent:speedtest-agent /etc/speedtest-agent/credential
chmod 0600 /etc/speedtest-agent/credential

printf 'STP|start|85\n'
systemctl daemon-reload
systemctl enable speedtest-agent
systemctl restart speedtest-agent
PREPARED=1
trap - EXIT HUP INT TERM
printf 'STP|prepared|90\n'
