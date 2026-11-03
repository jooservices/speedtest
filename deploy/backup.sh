#!/usr/bin/env bash
# Logical backup of the Speedtest database (custom-format pg_dump).
# Reads SPEEDTEST_DB_* from the environment (export them or run under systemd
# EnvironmentFile); the password is passed through PGPASSWORD, never argv.
# Usage: deploy/backup.sh [output-dir]   (default ./backups)
# Keeps the newest $KEEP (default 14) dumps.
set -euo pipefail
dir="${1:-./backups}"; keep="${KEEP:-14}"
: "${SPEEDTEST_DB_NAME:?set SPEEDTEST_DB_NAME}"
export PGPASSWORD="${SPEEDTEST_DB_PASSWORD:-}"
umask 077; mkdir -p "$dir"
out="$dir/${SPEEDTEST_DB_NAME}-$(date -u +%Y%m%dT%H%M%SZ).dump"
start=$(date +%s)
pg_dump -h "${SPEEDTEST_DB_HOST:-127.0.0.1}" -p "${SPEEDTEST_DB_PORT:-5432}" -U "${SPEEDTEST_DB_USER:-speedtest}" \
  --format=custom --no-owner --file "$out.part" "$SPEEDTEST_DB_NAME"
pg_restore --list "$out.part" >/dev/null   # the archive must be readable
mv "$out.part" "$out"
ls -1t "$dir"/"${SPEEDTEST_DB_NAME}"-*.dump 2>/dev/null | tail -n +"$((keep + 1))" | xargs -r rm --
echo "backup ok: $out ($(stat -c %s "$out") bytes, $(( $(date +%s) - start ))s)"
