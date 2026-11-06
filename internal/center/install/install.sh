#!/bin/sh
# Speedtest agent installer, served by the command center at /install.sh.
#
#   curl -fsSL https://CENTER/install.sh | sudo sh -s -- --token TOKEN --name NAME
#
# It downloads the agent for this machine from the command center, verifies its
# SHA-256 checksum, installs it as a systemd service under a dedicated user and
# enrols it with the one-time token. The token is single use and expires; it is
# briefly visible in the process list while enrolling.
#
# Test hooks (never needed in normal use): SPEEDTEST_INSTALL_ROOT installs under
# that directory without a dedicated user or systemd; SPEEDTEST_INSTALL_UNAME_M
# overrides the detected machine architecture.
set -eu

CENTER_URL="__CENTER_URL__"
ROOT="${SPEEDTEST_INSTALL_ROOT:-}"

usage() {
	cat <<'EOF'
Usage: install.sh --token TOKEN --name NAME

  --token TOKEN   one-time enrolment token shown by the dashboard
  --name NAME     name of this node (letters, digits, dot, dash, underscore, space)
EOF
}

die() {
	printf 'install: %s\n' "$*" >&2
	exit 1
}

token=""
name=""
while [ $# -gt 0 ]; do
	case "$1" in
	--token)
		[ $# -ge 2 ] || { usage >&2; exit 2; }
		token="$2"
		shift 2
		;;
	--name)
		[ $# -ge 2 ] || { usage >&2; exit 2; }
		name="$2"
		shift 2
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		printf 'install: unknown option: %s\n' "$1" >&2
		usage >&2
		exit 2
		;;
	esac
done
if [ -z "$token" ] || [ -z "$name" ]; then
	usage >&2
	exit 2
fi
case "$name" in
*[!A-Za-z0-9._\ -]*) die "the node name may only contain letters, digits, dot, dash, underscore and space" ;;
esac
if [ "${#name}" -gt 64 ]; then die "the node name is too long (64 characters at most)"; fi

[ "$(uname -s)" = "Linux" ] || die "this installer supports Linux only"
if [ -z "$ROOT" ] && [ "$(id -u)" -ne 0 ]; then
	die "run as root, for example: curl -fsSL $CENTER_URL/install.sh | sudo sh -s -- --token ... --name ..."
fi

machine="${SPEEDTEST_INSTALL_UNAME_M:-$(uname -m)}"
case "$machine" in
x86_64 | amd64) arch="amd64" ;;
aarch64 | arm64) arch="arm64" ;;
*) die "unsupported architecture: $machine (supported: x86_64, aarch64)" ;;
esac
binary="speedtest-agent-linux-$arch"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fetch() { # url destination
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --max-time 120 -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -T 120 -O "$2" "$1"
	else
		die "curl or wget is required"
	fi
}

printf 'Downloading %s from %s\n' "$binary" "$CENTER_URL"
fetch "$CENTER_URL/download/$binary" "$tmp/$binary" || die "could not download the agent"
fetch "$CENTER_URL/download/SHA256SUMS" "$tmp/SHA256SUMS" || die "could not download the checksum list"
fetch "$CENTER_URL/download/speedtest-agent.service" "$tmp/speedtest-agent.service" || die "could not download the service unit"

expected="$(awk -v f="$binary" '$2 == f || $2 == "*" f { print $1 }' "$tmp/SHA256SUMS")"
[ -n "$expected" ] || die "no checksum listed for $binary"
actual="$(sha256sum "$tmp/$binary" | awk '{ print $1 }')"
[ "$expected" = "$actual" ] || die "checksum mismatch for $binary; nothing was installed"
printf 'Checksum verified.\n'

# Verify LibreSpeed before installing either executable.
libre_version="${SPEEDTEST_LIBRESPEED_INSTALL_VERSION:-1.0.14}"
case "$libre_version" in *[!0-9.]* | '') die "invalid LibreSpeed version" ;; esac
libre_base="${SPEEDTEST_LIBRESPEED_INSTALL_URL:-https://github.com/librespeed/speedtest-cli/releases/download/v$libre_version}"
libre_archive="librespeed-cli_${libre_version}_linux_${arch}.tar.gz"
fetch "$libre_base/$libre_archive" "$tmp/$libre_archive" || die "could not download LibreSpeed"
fetch "$libre_base/checksums.txt" "$tmp/libre-checksums" || die "could not download LibreSpeed checksums"
expected="$(awk -v f="$libre_archive" '$2 == f || $2 == "*" f { print $1 }' "$tmp/libre-checksums")"
actual="$(sha256sum "$tmp/$libre_archive" | awk '{ print $1 }')"
if [ -z "$expected" ] || [ "$expected" != "$actual" ]; then die "checksum mismatch for LibreSpeed; nothing was installed"; fi
tar -xOzf "$tmp/$libre_archive" librespeed-cli >"$tmp/librespeed-cli" || die "invalid LibreSpeed archive"
[ -s "$tmp/librespeed-cli" ] || die "empty LibreSpeed executable"

install -d -m 0755 "$ROOT/usr/local/bin"
install -m 0755 "$tmp/librespeed-cli" "$ROOT/usr/local/bin/librespeed-cli"
install -m 0755 "$tmp/$binary" "$ROOT/usr/local/bin/speedtest-agent"

if [ -z "$ROOT" ]; then
	id speedtest-agent >/dev/null 2>&1 ||
		useradd --system --home-dir /var/lib/speedtest-agent --create-home --shell /usr/sbin/nologin speedtest-agent
	install -d -m 0750 -o speedtest-agent -g speedtest-agent /etc/speedtest-agent
	runuser -u speedtest-agent -- /usr/local/bin/speedtest-agent enroll --center "$CENTER_URL" --token "$token" --name "$name"
	# The service refuses to start without the center address; it reads it (and the local seed config) from here.
	printf 'SPEEDTEST_AGENT_CENTER=%s\n' "$CENTER_URL" >/etc/speedtest-agent/.env
	printf 'name: %s\n' "$name" >/etc/speedtest-agent/config.yaml
	chown speedtest-agent:speedtest-agent /etc/speedtest-agent/.env /etc/speedtest-agent/config.yaml
	chmod 0640 /etc/speedtest-agent/.env /etc/speedtest-agent/config.yaml
	install -m 0644 "$tmp/speedtest-agent.service" /etc/systemd/system/speedtest-agent.service
	systemctl daemon-reload
	systemctl enable --now speedtest-agent
	printf 'The agent is installed and running as the speedtest-agent service.\n'
else
	install -d -m 0750 "$ROOT/etc/speedtest-agent"
	SPEEDTEST_AGENT_CREDENTIAL="$ROOT/etc/speedtest-agent/credential" \
		SPEEDTEST_AGENT_CONFIG="$ROOT/etc/speedtest-agent/config.yaml" \
		"$ROOT/usr/local/bin/speedtest-agent" enroll --center "$CENTER_URL" --token "$token" --name "$name"
	printf 'SPEEDTEST_AGENT_CENTER=%s\n' "$CENTER_URL" >"$ROOT/etc/speedtest-agent/.env"
	printf 'name: %s\n' "$name" >"$ROOT/etc/speedtest-agent/config.yaml"
	install -d -m 0755 "$ROOT/etc/systemd/system"
	install -m 0644 "$tmp/speedtest-agent.service" "$ROOT/etc/systemd/system/speedtest-agent.service"
	printf 'Installed under %s (test mode: no user, no systemd).\n' "$ROOT"
fi
