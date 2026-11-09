#!/usr/bin/env bash
# Entrypoint of an extra node container ("VM"): enrols once with the token it was given, then runs one agent.
# It knows only its own name, the center address and its expected speeds; nothing about other nodes.
set -euo pipefail
STATE=/var/lib/speedtest
# shellcheck source=deploy/docker/schedule-defaults.sh
source "$(dirname "$0")/schedule-defaults.sh"
NODE_INTERVAL="$(node_interval)"
: "${NODE_NAME:?}" "${CENTER_URL:?}"
export SPEEDTEST_AGENT_CONFIG="$STATE/config.yaml" SPEEDTEST_AGENT_CREDENTIAL="$STATE/credential" SPEEDTEST_AGENT_CENTER="$CENTER_URL" SPEEDTEST_SPOOL_DIR="$STATE/spool"
log() { printf '%s node %s: %s\n' "$(date -u +%FT%TZ)" "$NODE_NAME" "$*"; }
if [ ! -f "$STATE/enrolled" ]; then
  : "${ENROLL_TOKEN:?}"
  log "enrolling with ${CENTER_URL}"
  speedtest-agent enroll --center "$CENTER_URL" --token "$ENROLL_TOKEN" --name "$NODE_NAME" --host "${NODE_HOST:-$(hostname)}"
  printf 'name: %s\nengine: %s\ninterval: %sm\nexpected_download_mbps: %s\nexpected_upload_mbps: %s\n' "$NODE_NAME" "${NODE_ENGINE:-ookla}" "$NODE_INTERVAL" "${EXPECTED_DOWN:-500}" "${EXPECTED_UP:-500}" > "$SPEEDTEST_AGENT_CONFIG"
  touch "$STATE/enrolled"
fi
log "starting the agent"
exec speedtest-agent run
