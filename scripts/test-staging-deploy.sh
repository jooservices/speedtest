#!/usr/bin/env bash
# Tests deploy/docker/stg-deploy.sh with fakes for docker, curl, node and cloudflared: no container, network or
# real measurement is needed. The password may appear only in the report file, never on the screen or in a log.
set -euo pipefail

here="$(cd "$(dirname "$0")/.." && pwd)"
mkdir -p "$here/.tmp"
work="$(mktemp -d "$here/.tmp/staging-deploy-test.XXXXXX")"
trap 'rm -rf "$work"' EXIT

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

repo="$work/repo"
mkdir -p "$repo/deploy/docker" "$repo/scripts" "$work/bin"
cp "$here/deploy/docker/stg-deploy.sh" "$repo/deploy/docker/stg-deploy.sh"
git -C "$repo" init -q && git -C "$repo" -c user.name=t -c user.email=t@t commit -q --allow-empty -m init
# shellcheck disable=SC2016 # the fakes' bodies must expand when the fakes run
{
	printf '#!/usr/bin/env bash\necho "stg.sh $*" >> "$FAKE_LOG"\n[ "${FAKE_REDEPLOY_FAILS:-0}" = 1 ] && exit 1 || exit 0\n' >"$repo/deploy/docker/stg.sh"
	printf '#!/usr/bin/env bash\ncat >> "$FAKE_LOG.stdin"\necho "docker $*" >> "$FAKE_LOG"\n' >"$work/bin/docker"
	printf '#!/usr/bin/env bash\nexit 0\n' >"$work/bin/curl"
	printf '#!/usr/bin/env bash\necho "node smoke" >> "$FAKE_LOG"\n[ "${FAKE_SMOKE_FAILS:-0}" = 1 ] && { echo "smoke: FAILED: x" >&2; exit 1; } || exit 0\n' >"$work/bin/node"
	printf '#!/usr/bin/env bash\necho "INF https://fake-tunnel-name.trycloudflare.com"\nsleep 1\n' >"$work/bin/cloudflared"
} 
touch "$repo/scripts/staging-smoke.mjs"
chmod +x "$repo/deploy/docker/"*.sh "$work/bin/"*
printf 'STAGING_OWNER_EMAIL=owner@example.test\nSTAGING_OWNER_PASSWORD=pw-from-env-file\nSTAGING_PORT=18099\n' >"$repo/deploy/docker/stg.env"

run() { # name, env assignments...
	local name="$1"; shift
	export FAKE_LOG="$work/$name.log"; : >"$FAKE_LOG"
	env -i PATH="$work/bin:$PATH" HOME="$work" FAKE_LOG="$FAKE_LOG" STG_REPORT="$work/$name.json" "$@" bash "$repo/deploy/docker/stg-deploy.sh" >"$work/$name.out" 2>&1
}

# success: the report holds the password, the screen and the log do not
run ok STG_TUNNEL_MINUTES=1 || fail "a healthy deploy must exit 0: $(cat "$work/ok.out")"
[ "$(jq -r .ok "$work/ok.json")" = true ] || fail "the report must say ok"
[ "$(jq -r .public_url "$work/ok.json")" = "https://fake-tunnel-name.trycloudflare.com" ] || fail "the report must carry the tunnel URL"
[ "$(jq -r .local_url "$work/ok.json")" = "http://127.0.0.1:18099" ] || fail "the local URL must use the staging port"
password="$(jq -r .password "$work/ok.json")"
[ "$password" = "pw-from-env-file" ] || fail "the report must carry the password of the env file"
if grep -q 'reset-owner-password' "$work/ok.log"; then fail "the password must not be changed per deploy"; fi
[ "$(stat -c %a "$work/ok.json")" = 600 ] || fail "the report must be mode 0600"
if grep -qF -- "$password" "$work/ok.out" "$work/ok.log"; then fail "the password must never be printed or logged"; fi
grep -q 'stg.sh redeploy' "$work/ok.log" || fail "the container must be redeployed"

# smoke failure: non-zero, the report says why and holds no password
run smoke FAKE_SMOKE_FAILS=1 && fail "a failed smoke test must fail the deploy"
[ "$(jq -r .ok "$work/smoke.json")" = false ] || fail "the report must say not ok"
[ "$(jq -r .password "$work/smoke.json")" = "" ] || fail "a failed deploy must not publish a password"
grep -q 'smoke test failed' "$work/smoke.json" || fail "the report must say the smoke test failed"

# redeploy failure: the smoke test must not run
run redeploy FAKE_REDEPLOY_FAILS=1 && fail "a failed redeploy must fail the deploy"
if grep -q 'node smoke' "$work/redeploy.log"; then fail "no smoke test after a failed redeploy"; fi

# missing env file
rm "$repo/deploy/docker/stg.env"
run noenv && fail "a missing env file must fail"
grep -q 'missing' "$work/noenv.out" || fail "the failure must name the missing env file"
printf 'ok: staging deploy tests passed\n'
