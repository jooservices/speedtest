#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
cd "$root"
source deploy/docker/schedule-defaults.sh

assert_eq() {
  if [[ "$1" != "$2" ]]; then
    printf 'FAIL: %s (got %q, want %q)\n' "$3" "$1" "$2" >&2
    exit 1
  fi
}

unset STAGING_INTERVAL_MINUTES NODE_INTERVAL
assert_eq "$(staging_interval)" 60 'staging default'
assert_eq "$(node_interval)" 60 'extra-node default'
for interval in 5 30 60 1440; do
  valid_schedule_interval "$interval" || { echo "FAIL: rejected valid interval $interval" >&2; exit 1; }
done
for interval in 4 1441 -5 1.5 abc 00000; do
  if valid_schedule_interval "$interval"; then
    echo "FAIL: accepted invalid interval $interval" >&2
    exit 1
  fi
done

mkdir -p .tmp
tmp="$(mktemp -d "$root/.tmp/schedule-defaults.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT
fakebin="$tmp/bin"
mkdir -p "$fakebin"

cat > "$fakebin/curl" <<'PY'
#!/usr/bin/env python3
import hashlib
import json
import os
import sys
from urllib.parse import urlsplit

args = sys.argv[1:]
method = "GET"
url = ""
body = ""
headers = []
dump_headers = None
write_out = None
i = 0
while i < len(args):
    arg = args[i]
    if arg in ("-X", "--request"):
        method = args[i + 1]
        i += 2
    elif arg in ("-H", "--header"):
        headers.append(args[i + 1])
        i += 2
    elif arg in ("-d", "--data", "--data-raw"):
        body = args[i + 1]
        i += 2
    elif arg == "-D":
        dump_headers = args[i + 1]
        i += 2
    elif arg == "-w":
        write_out = args[i + 1]
        i += 2
    elif arg == "-o":
        i += 2
    elif arg.startswith("http://") or arg.startswith("https://"):
        url = arg
        i += 1
    else:
        i += 1

state_path = os.environ["MOCK_HTTP_STATE"]
try:
    with open(state_path, encoding="utf-8") as stream:
        state = json.load(stream)
except FileNotFoundError:
    state = {"requests": [], "idempotency": {}, "creates": 0, "etag": 0, "put_attempts": 0}
header_map = {}
for header in headers:
    if ":" in header:
        key, value = header.split(":", 1)
        header_map[key.lower()] = value.strip()
path = urlsplit(url).path
request = {"method": method, "path": path, "key": header_map.get("idempotency-key", ""), "body": body}
state["requests"].append(request)
code = 200
response = ""

if path == "/healthz":
    response = "ok"
elif path == "/api/auth/me":
    response = json.dumps({"data": {"csrfToken": "test-csrf"}})
elif path == "/api/auth/login":
    response = json.dumps({"data": {"csrfToken": "test-csrf"}})
elif path == "/api/nodes" and method == "POST":
    idem = header_map.get("idempotency-key", "")
    previous = state["idempotency"].get(idem)
    if previous is not None and previous["body"] != body:
        code = 409
        response = json.dumps({"error": "idempotency body changed"})
    elif previous is None:
        payload = json.loads(body)
        name = payload["name"]
        suffix = hashlib.sha256(name.encode()).hexdigest()[:12]
        response = json.dumps({"data": {"token": "test-token-" + suffix,
                                         "node": {"id": "node-" + suffix}}})
        state["idempotency"][idem] = {"body": body, "response": response}
        state["creates"] += 1
    else:
        response = previous["response"]
elif path.startswith("/api/schedules") and method == "GET":
    state["etag"] += 1
    if dump_headers == "-":
        sys.stdout.write(f'HTTP/1.1 200 OK\r\nETag: "schedule-{state["etag"]}"\r\n\r\n')
elif path.startswith("/api/schedules/") and method == "PUT":
    state["put_attempts"] += 1
    codes = os.environ.get("MOCK_PUT_CODES", "").split(",")
    index = state["put_attempts"] - 1 - int(os.environ.get("MOCK_PUT_START", "0"))
    if index < len(codes) and codes[index]:
        code = int(codes[index])
    state.setdefault("schedule_updates", []).append({"if_match": header_map.get("if-match", ""), "body": body, "code": code})
else:
    code = 404
    response = json.dumps({"error": "unexpected request"})

with open(state_path, "w", encoding="utf-8") as stream:
    json.dump(state, stream)
if write_out:
    sys.stdout.write(str(code))
elif response:
    sys.stdout.write(response + "\n")
sys.exit(0)
PY

