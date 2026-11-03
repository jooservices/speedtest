#!/usr/bin/env bash
# Restore a backup made by backup.sh into the database named by SPEEDTEST_DB_NAME.
# Destructive: existing objects are dropped. Stop the center first, and confirm
# by passing the database name as the second argument.
# Usage: deploy/restore.sh <dump-file> <database-name>
set -euo pipefail
dump="${1:?dump file}"; confirm="${2:?repeat the target database name to confirm}"
: "${SPEEDTEST_DB_NAME:?set SPEEDTEST_DB_NAME}"
[ "$confirm" = "$SPEEDTEST_DB_NAME" ] || { echo "confirmation does not match SPEEDTEST_DB_NAME" >&2; exit 2; }
export PGPASSWORD="${SPEEDTEST_DB_PASSWORD:-}"
start=$(date +%s)
pg_restore -h "${SPEEDTEST_DB_HOST:-127.0.0.1}" -p "${SPEEDTEST_DB_PORT:-5432}" -U "${SPEEDTEST_DB_USER:-speedtest}" \
  -d "$SPEEDTEST_DB_NAME" --clean --if-exists --no-owner --single-transaction "$dump"
echo "restore ok into $SPEEDTEST_DB_NAME in $(( $(date +%s) - start ))s; start the center and check /readyz"
