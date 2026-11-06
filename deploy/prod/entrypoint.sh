#!/usr/bin/env bash
# Production entrypoint: apply pending migrations, then run the center in the foreground.
# The settings come from the container environment (an --env-file kept 0600 on the host).
set -euo pipefail
: "${SPEEDTEST_DB_HOST:?}" "${SPEEDTEST_DB_PASSWORD:?}" "${APP_KEY:?}"
export APP_ENV=production
export SPEEDTEST_HTTP_ADDR="${SPEEDTEST_HTTP_BIND:-0.0.0.0}:${SPEEDTEST_PORT:-18090}"
# The migrate tool wants a .env next to it; the real settings are already in the environment.
: > /var/lib/speedtest/.env
(cd /var/lib/speedtest && speedtest-migrate up)
exec speedtest-center
