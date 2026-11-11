#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."

tmp_root="$PWD/.tmp"
mkdir -p "$tmp_root"
tmp="$(mktemp -d "$tmp_root/joost-85.XXXXXX")"
state="$tmp/state"
cleanup() {
  mkdir -p "$state"
  touch "$state/release-enrollment"
  sleep 0.05
  rm -rf "$tmp"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
assert_eq() {
  local expected="$1" actual="$2" message="$3"
  [[ "$actual" == "$expected" ]] || fail "$message: expected '$expected', got '$actual'"
}

check_default_and_override() {
  local name="$1" file="$2" expression expected
  expression="$(grep -oE "\\$\{${name}:-[0-9]+\}" "$file" | head -n 1 || true)"
  expected="\${${name}:-60}"
  assert_eq "$expected" "$expression" "$file default expression"
  actual="$(env -u "$name" bash -c "printf '%s' \"$expression\"")"
  assert_eq 60 "$actual" "$file default value"
  actual="$(env "$name=37" bash -c "printf '%s' \"$expression\"")"
  assert_eq 37 "$actual" "$file override"
}

check_default_and_override STAGING_INTERVAL_MINUTES deploy/docker/entrypoint.sh
check_default_and_override NODE_INTERVAL deploy/docker/node-entrypoint.sh
check_default_and_override NODE_INTERVAL deploy/docker/nodes.sh

mkdir -p "$tmp/bin" "$state"
cat > "$tmp/stg.env" <<'EOF'
STAGING_OWNER_EMAIL=test@example.invalid
STAGING_OWNER_PASSWORD=test-only
EOF
chmod 0600 "$tmp/stg.env"

cat > "$tmp/bin/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail
command="${1:-}"
shift || true
case "$command" in
  ps|volume) exit 0 ;;
  run)
    name=""
    while (($#)); do
      case "$1" in
        --name) name="$2"; shift 2 ;;
        -e)
          [[ "$2" != NODE_INTERVAL=* ]] || printf '%s\n' "$2" >> "$FAKE_STATE/intervals"
          shift 2
          ;;
        *) shift ;;
      esac
    done
    printf '%s\n' "$name" >> "$FAKE_STATE/runs"
    mkdir -p "$FAKE_STATE/containers/$name"
    (
      while [[ ! -e "$FAKE_STATE/release-enrollment" ]]; do sleep 0.02; done
      touch "$FAKE_STATE/containers/$name/enrolled"
    ) >/dev/null 2>&1 </dev/null &
    ;;
  inspect) printf 'true\n' ;;
  cp)
    container="${1%%:*}"
    directory="$FAKE_STATE/containers/$container"
    [[ -f "$directory/enrolled" ]] || exit 1
    (cd "$directory" && tar -cf - enrolled)
    ;;
  *) printf 'unexpected fake docker command: %s\n' "$command" >&2; exit 1 ;;
esac
DOCKER

cat > "$tmp/bin/curl" <<'CURL'
#!/usr/bin/env bash
set -euo pipefail
method=GET
body=""
want_status=0
url=""
while (($#)); do
  case "$1" in
    -X) method="$2"; shift 2 ;;
    -d) body="$2"; shift 2 ;;
    -w) [[ "$2" != '%{http_code}' ]] || want_status=1; shift 2 ;;
    -D|-o|-H|-b|-c) shift 2 ;;
    -sS) shift ;;
    *) url="$1"; shift ;;
  esac
done
case "$url" in
  */api/auth/me|*/api/auth/login)
    printf '{"success":true,"data":{"csrfToken":"test-csrf"}}'
    ;;
  */api/nodes\?size=100)
    printf '{"success":true,"data":[]}'
    ;;
  */api/nodes)
    name="$(jq -r '.name' <<<"$body")"
    printf '{"success":true,"data":{"token":"test-token","node":{"id":"id-%s"}}}' "$name"
    ;;
  */api/schedules/stagger)
    touch "$FAKE_STATE/staggered"
    [[ "$want_status" == 0 ]] || printf '200'
    ;;
  */api/schedules|*/api/schedules/*)
    if [[ "$want_status" == 1 ]]; then
      printf '200'
    elif [[ "$method" == GET ]]; then
      printf 'ETag: "test-schedules"\r\n'
    else
      printf '{"success":true}'
    fi
    ;;
  *) printf 'unexpected fake URL: %s\n' "$url" >&2; exit 1 ;;
esac
CURL
chmod 0700 "$tmp/bin/docker" "$tmp/bin/curl"

FAKE_STATE="$state" PATH="$tmp/bin:$PATH" STG_ENV_FILE="$tmp/stg.env" \
  NODE_NETWORK=host NODE_INTERVAL=37 bash deploy/docker/nodes.sh up delayed-a delayed-b

grep -Fxq speedtest-node-delayed-a "$state/runs" || fail 'first node was not started'
grep -Fxq speedtest-node-delayed-b "$state/runs" || fail 'second node was not started'
[[ -e "$state/staggered" ]] || fail 'multi-node startup skipped the final stagger request'
assert_eq 2 "$(grep -c '^NODE_INTERVAL=37$' "$state/intervals")" 'explicit NODE_INTERVAL passed to containers'
[[ ! -e "$state/containers/speedtest-node-delayed-a/enrolled" && ! -e "$state/containers/speedtest-node-delayed-b/enrolled" ]] ||
  fail 'fake enrollment completed before the delayed-enrollment check'

touch "$state/release-enrollment"
for _ in {1..100}; do
  [[ -e "$state/containers/speedtest-node-delayed-a/enrolled" && -e "$state/containers/speedtest-node-delayed-b/enrolled" ]] && break
  sleep 0.02
done
[[ -e "$state/containers/speedtest-node-delayed-a/enrolled" && -e "$state/containers/speedtest-node-delayed-b/enrolled" ]] ||
  fail 'delayed enrollment did not complete for both nodes'

printf 'PASS: 60-minute staging defaults, explicit overrides, delayed enrollment, and multi-node startup\n'
