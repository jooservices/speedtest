#!/usr/bin/env bash
# Extra node containers ("VMs") that report to a running center (the staging center by default).
# Each node has its own container, volume and config; it knows only its own name and the center address.
# Network: NODE_NETWORK=host (default) shares the host network like the staging container. Some hosts block
# bridge-to-host traffic (firewall), which stops a bridge node reaching a host-network center; set
# NODE_NETWORK=bridge to give every node its own Docker network when that path is open (CENTER_URL = LAN address).
#
#   nodes.sh up NAME [NAME...]   create each node on the center (owner API) and start its container
#   nodes.sh list                containers and their last log line
#   nodes.sh down NAME [NAME...] stop and remove the containers (the nodes stay on the center, and keep their data)
#   nodes.sh purge NAME...       also delete the container volume
#   nodes.sh forget NAME...      delete the node (and its results) from the center
#
# Needs Docker access (sg docker -c "deploy/docker/nodes.sh ...") and deploy/docker/stg.env (0600) for the
# owner credentials; they are read by parsing, never sourced or printed.
# Environment: CENTER_URL (default http://<host LAN IP>:18090), NODE_INTERVAL (60), EXPECTED_DOWN/UP (500).
set -euo pipefail
cd "$(dirname "$0")/../.."
IMAGE="${STG_IMAGE:-speedtest-staging:latest}"
ENV_FILE="${STG_ENV_FILE:-deploy/docker/stg.env}"
PORT="${STAGING_PORT:-18090}"
NODE_NETWORK="${NODE_NETWORK:-host}"
if [ "$NODE_NETWORK" = host ]; then CENTER_URL="${CENTER_URL:-http://127.0.0.1:${PORT}}"; else CENTER_URL="${CENTER_URL:-http://$(hostname -I | awk '{print $1}'):${PORT}}"; fi
LOCAL="http://127.0.0.1:${PORT}"
NODE_INTERVAL="${NODE_INTERVAL:-60}"
cmd="${1:-list}"; shift || true

envval() { grep -m1 "^$1=" "$ENV_FILE" | cut -d= -f2- | sed -e 's/^["'\'']//' -e 's/["'\'']$//'; }
jar="$(mktemp)"; trap 'rm -f "$jar"' EXIT
api() { local m="$1" p="$2" b="${3:-}"; local a=(-sS -b "$jar" -c "$jar" -X "$m" "${LOCAL}${p}" -H "Origin: ${LOCAL}" -H "X-CSRF-Token: ${csrf:-}")
  [ -n "$b" ] && a+=(-H 'Content-Type: application/json' -d "$b"); [ "$m" = POST ] && a+=(-H "Idempotency-Key: node-$(date +%s%N)"); curl "${a[@]}"; }
login() { [ "$(stat -c %a "$ENV_FILE")" = 600 ] || { echo "$ENV_FILE must be mode 0600"; exit 1; }
  csrf="$(api GET /api/auth/me | jq -r '.data.csrfToken')"
  csrf="$(api POST /api/auth/login "$(jq -nc --arg e "$(envval STAGING_OWNER_EMAIL)" --arg p "$(envval STAGING_OWNER_PASSWORD)" '{email:$e,password:$p}')" | jq -r '.data.csrfToken // empty')"
  [ -n "$csrf" ] || { echo "owner sign-in failed"; exit 1; }; }

