#!/usr/bin/env bash
# Container entrypoint: wait for the database, migrate, start the center, bootstrap the owner
# account and the nodes once, then run one agent per node. If any process stops the container
# exits so Docker's restart policy brings everything back (state lives in the volume).
set -euo pipefail

STATE=/var/lib/speedtest
PORT="${STAGING_PORT:-18090}"
INTERVAL="${STAGING_INTERVAL_MINUTES:-30}"
NODES="${STAGING_NODES:-VM1:0 VM2:10 VM3:20}"          # name:startMinuteOffset ...
EXPECTED_DOWN="${STAGING_EXPECTED_DOWN_MBPS:-500}"
EXPECTED_UP="${STAGING_EXPECTED_UP_MBPS:-500}"
CENTER="http://127.0.0.1:${PORT}"
BIND="${STAGING_BIND:-0.0.0.0}"
export APP_ENV="${APP_ENV:-local}" SPEEDTEST_HTTP_ADDR="${BIND}:${PORT}"

log() { printf '%s entrypoint: %s\n' "$(date -u +%FT%TZ)" "$*"; }
die() { log "ERROR: $*"; exit 1; }
: "${SPEEDTEST_DB_HOST:?}" "${SPEEDTEST_DB_PORT:?}" "${SPEEDTEST_DB_NAME:?}" "${SPEEDTEST_DB_USER:?}" "${SPEEDTEST_DB_PASSWORD:?}"
: "${STAGING_OWNER_EMAIL:?}" "${STAGING_OWNER_PASSWORD:?}" "${APP_KEY:?}"
mkdir -p "$STATE/agents"

psql_run() { PGPASSWORD="$SPEEDTEST_DB_PASSWORD" psql -h "$SPEEDTEST_DB_HOST" -p "$SPEEDTEST_DB_PORT" -U "$SPEEDTEST_DB_USER" -d "$SPEEDTEST_DB_NAME" -v ON_ERROR_STOP=1 -qAt "$@"; }

log "waiting for the database"
for _ in $(seq 1 60); do psql_run -c 'select 1' >/dev/null 2>&1 && break; sleep 2; done
psql_run -c 'select 1' >/dev/null 2>&1 || die "database not reachable"

log "applying migrations"
speedtest-migrate up

log "starting the command center on ${CENTER}"
speedtest-center seed-admin
speedtest-center &
center_pid=$!
for _ in $(seq 1 60); do curl -fsS "${CENTER}/healthz" >/dev/null 2>&1 && break; sleep 1; done
curl -fsS "${CENTER}/healthz" >/dev/null 2>&1 || die "the command center did not become healthy"

jar="$STATE/.jar"
api() { # method path [json] -> body on stdout; uses jar, csrf, same-origin header
  local method="$1" path="$2" body="${3:-}"
  local args=(-sS -b "$jar" -c "$jar" -X "$method" "${CENTER}${path}" -H "Origin: ${CENTER}" -H "X-CSRF-Token: ${csrf:-}")
  if [ -n "$body" ]; then args+=(-H 'Content-Type: application/json' -d "$body"); fi
  if [ "$method" = POST ]; then args+=(-H "Idempotency-Key: stg-$(date +%s%N)"); fi
  curl "${args[@]}"
}
login() { # email password
  rm -f "$jar"; csrf=""
  csrf="$(api GET /api/auth/me | jq -r '.data.csrfToken')"
  local out; out="$(api POST /api/auth/login "$(jq -nc --arg e "$1" --arg p "$2" '{email:$e,password:$p}')")"
  csrf="$(printf '%s' "$out" | jq -r '.data.csrfToken // empty')"
  [ -n "$csrf" ]
}

if [ ! -f "$STATE/owner-ready" ]; then
  log "setting the owner password"
  if login "$STAGING_OWNER_EMAIL" "$STAGING_OWNER_PASSWORD" 2>/dev/null; then
    log "owner password already set"
  else
    login "$STAGING_OWNER_EMAIL" "password" || die "cannot sign in with the default owner account"
    out="$(api POST /api/auth/password "$(jq -nc --arg c password --arg n "$STAGING_OWNER_PASSWORD" '{currentPassword:$c,newPassword:$n}')")"
    login "$STAGING_OWNER_EMAIL" "$STAGING_OWNER_PASSWORD" || die "password change failed: $(printf '%s' "$out" | head -c 200)"
  fi
  touch "$STATE/owner-ready"
else
  login "$STAGING_OWNER_EMAIL" "$STAGING_OWNER_PASSWORD" || die "cannot sign in as the owner"
fi

agent_pids=()
for spec in $NODES; do
  name="${spec%%:*}"; offset="${spec##*:}"
  dir="$STATE/agents/$name"; mkdir -p "$dir"; chmod 0700 "$dir"
  if [ ! -f "$dir/enrolled" ]; then
    log "creating and enrolling node ${name} (offset ${offset} min, every ${INTERVAL} min)"
    created="$(api POST /api/nodes "$(jq -nc --arg n "$name" --arg l "Staging ${name}" --argjson d "$EXPECTED_DOWN" --argjson u "$EXPECTED_UP" '{name:$n,label:$l,expectedDown:$d,expectedUp:$u}')")"
    token="$(printf '%s' "$created" | jq -r '.data.token // empty')"; node_id="$(printf '%s' "$created" | jq -r '.data.node.id // empty')"
    if [ -z "$token" ] || [ -z "$node_id" ]; then die "node ${name} was not created: $(printf '%s' "$created" | head -c 300)"; fi
    SPEEDTEST_AGENT_CONFIG="$dir/config.yaml" SPEEDTEST_AGENT_CREDENTIAL="$dir/credential" \
      speedtest-agent enroll --center "$CENTER" --token "$token" --name "$name" --host "docker-staging"
    psql_run -c "UPDATE schedules SET interval_minutes=${INTERVAL}, offset_minutes=${offset}, revision=revision+1 WHERE node_id='${node_id}'" \
             -c "UPDATE nodes SET config_revision=config_revision+1 WHERE id='${node_id}'"
    printf 'name: %s\nengine: speedtestcli\ninterval: %sm\nexpected_download_mbps: %s\nexpected_upload_mbps: %s\n' "$name" "$INTERVAL" "$EXPECTED_DOWN" "$EXPECTED_UP" > "$dir/config.yaml"
    touch "$dir/enrolled"
  fi
  log "starting agent ${name}"
  SPEEDTEST_AGENT_CONFIG="$dir/config.yaml" SPEEDTEST_AGENT_CREDENTIAL="$dir/credential" SPEEDTEST_AGENT_CENTER="$CENTER" \
    SPEEDTEST_SPOOL_DIR="$dir/spool" speedtest-agent run &
  agent_pids+=("$!")
done

log "running: center ${center_pid}, agents ${agent_pids[*]}"
set +e
wait -n
code=$?
log "a process exited with code ${code}; stopping the container so it restarts"
kill "${agent_pids[@]}" "$center_pid" 2>/dev/null
exit "$code"
