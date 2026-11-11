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
source deploy/docker/schedule-defaults.sh
IMAGE="${STG_IMAGE:-speedtest-staging:latest}"
ENV_FILE="${STG_ENV_FILE:-deploy/docker/stg.env}"
PORT="${STAGING_PORT:-18090}"
NODE_NETWORK="${NODE_NETWORK:-host}"
if [ "$NODE_NETWORK" = host ]; then CENTER_URL="${CENTER_URL:-http://127.0.0.1:${PORT}}"; else CENTER_URL="${CENTER_URL:-http://$(hostname -I | awk '{print $1}'):${PORT}}"; fi
LOCAL="http://127.0.0.1:${PORT}"
NODE_INTERVAL="$(node_interval)"
cmd="${1:-list}"; shift || true

envval() { grep -m1 "^$1=" "$ENV_FILE" | cut -d= -f2- | sed -e 's/^["'\'']//' -e 's/["'\'']$//'; }
jar="$(mktemp)"; trap 'rm -f "$jar"' EXIT
api() { local m="$1" p="$2" b="${3:-}" idem="${4:-}"; local a=(-sS -b "$jar" -c "$jar" -X "$m" "${LOCAL}${p}" -H "Origin: ${LOCAL}" -H "X-CSRF-Token: ${csrf:-}")
  [ -n "$b" ] && a+=(-H 'Content-Type: application/json' -d "$b"); [ "$m" = POST ] && a+=(-H "Idempotency-Key: ${idem:-node-$(date +%s%N)}"); curl "${a[@]}"; }
attempt_dir() {
  if [ -n "${XDG_STATE_HOME:-}" ]; then printf '%s/speedtest/node-attempts' "$XDG_STATE_HOME"
  elif [ -n "${HOME:-}" ]; then printf '%s/.local/state/speedtest/node-attempts' "$HOME"
  else echo 'set HOME or XDG_STATE_HOME for resumable node creation' >&2; return 1
  fi
}
attempt_path() {
  local dir
  dir="$(attempt_dir)" || return 1
  printf '%s/%s.json' "$dir" "$(printf '%s' "$1" | sha256sum | cut -c1-64)"
}
save_attempt() {
  local name="$1" key="$2" stage="$3" node_id="${4:-}" token="${5:-}" dir file tmp
  dir="$(attempt_dir)" || return 1
  mkdir -p "$dir" || return 1
  chmod 0700 "$dir" || return 1
  file="$(attempt_path "$name")"
  tmp="$(mktemp "$dir/.attempt.XXXXXX")" || return 1
  jq -nc --arg key "$key" --arg stage "$stage" --arg id "$node_id" --arg token "$token" \
    '{key:$key,stage:$stage,nodeId:$id,token:$token}' > "$tmp" || { rm -f "$tmp"; return 1; }
  chmod 0600 "$tmp" || { rm -f "$tmp"; return 1; }
  mv -f "$tmp" "$file" || { rm -f "$tmp"; return 1; }
}
remove_attempt() { local file; file="$(attempt_path "$1")" || return 0; rm -f "$file"; }
remove_local_node() {
  local name="$1"
  docker rm -f "speedtest-node-$name" >/dev/null 2>&1 || true
  docker network rm "speedtest-net-$name" >/dev/null 2>&1 || true
  docker volume rm "speedtest-node-$name" >/dev/null 2>&1 || true
}
read_attempt() {
  local file="$1"
  jq -e '(.key|type=="string" and length>0) and (.stage|type=="string")' "$file" >/dev/null 2>&1 || return 1
  ATTEMPT_KEY="$(jq -r '.key' "$file")"
  ATTEMPT_STAGE="$(jq -r '.stage' "$file")"
  ATTEMPT_NODE_ID="$(jq -r '.nodeId // empty' "$file")"
  ATTEMPT_TOKEN="$(jq -r '.token // empty' "$file")"
}
new_idempotency_key() { printf 'docker-node-%s' "$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"; }
find_node_id() {
  local name="$1" response
  response="$(api GET '/api/nodes?size=100')" || { echo 'could not list center nodes' >&2; return 1; }
  jq -e '.success == true and (.data|type=="array")' <<<"$response" >/dev/null 2>&1 || { echo 'center returned an invalid node list' >&2; return 1; }
  jq -r --arg name "$name" '[.data[] | select(.name==$name)][0].id // empty' <<<"$response"
}
login() { [ "$(stat -c %a "$ENV_FILE")" = 600 ] || { echo "$ENV_FILE must be mode 0600"; exit 1; }
  csrf="$(api GET /api/auth/me | jq -r '.data.csrfToken')"
  csrf="$(api POST /api/auth/login "$(jq -nc --arg e "$(envval STAGING_OWNER_EMAIL)" --arg p "$(envval STAGING_OWNER_PASSWORD)" '{email:$e,password:$p}')" | jq -r '.data.csrfToken // empty')"
  [ -n "$csrf" ] || { echo "owner sign-in failed"; exit 1; }; }
