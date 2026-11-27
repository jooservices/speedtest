#!/usr/bin/env bash
# Deploy the current checkout to the staging container and prove that the project runs.
# Run from anywhere; it needs Docker, Go (make release), Node and the staging env file (see stg.sh).
#
#   1. stg.sh redeploy             build the binaries and the image, replace the container
#   2. wait for /readyz            the center answers
#   3. owner password              the one in the env file (the container's entrypoint signs in with it at every start)
#   4. scripts/staging-smoke.mjs   sign in and complete one measurement (the proof)
#   5. report file                 JSON for whoever announces the deploy; mode 0600, the only place the password is copied to
#
# Nothing secret is printed. A new password is not made per deploy: the container's entrypoint signs in with the one in
# the env file at every start, so changing it in the database alone would put the container in a restart loop.
# Optional: STG_TUNNEL_MINUTES=N opens a temporary Cloudflare quick tunnel for N minutes
# and puts its URL in the report (a test aid until the permanent staging domain exists).
set -euo pipefail
cd "$(dirname "$0")/../.."
ENV_FILE="${STG_ENV_FILE:-deploy/docker/stg.env}"
REPORT="${STG_REPORT:-${XDG_STATE_HOME:-$HOME/.local/state}/speedtest-staging/last-deploy.json}"
TIMEOUT_S="${STG_SMOKE_TIMEOUT_S:-300}"
TUNNEL_MIN="${STG_TUNNEL_MINUTES:-0}"

log() { printf '%s stg-deploy: %s\n' "$(date -u +%FT%TZ)" "$*"; }
[ -f "$ENV_FILE" ] || { log "missing $ENV_FILE (see docs/deployment.md)"; exit 1; }
COMMIT="$(git rev-parse HEAD)"
PORT="$(sed -n 's/^STAGING_PORT=//p' "$ENV_FILE" | tail -1)"; PORT="${PORT:-18090}"
EMAIL="$(sed -n 's/^STAGING_OWNER_EMAIL=//p' "$ENV_FILE" | tail -1)"
PASSWORD="$(sed -n 's/^STAGING_OWNER_PASSWORD=//p' "$ENV_FILE" | tail -1)"
if [ -z "$EMAIL" ] || [ -z "$PASSWORD" ]; then log "STAGING_OWNER_EMAIL and STAGING_OWNER_PASSWORD must be set in $ENV_FILE"; exit 1; fi
URL="http://127.0.0.1:${PORT}"
mkdir -p "$(dirname "$REPORT")"

write_report() { # ok reason password public_url
	umask 077
	jq -n --argjson ok "$1" --arg reason "$2" --arg password "$3" --arg public "$4" --arg commit "$COMMIT" \
		--arg local "$URL" --arg email "$EMAIL" --arg at "$(date -u +%FT%TZ)" \
		'{ok:$ok, reason:$reason, commit:$commit, local_url:$local, public_url:$public, email:$email, password:$password, finished_at:$at}' >"$REPORT"
}
fail() { log "FAILED: $1"; write_report false "$1" "" ""; exit 1; }

log "redeploy of ${COMMIT:0:12}"
deploy/docker/stg.sh redeploy >/dev/null || fail "stg.sh redeploy failed"
for _ in $(seq 1 60); do curl -fs -o /dev/null "$URL/readyz" && break; sleep 2; done
curl -fs -o /dev/null "$URL/readyz" || fail "the center did not answer /readyz"

log "smoke test (timeout ${TIMEOUT_S} s)"
SMOKE_URL="$URL" SMOKE_EMAIL="$EMAIL" SMOKE_PASSWORD="$PASSWORD" SMOKE_TIMEOUT_S="$TIMEOUT_S" node scripts/staging-smoke.mjs \
	|| fail "the smoke test failed"

PUBLIC=""
if [ "$TUNNEL_MIN" -gt 0 ]; then
	TLOG="$(mktemp)"
	nohup timeout "${TUNNEL_MIN}m" cloudflared tunnel --no-autoupdate --url "$URL" >"$TLOG" 2>&1 &
	for _ in $(seq 1 30); do PUBLIC="$(grep -o 'https://[a-z0-9-]*\.trycloudflare\.com' "$TLOG" | head -1 || true)"; [ -n "$PUBLIC" ] && break; sleep 1; done
	[ -n "$PUBLIC" ] || fail "the temporary tunnel did not start"
fi
write_report true "" "$PASSWORD" "$PUBLIC"
log "deployed ${COMMIT:0:12}; report written to $REPORT"
