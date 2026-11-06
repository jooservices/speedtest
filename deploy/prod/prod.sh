#!/usr/bin/env bash
# Deploy the command center to the production host over SSH. Run from the repository root.
#
#   prod.sh deploy     build (make release), ship the image, replace the container, wait for /readyz
#   prod.sh rollback   run the previous image again
#   prod.sh status     container state and the last log lines
#   prod.sh logs       follow the logs
#
# Host settings live in /etc/speedtest/center.env on the host (root, mode 0600); this script never reads or prints them.
# Overrides: PROD_HOST (default joos@192.168.10.10), PROD_PORT (18090), PROD_VERSION (git describe).
set -euo pipefail
cd "$(dirname "$0")/../.."
HOST="${PROD_HOST:-joos@192.168.10.10}"
PORT="${PROD_PORT:-18090}"
NAME=speedtest-center
VERSION="${PROD_VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
remote() { ssh -o BatchMode=yes "$HOST" "$@"; }

run_container() { # image tag
	remote "sudo docker rm -f $NAME >/dev/null 2>&1 || true; sudo docker run -d --name $NAME --restart unless-stopped --network host \
		--env-file /etc/speedtest/center.env -e SPEEDTEST_PORT=$PORT -v speedtest-center-data:/var/lib/speedtest $1"
}
wait_ready() {
	for _ in $(seq 1 40); do
		remote "curl -fsS http://127.0.0.1:$PORT/readyz >/dev/null" 2>/dev/null && { echo "ready"; return 0; }
		sleep 3
	done
	echo "NOT READY: see '$0 logs'" >&2
	return 1
}

case "${1:-status}" in
deploy)
	make release VERSION="${VERSION#v}"
	docker build -f deploy/prod/Dockerfile -t "speedtest-center:$VERSION" .
	docker save "speedtest-center:$VERSION" | remote "sudo docker load"
	prev="$(remote "sudo docker inspect $NAME --format '{{.Config.Image}}' 2>/dev/null" || true)"
	[ -n "$prev" ] && remote "sudo docker tag $prev speedtest-center:previous"
	run_container "speedtest-center:$VERSION"
	if ! wait_ready; then echo "deploy failed; run '$0 rollback'" >&2; exit 1; fi
	;;
rollback)
	run_container speedtest-center:previous
	wait_ready
	;;
status)
	remote "sudo docker ps -a --filter name=^$NAME\$ --format 'table {{.Names}}\t{{.Status}}\t{{.Image}}'; sudo docker logs --tail 15 $NAME 2>&1 | cut -c1-220"
	;;
logs)
	remote "sudo docker logs -f --tail 100 $NAME"
	;;
*)
	echo "usage: $0 deploy|rollback|status|logs" >&2
	exit 2
	;;
esac
