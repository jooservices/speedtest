#!/usr/bin/env bash
# Shared schedule defaults for the staging center and extra-node containers.
staging_interval() { printf '%s' "${STAGING_INTERVAL_MINUTES:-60}"; }
node_interval() { printf '%s' "${NODE_INTERVAL:-60}"; }

valid_schedule_interval() {
  local value="$1"
  [[ "$value" =~ ^[0-9]+$ && ${#value} -le 4 ]] || return 1
  local minutes=$((10#$value))
  (( minutes >= 5 && minutes <= 1440 ))
}

require_schedule_interval() {
  local label="$1" value="$2"
  if ! valid_schedule_interval "$value"; then
    printf '%s must be an integer from 5 through 1440 minutes\n' "$label" >&2
    return 1
  fi
}
