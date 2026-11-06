#!/usr/bin/env bash
# Runs Vector (log and metric shipping to OpenObserve) next to the staging stack.
#
#   observe.sh env      create a template deploy/observability/vector.env (0600) to fill in
#   observe.sh up       start the vector container (host network, Docker socket read-only)
#   observe.sh probe    start the uptime/disk/certificate probe container (one JSON line a minute)
#   observe.sh status   container state and its last lines
#   observe.sh down     stop and remove it
#
# Needs Docker access (sg docker -c "deploy/observability/observe.sh up"). The credentials are never printed by this script.
set -euo pipefail
cd "$(dirname "$0")/../.."
ENV_FILE="${OBSERVE_ENV_FILE:-deploy/observability/vector.env}"
NAME="${OBSERVE_CONTAINER:-speedtest-vector}"
IMAGE="${VECTOR_IMAGE:-timberio/vector:0.43.1-alpine}"

case "${1:-status}" in
env)
  [ -f "$ENV_FILE" ] && { echo "$ENV_FILE exists"; exit 0; }
  umask 077
  printf '# OpenObserve ingestion settings for Vector. Use a dedicated ingest user (not the root user) in production.\nOPENOBSERVE_URL=http://127.0.0.1:15080\nOPENOBSERVE_ORG=default\nOPENOBSERVE_USER=\nOPENOBSERVE_PASSWORD=\n' > "$ENV_FILE"
  echo "wrote a template $ENV_FILE (mode 0600): fill in OPENOBSERVE_USER and OPENOBSERVE_PASSWORD"
  ;;
up)
  [ -f "$ENV_FILE" ] || { echo "missing $ENV_FILE: run observe.sh env"; exit 1; }
  grep -q '^OPENOBSERVE_USER=.' "$ENV_FILE" && grep -q '^OPENOBSERVE_PASSWORD=.' "$ENV_FILE" || { echo "fill in OPENOBSERVE_USER and OPENOBSERVE_PASSWORD in $ENV_FILE"; exit 1; }
  [ "$(stat -c %a "$ENV_FILE")" = 600 ] || { echo "$ENV_FILE must be mode 0600"; exit 1; }
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  docker volume create speedtest-vector-data >/dev/null
  docker run -d --name "$NAME" --restart unless-stopped --network host --env-file "$ENV_FILE" \
    -v "$PWD/deploy/observability/vector.yaml":/etc/vector/vector.yaml:ro -v speedtest-vector-data:/var/lib/vector \
    -v /var/run/docker.sock:/var/run/docker.sock:ro "$IMAGE" --config /etc/vector/vector.yaml
  ;;
probe)
  docker rm -f speedtest-probe >/dev/null 2>&1 || true
  docker run -d --name speedtest-probe --restart unless-stopped --network host -e PROBE_URL="${PROBE_URL:-http://127.0.0.1:18090}" -e PROBE_INTERVAL=60     -v "$PWD/deploy/observability/probe.sh":/probe.sh:ro --entrypoint /bin/sh "${PROBE_IMAGE:-speedtest-staging:latest}" /probe.sh -loop
  ;;
status) docker ps -a --filter "name=^${NAME}$" --format 'table {{.Names}}\t{{.Status}}'; docker logs --tail 8 "$NAME" 2>&1 | cut -c1-200 ;;
down) docker rm -f "$NAME" ;;
*) sed -n 2,10p "$0"; exit 1 ;;
esac
