#!/usr/bin/env bash
# Literal expansions below are source-code patterns used to verify the enrollment guard.
# shellcheck disable=SC2016
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
cd "$root"
source deploy/docker/schedule-defaults.sh

unset STAGING_INTERVAL_MINUTES NODE_INTERVAL
[[ "$(staging_interval)" = 60 ]]
[[ "$(node_interval)" = 60 ]]
STAGING_INTERVAL_MINUTES=45
NODE_INTERVAL=5
[[ "$(staging_interval)" = 45 ]]
[[ "$(node_interval)" = 5 ]]

staging_bootstrap="$(sed -n '/if \[ ! -f "\$dir\/enrolled" \]; then/,/^  fi$/p' deploy/docker/entrypoint.sh)"
for statement in 'BEGIN;' 'UPDATE schedules' 'UPDATE engine_schedules' 'engine='"'ookla'" 'UPDATE nodes' 'COMMIT;'; do
  grep -Fq "$statement" <<<"$staging_bootstrap"
done
grep -Fq 'touch "$dir/enrolled"' <<<"$staging_bootstrap"
staging_restart="$(sed -n '/^for spec in \$NODES; do/,$p' deploy/docker/entrypoint.sh | sed -n '/^  fi$/,$p' | tail -n +2)"
if grep -Fq 'UPDATE schedules' <<<"$staging_restart"; then
  printf 'schedule updates must stay inside the first-enrollment block\n' >&2
  exit 1
fi

node_bootstrap="$(sed -n '/if \[ ! -f "\$STATE\/enrolled" \]; then/,/^fi$/p' deploy/docker/node-entrypoint.sh)"
grep -Fq 'interval: %sm' <<<"$node_bootstrap"
grep -Fq '"$NODE_INTERVAL"' <<<"$node_bootstrap"
grep -Fq 'touch "$STATE/enrolled"' <<<"$node_bootstrap"
node_restart="$(sed -n '/^fi$/,$p' deploy/docker/node-entrypoint.sh | tail -n +2)"
if grep -Fq 'interval: %sm' <<<"$node_restart"; then
  printf 'node config writes must stay inside the first-enrollment block\n' >&2
  exit 1
fi
grep -Fq 'set_node_interval "$node_id"' deploy/docker/nodes.sh
grep -Fq 'NODE_INTERVAL="$NODE_INTERVAL"' deploy/docker/nodes.sh
printf 'PASS: staging/node defaults are 60; overrides are honored; schedules synchronize once on first enrollment\n'