set_node_interval() { local id="$1" tag body code attempts=0
  body="$(jq -nc --argjson interval "$NODE_INTERVAL" '{interval:$interval,offset:0,enabled:true}')"
  while (( attempts < 3 )); do
    attempts=$((attempts + 1))
    tag="$(curl -sS -D - -o /dev/null -b "$jar" "${LOCAL}/api/schedules/${id}" | tr -d '\r' | awk -F': ' 'tolower($1)=="etag"{print $2}')" || { echo "could not read schedule for node $id" >&2; return 1; }
    [ -n "$tag" ] || { echo "could not read schedule ETag for node $id" >&2; return 1; }
    code="$(curl -sS -o /dev/null -w '%{http_code}' -b "$jar" -X PUT "${LOCAL}/api/schedules/${id}" -H "Origin: ${LOCAL}" -H "X-CSRF-Token: ${csrf}" -H 'Content-Type: application/json' -H "If-Match: ${tag}" -d "$body")" || { echo "schedule update request failed for node $id" >&2; return 1; }
    [ "$code" = 200 ] && return 0
    [ "$code" = 412 ] || { echo "could not set node $id interval (HTTP $code)" >&2; return 1; }
  done
  echo "schedule for node $id changed during all update attempts" >&2
  return 1
}

case "$cmd" in
up)
  [ $# -ge 1 ] || { echo "usage: nodes.sh up NAME..."; exit 1; }
  require_schedule_interval NODE_INTERVAL "$NODE_INTERVAL"
  login
  new_node_started=0
  for name in "$@"; do
    attempt="$(attempt_path "$name")"
    node_id="$(find_node_id "$name")" || exit 1
    if docker ps -a --format '{{.Names}}' | grep -qx "speedtest-node-$name"; then
      if [ -n "$node_id" ]; then
        remove_attempt "$name"
        echo "node $name already has a container"
        continue
      fi
      remove_attempt "$name"
      remove_local_node "$name"
    fi
    token=""; creation_pending=0
    if [ -f "$attempt" ]; then
      read_attempt "$attempt" || { echo "node $name has invalid creation state; remove it only after checking the center" >&2; exit 1; }
      case "$ATTEMPT_STAGE" in
        creating)
          creation_body="$(jq -nc --arg n "$name" --arg l "Docker node $name" --argjson d "${EXPECTED_DOWN:-500}" --argjson u "${EXPECTED_UP:-500}" '{name:$n,label:$l,expectedDown:$d,expectedUp:$u}')"
          created="$(api POST /api/nodes "$creation_body" "$ATTEMPT_KEY")"
          token="$(printf '%s' "$created" | jq -r '.data.token // empty')"
          node_id="$(printf '%s' "$created" | jq -r '.data.node.id // empty')"
          if [ -z "$token" ] || [ -z "$node_id" ]; then echo "node $name was not created; retry the same attempt or forget it before starting over" >&2; exit 1; fi
          ATTEMPT_STAGE=created; ATTEMPT_NODE_ID="$node_id"; ATTEMPT_TOKEN="$token"
          save_attempt "$name" "$ATTEMPT_KEY" "$ATTEMPT_STAGE" "$node_id" "$token"
          ;;
        created|scheduled)
          node_id="$ATTEMPT_NODE_ID"; token="$ATTEMPT_TOKEN"
          if [ -z "$node_id" ] || [ -z "$token" ]; then echo "node $name has incomplete creation state" >&2; exit 1; fi
          ;;
        *) echo "node $name has unknown creation state" >&2; exit 1 ;;
      esac
      creation_pending=1
    else
      if [ -z "$node_id" ]; then
        remove_local_node "$name"
        ATTEMPT_KEY="$(new_idempotency_key)"
        ATTEMPT_STAGE=creating
        save_attempt "$name" "$ATTEMPT_KEY" "$ATTEMPT_STAGE"
        creation_body="$(jq -nc --arg n "$name" --arg l "Docker node $name" --argjson d "${EXPECTED_DOWN:-500}" --argjson u "${EXPECTED_UP:-500}" '{name:$n,label:$l,expectedDown:$d,expectedUp:$u}')"
        created="$(api POST /api/nodes "$creation_body" "$ATTEMPT_KEY")"
        token="$(printf '%s' "$created" | jq -r '.data.token // empty')"
        node_id="$(printf '%s' "$created" | jq -r '.data.node.id // empty')"
        if [ -z "$token" ] || [ -z "$node_id" ]; then echo "node $name was not created; retry the same attempt or forget it before starting over" >&2; exit 1; fi
        ATTEMPT_STAGE=created; ATTEMPT_NODE_ID="$node_id"; ATTEMPT_TOKEN="$token"
        save_attempt "$name" "$ATTEMPT_KEY" "$ATTEMPT_STAGE" "$node_id" "$token"
        creation_pending=1
      else
        echo "resuming existing node $name without changing its schedule"
      fi
    fi
    if (( creation_pending )); then
      new_node_started=1
      if [ "$ATTEMPT_STAGE" != scheduled ]; then
        set_node_interval "$node_id"
        ATTEMPT_STAGE=scheduled
        save_attempt "$name" "$ATTEMPT_KEY" "$ATTEMPT_STAGE" "$node_id" "$token"
      fi
    fi
    net=host
    if [ "$NODE_NETWORK" != host ]; then net="speedtest-net-$name"; docker network inspect "$net" >/dev/null 2>&1 || docker network create "$net" >/dev/null; fi
    docker volume create "speedtest-node-$name" >/dev/null
    run_args=(run -d --name "speedtest-node-$name" --restart unless-stopped --network "$net" \
      -e "NODE_NAME=$name" -e "CENTER_URL=$CENTER_URL" -e "NODE_INTERVAL=$NODE_INTERVAL")
    [ -n "$token" ] && run_args+=(-e "ENROLL_TOKEN=$token")
    run_args+=(
      -e EXPECTED_DOWN="${EXPECTED_DOWN:-500}" -e EXPECTED_UP="${EXPECTED_UP:-500}" -v "speedtest-node-$name":/var/lib/speedtest \
      --entrypoint /usr/bin/tini "$IMAGE" -- /usr/local/bin/node-entrypoint.sh)
    docker "${run_args[@]}" >/dev/null
    remove_attempt "$name"
    echo "started node $name (network $net)"
  done
  if (( new_node_started )); then
    # Spread new nodes after creation; a restart must leave saved schedules alone.
    etag="$(curl -sS -D - -o /dev/null -b "$jar" "${LOCAL}/api/schedules" | tr -d '\r' | awk -F': ' 'tolower($1)=="etag"{print $2}')"
    curl -sS -o /dev/null -b "$jar" -X POST "${LOCAL}/api/schedules/stagger" -H "Origin: ${LOCAL}" -H "X-CSRF-Token: ${csrf}" -H "If-Match: ${etag}" -H "Idempotency-Key: stagger-$(date +%s%N)" || true
  fi
  ;;
