#!/usr/bin/env sh
# Uptime and host probe: one JSON line per run on stdout, for Vector/journald to ship and for alert rules to read.
#
#   probe.sh                       one run
#   PROBE_INTERVAL=60 probe.sh -loop   keep running every PROBE_INTERVAL seconds (container use)
#
# Settings: PROBE_URL (default http://127.0.0.1:18090), PROBE_DATA_DIR (disk to watch, default /), PROBE_TIMEOUT (seconds, 5).
# What it reports: "readyz" HTTP status of /readyz (0 = no answer), "readyz_ms", "cert_days" (days until the TLS
# certificate of PROBE_URL expires, -1 when the URL is not https or unreadable), "disk_free_pct" for PROBE_DATA_DIR.
set -u
URL="${PROBE_URL:-http://127.0.0.1:18090}"
DIR="${PROBE_DATA_DIR:-/}"
TIMEOUT="${PROBE_TIMEOUT:-5}"

probe_once() {
  start=$(date +%s%N 2>/dev/null || echo 0)
  code=$(curl -s -o /dev/null -m "$TIMEOUT" -w '%{http_code}' "$URL/readyz" 2>/dev/null || true)
  case "$code" in ""|000) code=0;; esac
  end=$(date +%s%N 2>/dev/null || echo 0)
  ms=$(( (end - start) / 1000000 ))
  cert=-1
  case "$URL" in
    https://*)
      hostport=${URL#https://}; hostport=${hostport%%/*}
      host=${hostport%%:*}; port=443; [ "$hostport" != "$host" ] && port=${hostport##*:}
      expiry=$(echo | openssl s_client -servername "$host" -connect "$host:$port" 2>/dev/null | openssl x509 -noout -enddate 2>/dev/null | cut -d= -f2)
      if [ -n "$expiry" ]; then
        exp=$(date -d "$expiry" +%s 2>/dev/null || echo 0)
        [ "$exp" -gt 0 ] && cert=$(( (exp - $(date +%s)) / 86400 ))
      fi;;
  esac
  free=$(df -P "$DIR" 2>/dev/null | awk 'NR==2 { gsub("%","",$5); print 100-$5 }')
  [ -n "$free" ] || free=-1
  printf '{"time":"%s","level":"INFO","msg":"probe","component":"probe","target":"%s","readyz":%s,"readyz_ms":%s,"cert_days":%s,"disk_free_pct":%s}\n' \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$URL" "$code" "$ms" "$cert" "$free"
}

if [ "${1:-}" = "-loop" ]; then
  while :; do probe_once; sleep "${PROBE_INTERVAL:-60}"; done
else
  probe_once
fi
