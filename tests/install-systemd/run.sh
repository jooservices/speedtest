#!/usr/bin/env bash
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../.." && pwd)"

if ! command -v docker >/dev/null 2>&1; then
	printf 'SKIP: Docker CLI is unavailable; systemd installer test was not run.\n'
	exit 0
fi

mkdir -p "$repo/.tmp"
work="$(mktemp -d "$repo/.tmp/install-systemd.XXXXXX")"
image="speedtest-install-systemd:joost83-${BASHPID}"
container="speedtest-install-systemd-${BASHPID}"

docker_cli() {
	if docker info --format '{{.ServerVersion}}' >/dev/null 2>&1; then
		docker "$@"
		return
	fi
	local quoted
	printf -v quoted '%q ' "$@"
	sg docker -c "docker $quoted"
}

cleanup() {
	docker_cli rm -f "$container" >/dev/null 2>&1 || true
	docker_cli image rm "$image" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT

download="$work/www/download"
release="$work/release"
mkdir -p "$download" "$release" "$work/libre"

cat >"$release/speedtest-agent-linux-amd64" <<'EOF'
#!/bin/sh
case "$1" in
enroll)
	printf 'test-credential\n' >/etc/speedtest-agent/credential
	;;
update-apply)
	;;
run)
	exec sleep infinity
	;;
esac
EOF
chmod 0755 "$release/speedtest-agent-linux-amd64"
cp "$release/speedtest-agent-linux-amd64" "$download/speedtest-agent-linux-amd64"
cp "$repo/deploy/speedtest-agent.service" "$download/speedtest-agent.service"
(cd "$download" && sha256sum speedtest-agent-linux-amd64 speedtest-agent.service >SHA256SUMS)

cat >"$work/libre/librespeed-cli" <<'EOF'
#!/bin/sh
exit 0
EOF
chmod 0755 "$work/libre/librespeed-cli"
tar -czf "$download/librespeed-cli_1.0.14_linux_amd64.tar.gz" -C "$work/libre" librespeed-cli
(cd "$download" && sha256sum librespeed-cli_1.0.14_linux_amd64.tar.gz >checksums.txt)

sed 's|__CENTER_URL__|http://127.0.0.1:18080|' \
	"$repo/internal/center/install/install.sh" >"$work/install.sh"
cp "$repo/internal/center/installer/uninstall_remote.sh" "$work/uninstall.sh"
cp "$here/in-container.sh" "$work/in-container.sh"

docker_cli build -f "$here/Dockerfile" -t "$image" "$here"
docker_cli run -d --name "$container" --privileged --cgroupns=private \
	--tmpfs /run --tmpfs /run/lock \
	-v "$work:/test:ro" "$image" >/dev/null
if ! docker_cli exec "$container" /bin/bash /test/in-container.sh; then
	docker_cli logs "$container" >&2 || true
	exit 1
fi