list)
  docker ps -a --filter 'name=^speedtest-node-' --format 'table {{.Names}}\t{{.Status}}\t{{.Networks}}'
  for c in $(docker ps -a --filter 'name=^speedtest-node-' --format '{{.Names}}'); do echo "-- $c"; docker logs --tail 2 "$c" 2>&1 | cut -c1-200; done
  ;;
down|purge)
  for name in "$@"; do docker rm -f "speedtest-node-$name" >/dev/null 2>&1 || true; docker network rm "speedtest-net-$name" >/dev/null 2>&1 || true
    if [ "$cmd" = purge ]; then docker volume rm "speedtest-node-$name" >/dev/null 2>&1 || true; fi
    echo "removed $name"
  done
  ;;
forget)
  login
  for name in "$@"; do
    id="$(find_node_id "$name")" || exit 1
    [ -n "$id" ] || { remove_attempt "$name"; remove_local_node "$name"; echo "no node $name on the center"; continue; }
    etag="$(curl -sS -D - -o /dev/null -b "$jar" "${LOCAL}/api/nodes/${id}" | tr -d '\r' | awk -F': ' 'tolower($1)=="etag"{print $2}')"
    code="$(curl -sS -o /dev/null -w '%{http_code}' -b "$jar" -X DELETE "${LOCAL}/api/nodes/${id}?confirm=true" -H "Origin: ${LOCAL}" -H "X-CSRF-Token: ${csrf}" -H "If-Match: ${etag}")"
    echo "forget $name -> HTTP $code"
    if [[ "$code" =~ ^2[0-9][0-9]$ ]]; then remove_attempt "$name"; remove_local_node "$name"; else echo "could not forget node $name" >&2; exit 1; fi
  done
  ;;
*) sed -n 2,14p "$0"; exit 1;;
esac