case "$cmd" in
up)
  [ $# -ge 1 ] || { echo "usage: nodes.sh up NAME..."; exit 1; }
  login
  for name in "$@"; do
    if docker ps -a --format '{{.Names}}' | grep -qx "speedtest-node-$name"; then echo "node $name already has a container"; continue; fi
    created="$(api POST /api/nodes "$(jq -nc --arg n "$name" --arg l "Docker node $name" --argjson d "${EXPECTED_DOWN:-500}" --argjson u "${EXPECTED_UP:-500}" '{name:$n,label:$l,expectedDown:$d,expectedUp:$u}')")"
    token="$(printf '%s' "$created" | jq -r '.data.token // empty')"; node_id="$(printf '%s' "$created" | jq -r '.data.node.id // empty')"
    if [ -z "$token" ] || [ -z "$node_id" ]; then echo "node $name was not created: $(printf '%s' "$created" | head -c 300)"; exit 1; fi
    # The center schedule must carry the same interval as the agent (the node is created with the center default).
    sched_etag="$(curl -sS -D - -o /dev/null -b "$jar" "${LOCAL}/api/schedules/${node_id}" | tr -d '\r' | awk -F': ' 'tolower($1)=="etag"{print $2}')"
    code="$(curl -sS -o /dev/null -w '%{http_code}' -b "$jar" -X PUT "${LOCAL}/api/schedules/${node_id}" -H "Origin: ${LOCAL}" -H "X-CSRF-Token: ${csrf}" \
      -H "If-Match: ${sched_etag}" -H 'Content-Type: application/json' -d "$(jq -nc --argjson i "$NODE_INTERVAL" '{interval:$i,offset:0,enabled:true}')")"
    case "$code" in 2??) ;; *) echo "node $name: interval ${NODE_INTERVAL} not saved on the center (HTTP $code)"; exit 1;; esac
    net=host
    if [ "$NODE_NETWORK" != host ]; then net="speedtest-net-$name"; docker network inspect "$net" >/dev/null 2>&1 || docker network create "$net" >/dev/null; fi
    docker volume create "speedtest-node-$name" >/dev/null
    docker run -d --name "speedtest-node-$name" --restart unless-stopped --network "$net" \
      -e NODE_NAME="$name" -e CENTER_URL="$CENTER_URL" -e ENROLL_TOKEN="$token" -e NODE_INTERVAL="$NODE_INTERVAL" \
      -e EXPECTED_DOWN="${EXPECTED_DOWN:-500}" -e EXPECTED_UP="${EXPECTED_UP:-500}" -v "speedtest-node-$name":/var/lib/speedtest \
      --entrypoint /usr/bin/tini "$IMAGE" -- /usr/local/bin/node-entrypoint.sh >/dev/null
    echo "started node $name (network $net)"
  done
  # Spread the start minutes so nodes never test at the same time.
  etag="$(curl -sS -D - -o /dev/null -b "$jar" "${LOCAL}/api/schedules" | tr -d '\r' | awk -F': ' 'tolower($1)=="etag"{print $2}')"
  curl -sS -o /dev/null -b "$jar" -X POST "${LOCAL}/api/schedules/stagger" -H "Origin: ${LOCAL}" -H "X-CSRF-Token: ${csrf}" -H "If-Match: ${etag}" -H "Idempotency-Key: stagger-$(date +%s%N)" || true
  ;;
list)
  docker ps -a --filter 'name=^speedtest-node-' --format 'table {{.Names}}\t{{.Status}}\t{{.Networks}}'
  for c in $(docker ps -a --filter 'name=^speedtest-node-' --format '{{.Names}}'); do echo "-- $c"; docker logs --tail 2 "$c" 2>&1 | cut -c1-200; done
  ;;
down|purge)
  for name in "$@"; do docker rm -f "speedtest-node-$name" >/dev/null 2>&1 || true; docker network rm "speedtest-net-$name" >/dev/null 2>&1 || true
    if [ "$cmd" = purge ]; then docker volume rm "speedtest-node-$name" >/dev/null 2>&1 || true; fi; echo "removed $name"; done
  ;;
forget)
  login
  for name in "$@"; do
    id="$(api GET "/api/nodes?size=100" | jq -r --arg n "$name" '.data[] | select(.name==$n) | .id')"
    [ -n "$id" ] || { echo "no node $name on the center"; continue; }
    etag="$(curl -sS -D - -o /dev/null -b "$jar" "${LOCAL}/api/nodes/${id}" | tr -d '\r' | awk -F': ' 'tolower($1)=="etag"{print $2}')"
    code="$(curl -sS -o /dev/null -w '%{http_code}' -b "$jar" -X DELETE "${LOCAL}/api/nodes/${id}?confirm=true" -H "Origin: ${LOCAL}" -H "X-CSRF-Token: ${csrf}" -H "If-Match: ${etag}")"
    echo "forget $name -> HTTP $code"
  done
  ;;
*) sed -n 2,14p "$0"; exit 1;;
esac
