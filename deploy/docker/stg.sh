#!/usr/bin/env bash
# Manage the Speedtest staging container. Run from the repository root; needs Docker access
# (for example `sg docker -c "deploy/docker/stg.sh status"` or membership of the docker group).
#
#   stg.sh build      build binaries (make release) and the image
#   stg.sh up         create and start the container (needs deploy/docker/stg.env, mode 0600)
#   stg.sh status     container state plus the last log lines
#   stg.sh logs       follow the logs
#   stg.sh redeploy   build, then replace the container (data and schedule are kept in the volume)
#   stg.sh down       stop and remove the container (the data volume is kept)
#   stg.sh purge      stop, remove the container AND delete the data volume (asks first)
set -euo pipefail
cd "$(dirname "$0")/../.."
NAME="${STG_CONTAINER:-speedtest-staging}"
IMAGE="${STG_IMAGE:-speedtest-staging:latest}"
VOLUME="${STG_VOLUME:-speedtest-staging-data}"
ENV_FILE="${STG_ENV_FILE:-deploy/docker/stg.env}"

cmd="${1:-status}"
case "$cmd" in
build)
	make release
	docker build -f deploy/docker/Dockerfile -t "$IMAGE" .
	;;
up)
	[ -f "$ENV_FILE" ] || { echo "missing $ENV_FILE (see docs/deployment.md)"; exit 1; }
	[ "$(stat -c %a "$ENV_FILE")" = 600 ] || { echo "$ENV_FILE must have mode 0600"; exit 1; }
	docker volume create "$VOLUME" >/dev/null
	# Host networking: the shared PostgreSQL listens on the host loopback; the center binds to
	# 127.0.0.1 only, so nothing is exposed beyond this machine.
	docker run -d --name "$NAME" --restart unless-stopped --network host \
		--env-file "$ENV_FILE" -v "$VOLUME":/var/lib/speedtest "$IMAGE"
	;;
status)
	docker ps -a --filter "name=^${NAME}$" --format 'table {{.Names}}\t{{.Status}}\t{{.Image}}'
	docker logs --tail 15 "$NAME" 2>&1 | cut -c1-220
	;;
logs)
	docker logs -f --tail 100 "$NAME"
	;;
redeploy)
	"$0" build
	docker rm -f "$NAME" >/dev/null 2>&1 || true
	"$0" up
	;;
down)
	docker rm -f "$NAME"
	;;
purge)
	read -r -p "Delete container ${NAME} and ALL its data volume ${VOLUME}? Type the container name to confirm: " answer
	[ "$answer" = "$NAME" ] || { echo "aborted"; exit 1; }
	docker rm -f "$NAME" >/dev/null 2>&1 || true
	docker volume rm "$VOLUME"
	;;
*)
	echo "usage: $0 build|up|status|logs|redeploy|down|purge" >&2
	exit 2
	;;
esac
