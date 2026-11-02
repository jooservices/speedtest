#!/usr/bin/env bash
set -euo pipefail

if [[ "$(id -u)" != 0 ]]; then
	printf '%s\n' 'SKIP: network namespace verification requires root.'
	exit 0
fi

for required in ip python3 go; do
	if ! command -v "$required" >/dev/null 2>&1; then
		printf 'ERROR: required command not found: %s\n' "$required" >&2
		exit 1
	fi
done

# When invoked with sudo, reuse the invoking developer's populated module
# cache instead of making root attempt to download the same modules.
if [[ -z "${GOMODCACHE:-}" && -n "${SUDO_USER:-}" ]]; then
	user_module_cache="$(sudo -u "$SUDO_USER" -H go env GOMODCACHE)"
	if [[ -d "$user_module_cache" ]]; then
		export GOMODCACHE="$user_module_cache"
	fi
fi

root="$(git rev-parse --show-toplevel)"
suffix="${BASHPID}"
agent_ns="sp16a${suffix}"
center_ns="sp16c${suffix}"
agent_if="a16v${suffix}"
center_if="c16v${suffix}"
center_pid=""

cleanup() {
	if [[ -n "$center_pid" ]]; then
		kill "$center_pid" >/dev/null 2>&1 || true
		wait "$center_pid" >/dev/null 2>&1 || true
	fi
	ip netns del "$agent_ns" >/dev/null 2>&1 || true
	ip netns del "$center_ns" >/dev/null 2>&1 || true
}
trap cleanup EXIT

if ! ip netns add "$agent_ns" 2>/dev/null; then
	printf '%s\n' 'SKIP: this environment cannot create network namespaces (CAP_NET_ADMIN is unavailable).'
	exit 0
fi
if ! ip netns add "$center_ns" 2>/dev/null; then
	printf '%s\n' 'SKIP: this environment cannot create a second network namespace (CAP_NET_ADMIN is unavailable).'
	exit 0
fi

ip link add "$agent_if" type veth peer name "$center_if"
ip link set "$agent_if" netns "$agent_ns"
ip link set "$center_if" netns "$center_ns"
ip -n "$agent_ns" address add 198.18.16.1/24 dev "$agent_if"
ip -n "$agent_ns" link set lo up
ip -n "$agent_ns" link set "$agent_if" up
ip -n "$center_ns" address add 198.18.16.2/24 dev "$center_if"
ip -n "$center_ns" link set lo up
ip -n "$center_ns" link set "$center_if" up

server_code=$(cat <<'PY'
import json
from http.server import BaseHTTPRequestHandler, HTTPServer

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path != "/api/agent/ip":
            self.send_error(404)
            return
        body = json.dumps({
            "success": True,
            "code": 200,
            "message": "OK",
            "data": {"ip": self.client_address[0]},
            "meta": {},
            "errors": None,
        }).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_):
        pass

HTTPServer(("198.18.16.2", 38116), Handler).serve_forever()
PY
)
ip netns exec "$center_ns" python3 -u -c "$server_code" &
center_pid=$!

ready=0
for _ in $(seq 1 50); do
	if ip netns exec "$agent_ns" python3 -c 'import socket; s=socket.create_connection(("198.18.16.2", 38116), timeout=.2); s.close()' >/dev/null 2>&1; then
		ready=1
		break
	fi
	sleep 0.1
done
if [[ "$ready" != 1 ]]; then
	printf '%s\n' 'ERROR: namespace echo server did not become ready.' >&2
	exit 1
fi

mkdir -p "$root/.tmp"
ip netns exec "$agent_ns" env \
	TMPDIR="$root/.tmp" \
	GOTMPDIR="$root/.tmp" \
	GOCACHE="$root/.tmp/go-cache" \
	SPEEDTEST_NETPATH_NAMESPACE_TEST=1 \
	SPEEDTEST_NETPATH_CENTER_URL='http://198.18.16.2:38116' \
	SPEEDTEST_NETPATH_INTERFACE="$agent_if" \
	SPEEDTEST_NETPATH_EXPECTED_IP='198.18.16.1' \
	go test -tags integration ./internal/agent/netpath -run '^TestNamespaceEchoUsesSelectedSourceAddress$' -count=1 -v