cat > "$fakebin/docker" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
state="$MOCK_DOCKER_STATE"
mkdir -p "$state"
case "$1 ${2:-}" in
  'ps -a')
    [[ -f "$state/containers" ]] && cat "$state/containers"
    ;;
  'volume create') ;;
  'run -d')
    name=''
    while (($#)); do
      if [[ "$1" == --name ]]; then name="$2"; shift 2; else shift; fi
    done
    printf '%s\n' "$name" >> "$state/containers"
    ;;
  *) ;;
esac
SH

cat > "$fakebin/psql" <<'PY'
#!/usr/bin/env python3
import json
import os
import re
import sys

args = sys.argv[1:]
sql = args[args.index("-c") + 1] if "-c" in args else ""
path = os.environ["MOCK_DB_STATE"]
try:
    with open(path, encoding="utf-8") as stream:
        state = json.load(stream)
except FileNotFoundError:
    state = {"schedules": {}, "engine_schedules": {}, "transactions": []}
if "select 1" in sql.lower():
    pass
elif "UPDATE schedules" in sql:
    if os.environ.get("MOCK_PSQL_FAIL_UPDATES") == "1":
        print("controlled psql transaction failure", file=sys.stderr)
        sys.exit(1)
    legacy = re.search(r"UPDATE schedules SET interval_minutes=(\d+), offset_minutes=(\d+).*?WHERE node_id='([^']+)'", sql, re.S)
    engine = re.search(r"UPDATE engine_schedules SET interval_minutes=(\d+), offset_minutes=(\d+).*?WHERE node_id='([^']+)'", sql, re.S)
    if not (legacy and engine and "BEGIN;" in sql and "COMMIT;" in sql):
        print("schedule changes must be one complete transaction", file=sys.stderr)
        sys.exit(1)
    state["schedules"][legacy.group(3)] = {"interval": int(legacy.group(1)), "offset": int(legacy.group(2))}
    state["engine_schedules"][engine.group(3)] = {"interval": int(engine.group(1)), "offset": int(engine.group(2))}
    state["transactions"].append(sql)
with open(path, "w", encoding="utf-8") as stream:
    json.dump(state, stream)
PY

cat > "$fakebin/speedtest-agent" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$1" >> "$MOCK_AGENT_LOG"
if [[ "$1" == enroll ]]; then
  : > "$SPEEDTEST_AGENT_CREDENTIAL"
fi
SH

cat > "$fakebin/speedtest-center" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
exit 0
SH

cat > "$fakebin/speedtest-migrate" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
exit 0
SH
chmod +x "$fakebin"/*

cat > "$tmp/stg.env" <<'ENV'
STAGING_OWNER_EMAIL=owner@example.invalid
STAGING_OWNER_PASSWORD=test-password
ENV
chmod 600 "$tmp/stg.env"

init_http_state() {
  python3 - "$1" <<'PY'
import json, sys
with open(sys.argv[1], "w", encoding="utf-8") as stream:
    json.dump({"requests": [], "idempotency": {}, "creates": 0, "etag": 0, "put_attempts": 0}, stream)
PY
}
init_http_state "$tmp/http-nodes.json"
python3 - "$tmp/db-invalid.json" <<'PY'
import json, sys
with open(sys.argv[1], "w", encoding="utf-8") as stream:
    json.dump({"schedules": {}, "engine_schedules": {}, "transactions": []}, stream)
PY

run_nodes() {
  local name="$1"
  shift
  env PATH="$fakebin:$PATH" MOCK_HTTP_STATE="$tmp/http-nodes.json" \
    MOCK_DOCKER_STATE="$tmp/docker" STG_ENV_FILE="$tmp/stg.env" NODE_NETWORK=host \
    CENTER_URL=http://127.0.0.1:18090 "$@" bash deploy/docker/nodes.sh up "$name"
}

if run_nodes dock-invalid NODE_INTERVAL=4 >"$tmp/invalid-node.log" 2>&1; then
  echo 'FAIL: nodes.sh accepted an interval below five before creation' >&2
  exit 1
fi
python3 - "$tmp/http-nodes.json" <<'PY'
import json, sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
assert not any(r["path"] == "/api/nodes" for r in state["requests"]), "invalid interval reached node creation"
PY

if run_nodes dock-retry NODE_INTERVAL=45 MOCK_PUT_CODES=500 >"$tmp/node-failure.log" 2>&1; then
  echo 'FAIL: nodes.sh treated a failed schedule update as success' >&2
  exit 1
fi
python3 - "$tmp/http-nodes.json" "$tmp/docker" <<'PY'
import json, os, sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
assert state["creates"] == 1, state
assert len([r for r in state["requests"] if r["path"] == "/api/nodes"]) == 1, state
assert not os.path.exists(os.path.join(sys.argv[2], "containers")), "container started before schedule succeeded"
PY

run_nodes dock-retry NODE_INTERVAL=45 >"$tmp/node-retry.log" 2>&1
run_nodes dock-stale NODE_INTERVAL=5 MOCK_PUT_CODES=412,200 MOCK_PUT_START=2 >"$tmp/node-stale.log" 2>&1
python3 - "$tmp/http-nodes.json" "$tmp/docker" <<'PY'
import json, os, sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
node_requests = [r for r in state["requests"] if r["path"] == "/api/nodes"]
assert state["creates"] == 2, state
assert len(node_requests) == 3, node_requests
assert node_requests[0]["key"] == node_requests[1]["key"], "retry did not reuse create idempotency key"
assert node_requests[0]["body"] == node_requests[1]["body"], "retry changed create request"
stale_updates = [u for u in state["schedule_updates"] if u["body"] and '"interval":5' in u["body"]]
assert [u["code"] for u in stale_updates] == [412, 200], stale_updates
assert stale_updates[0]["if_match"] != stale_updates[1]["if_match"], stale_updates
containers = open(os.path.join(sys.argv[2], "containers"), encoding="utf-8").read().splitlines()
assert containers == ["speedtest-node-dock-retry", "speedtest-node-dock-stale"], containers
PY

run_stage() {
  local state="$1" dbstate="$2" httpstate="$3" agentlog="$4" logfile="$5"
  shift 5
  env PATH="$fakebin:$PATH" MOCK_HTTP_STATE="$httpstate" MOCK_DB_STATE="$dbstate" \
    MOCK_AGENT_LOG="$agentlog" SPEEDTEST_STATE_DIR="$state" \
    SPEEDTEST_DB_HOST=127.0.0.1 SPEEDTEST_DB_PORT=5432 SPEEDTEST_DB_NAME=speedtest_wt85 \
    SPEEDTEST_DB_USER=test SPEEDTEST_DB_PASSWORD=test-only APP_KEY=test-only \
    STAGING_OWNER_EMAIL=owner@example.invalid STAGING_OWNER_PASSWORD=test-only \
    "$@" bash deploy/docker/entrypoint.sh >"$logfile" 2>&1
}

invalid_state="$tmp/stage-invalid"
init_http_state "$tmp/http-invalid.json"
python3 - "$tmp/db-invalid.json" <<'PY'
import json, sys
with open(sys.argv[1], "w", encoding="utf-8") as stream:
    json.dump({"schedules": {}, "engine_schedules": {}, "transactions": []}, stream)
PY
if run_stage "$invalid_state" "$tmp/db-invalid.json" "$tmp/http-invalid.json" "$tmp/agent-invalid.log" "$tmp/stage-invalid.log" STAGING_INTERVAL_MINUTES=4; then
  echo 'FAIL: staging accepted an invalid interval' >&2
  exit 1
fi
python3 - "$tmp/http-invalid.json" "$tmp/db-invalid.json" <<'PY'
import json, os, sys
http = json.load(open(sys.argv[1], encoding="utf-8"))
assert not any(r["path"] == "/api/nodes" for r in http["requests"]), http
db = json.load(open(sys.argv[2], encoding="utf-8"))
assert not db["transactions"], db
PY

stage_state="$tmp/stage-retry"
init_http_state "$tmp/http-retry.json"
mkdir -p "$stage_state"
: > "$stage_state/owner-ready"
if run_stage "$stage_state" "$tmp/db-retry.json" "$tmp/http-retry.json" "$tmp/agent-retry.log" "$tmp/stage-failure.log" MOCK_PSQL_FAIL_UPDATES=1 STAGING_NODES=VM1:7; then
  echo 'FAIL: staging ignored an atomic schedule update failure' >&2
  exit 1
fi
python3 - "$tmp/db-retry.json" "$tmp/agent-retry.log" "$stage_state" <<'PY'
import json, os, sys
db = json.load(open(sys.argv[1], encoding="utf-8"))
assert not db["schedules"] and not db["engine_schedules"] and not db["transactions"], db
assert not os.path.exists(sys.argv[2]), "agent enrolled before schedule transaction succeeded"
assert not os.path.exists(os.path.join(sys.argv[3], "agents", "VM1", "enrolled")), "failed bootstrap was marked enrolled"
PY
run_stage "$stage_state" "$tmp/db-retry.json" "$tmp/http-retry.json" "$tmp/agent-retry.log" "$tmp/stage-retry.log" STAGING_NODES=VM1:7
python3 - "$tmp/db-retry.json" "$tmp/http-retry.json" "$tmp/agent-retry.log" "$stage_state" <<'PY'
import json, os, sys
db = json.load(open(sys.argv[1], encoding="utf-8"))
http = json.load(open(sys.argv[2], encoding="utf-8"))
requests = [r for r in http["requests"] if r["path"] == "/api/nodes"]
assert len(db["schedules"]) == 1, db
assert next(iter(db["schedules"].values())) == {"interval": 60, "offset": 7}, db
assert db["engine_schedules"] == db["schedules"], db
assert len(db["transactions"]) == 1, db
assert len(requests) == 2 and requests[0]["key"] == requests[1]["key"], requests
assert open(sys.argv[3], encoding="utf-8").read().splitlines() == ["enroll", "run"]
config = open(os.path.join(sys.argv[4], "agents", "VM1", "config.yaml"), encoding="utf-8").read()
assert "interval: 60m" in config, config
PY

run_stage "$stage_state" "$tmp/db-retry.json" "$tmp/http-retry.json" "$tmp/agent-retry.log" "$tmp/stage-restart.log" STAGING_INTERVAL_MINUTES=45 STAGING_NODES=VM1:38
python3 - "$tmp/db-retry.json" "$tmp/http-retry.json" "$tmp/agent-retry.log" "$stage_state" <<'PY'
import json, os, sys
db = json.load(open(sys.argv[1], encoding="utf-8"))
http = json.load(open(sys.argv[2], encoding="utf-8"))
assert len(db["transactions"]) == 1, db
assert next(iter(db["schedules"].values())) == {"interval": 60, "offset": 7}, db
assert len([r for r in http["requests"] if r["path"] == "/api/nodes"]) == 2, http
assert open(sys.argv[3], encoding="utf-8").read().splitlines() == ["enroll", "run", "run"]
config = open(os.path.join(sys.argv[4], "agents", "VM1", "config.yaml"), encoding="utf-8").read()
assert "interval: 60m" in config and "interval: 45m" not in config, config
PY

init_http_state "$tmp/http-override.json"
run_stage "$tmp/stage-override" "$tmp/db-override.json" "$tmp/http-override.json" "$tmp/agent-override.log" "$tmp/stage-override.log" STAGING_INTERVAL_MINUTES=45 STAGING_NODES=VM2:2
python3 - "$tmp/db-override.json" <<'PY'
import json, sys
db = json.load(open(sys.argv[1], encoding="utf-8"))
assert next(iter(db["schedules"].values())) == {"interval": 45, "offset": 2}, db
assert db["engine_schedules"] == db["schedules"], db
assert len(db["transactions"]) == 1, db
PY

node_state="$tmp/node-entry"
mkdir -p "$node_state"
if env PATH="$fakebin:$PATH" MOCK_AGENT_LOG="$tmp/node-agent.log" SPEEDTEST_STATE_DIR="$node_state" NODE_INTERVAL=4 \
  NODE_NAME=VM3 CENTER_URL=http://127.0.0.1:18090 bash deploy/docker/node-entrypoint.sh >"$tmp/node-entry-invalid.log" 2>&1; then
  echo 'FAIL: node-entrypoint accepted an invalid interval' >&2
  exit 1
fi
env PATH="$fakebin:$PATH" MOCK_AGENT_LOG="$tmp/node-agent.log" SPEEDTEST_STATE_DIR="$node_state" NODE_INTERVAL=5 \
  NODE_NAME=VM3 CENTER_URL=http://127.0.0.1:18090 ENROLL_TOKEN=test-only bash deploy/docker/node-entrypoint.sh >"$tmp/node-entry-first.log" 2>&1
env PATH="$fakebin:$PATH" MOCK_AGENT_LOG="$tmp/node-agent.log" SPEEDTEST_STATE_DIR="$node_state" NODE_INTERVAL=30 \
  NODE_NAME=VM3 CENTER_URL=http://127.0.0.1:18090 bash deploy/docker/node-entrypoint.sh >"$tmp/node-entry-restart.log" 2>&1
assert_eq "$(grep -c '^enroll$' "$tmp/node-agent.log")" 1 'extra-node enrollment count across restart'
assert_eq "$(grep -c '^run$' "$tmp/node-agent.log")" 2 'extra-node run count'
grep -Fq 'interval: 5m' "$node_state/config.yaml" || { echo 'FAIL: extra-node interval override not persisted' >&2; exit 1; }
! grep -Fq 'interval: 30m' "$node_state/config.yaml" || { echo 'FAIL: restart rewrote extra-node interval' >&2; exit 1; }

echo 'PASS: staging and extra-node defaults, overrides, validation, transaction failures, retry idempotency, and restart preservation'
