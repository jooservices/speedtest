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
    state = {"requests": [], "idempotency": {}, "creates": 0, "etag": 0, "put_attempts": 0, "nodes": [], "schedules": {}}
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
elif path == "/api/nodes" and method == "GET":
    response = json.dumps({"success": True, "data": [node for node in state["nodes"] if not node.get("deleted")]})
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
        state["creates"] += 1
        node_id = "node-" + suffix + "-" + str(state["creates"])
        response = json.dumps({"data": {"token": "test-token-" + suffix + "-" + str(state["creates"]),
                                         "node": {"id": node_id}}})
        state["idempotency"][idem] = {"body": body, "response": response}
        state["nodes"].append({"id": node_id, "name": name})
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
    if code == 200:
        state["schedules"][path.rsplit("/", 1)[-1]] = json.loads(body)
elif path.startswith("/api/nodes/") and method == "GET":
    state["etag"] += 1
    if dump_headers == "-":
        sys.stdout.write(f'HTTP/1.1 200 OK\r\nETag: "node-{state["etag"]}"\r\n\r\n')
elif path.startswith("/api/nodes/") and method == "DELETE":
    node_id = path.rsplit("/", 1)[-1]
    for node in state["nodes"]:
        if node["id"] == node_id:
            node["deleted"] = True
    code = 200
else:
    code = 404
    response = json.dumps({"error": "unexpected request"})

with open(state_path, "w", encoding="utf-8") as stream:
    json.dump(state, stream)
drop_post_response = method == "POST" and path == "/api/nodes" and os.environ.get("MOCK_POST_DROP") == "1" and not state.get("post_response_dropped", False)
if drop_post_response:
    state["post_response_dropped"] = True
    with open(state_path, "w", encoding="utf-8") as stream:
        json.dump(state, stream)
    sys.exit(7)
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
  'volume create')
    volume="$3"
    touch "$state/volumes"
    grep -qx "$volume" "$state/volumes" || printf '%s\n' "$volume" >> "$state/volumes"
    mkdir -p "$state/volume-data/$volume"
    printf 'create %s\n' "$volume" >> "$state/volume-events"
    ;;
  'volume rm')
    volume="$3"
    if [[ -f "$state/volumes" ]]; then
      awk -v name="$volume" '$0 != name' "$state/volumes" > "$state/volumes.tmp"
      mv "$state/volumes.tmp" "$state/volumes"
    fi
    rm -rf "$state/volume-data/$volume"
    printf 'remove %s\n' "$volume" >> "$state/volume-events"
    ;;
  'network rm') ;;
  'rm -f')
    container="$3"
    if [[ -f "$state/containers" ]]; then
      awk -v name="$container" '$0 != name' "$state/containers" > "$state/containers.tmp"
      mv "$state/containers.tmp" "$state/containers"
    fi
    rm -f "$state/running.$container"
    ;;
  'run -d')
    name='' token='' volume=''
    while (($#)); do
      case "$1" in
        --name) name="$2"; shift 2 ;;
        -e) [[ "$2" == ENROLL_TOKEN=* ]] && token="${2#ENROLL_TOKEN=}"; shift 2 ;;
        -v) volume="${2%%:*}"; shift 2 ;;
        *) shift ;;
      esac
    done
    printf '%s\n' "$name" >> "$state/containers"
    printf '%s\t%s\n' "$name" "$token" >> "$state/run-tokens"
    mkdir -p "$state/volume-data/$volume"
    if [[ "${MOCK_DOCKER_ENROLL_ON_RUN:-1}" == 1 ]]; then
      : > "$state/volume-data/$volume/enrolled"
      printf 'true\n' > "$state/running.$name"
    else
      printf 'false\n' > "$state/running.$name"
    fi
    ;;
  'inspect '*)
    container="${@: -1}"
    cat "$state/running.$container" 2>/dev/null || printf 'false\n'
    ;;
  'cp '*)
    source_path="$2"
    container="${source_path%%:*}"
    volume="${container#speedtest-node-}"
    if [[ -f "$state/volume-data/speedtest-node-$volume/enrolled" ]]; then
      tar -cf - -C "$state/volume-data/speedtest-node-$volume" enrolled
    else
      exit 1
    fi
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
    for update in (legacy, engine):
        interval, offset = int(update.group(1)), int(update.group(2))
        if interval < 5 or offset < 0 or offset >= interval:
            print("schedule offset violates interval constraint", file=sys.stderr)
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
    json.dump({"requests": [], "idempotency": {}, "creates": 0, "etag": 0, "put_attempts": 0, "nodes": [], "schedules": {}}, stream)
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
    MOCK_DOCKER_STATE="$tmp/docker" XDG_STATE_HOME="$tmp/state" STG_ENV_FILE="$tmp/stg.env" NODE_NETWORK=host \
    CENTER_URL=http://127.0.0.1:18090 "$@" bash deploy/docker/nodes.sh up "$name"
}

run_nodes_command() {
  local command="$1"
  shift
  env PATH="$fakebin:$PATH" MOCK_HTTP_STATE="$tmp/http-nodes.json" \
    MOCK_DOCKER_STATE="$tmp/docker" XDG_STATE_HOME="$tmp/state" STG_ENV_FILE="$tmp/stg.env" NODE_NETWORK=host \
    CENTER_URL=http://127.0.0.1:18090 bash deploy/docker/nodes.sh "$command" "$@"
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
assert state["creates"] == 1
assert len([r for r in state["requests"] if r["method"] == "POST" and r["path"] == "/api/nodes"]) == 1
assert not os.path.exists(os.path.join(sys.argv[2], "containers")), "container started before schedule succeeded"
PY

run_nodes dock-retry NODE_INTERVAL=45 >"$tmp/node-retry.log" 2>&1
run_nodes dock-stale NODE_INTERVAL=5 MOCK_PUT_CODES=412,200 MOCK_PUT_START=2 >"$tmp/node-stale.log" 2>&1
python3 - "$tmp/http-nodes.json" "$tmp/docker" <<'PY'
import json, os, sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
node_requests = [r for r in state["requests"] if r["method"] == "POST" and r["path"] == "/api/nodes"]
assert state["creates"] == 2
assert len(node_requests) == 2, node_requests
assert node_requests[0]["key"] != node_requests[1]["key"], "distinct creates reused an idempotency key"
stale_updates = [u for u in state["schedule_updates"] if u["body"] and '"interval":5' in u["body"]]
assert [u["code"] for u in stale_updates] == [412, 200], stale_updates
assert stale_updates[0]["if_match"] != stale_updates[1]["if_match"], stale_updates
containers = open(os.path.join(sys.argv[2], "containers"), encoding="utf-8").read().splitlines()
assert containers == ["speedtest-node-dock-retry", "speedtest-node-dock-stale"], containers
PY

if run_nodes dock-idem NODE_INTERVAL=35 MOCK_POST_DROP=1 >"$tmp/node-idem-drop.log" 2>&1; then
  echo 'FAIL: nodes.sh treated a lost create response as success' >&2
  exit 1
fi
run_nodes dock-idem NODE_INTERVAL=35 >"$tmp/node-idem-retry.log" 2>&1
python3 - "$tmp/http-nodes.json" "$tmp/state/speedtest/node-attempts" <<'PY'
import json, os, sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
posts = [r for r in state["requests"] if r["method"] == "POST" and r["path"] == "/api/nodes" and json.loads(r["body"])["name"] == "dock-idem"]
assert len(posts) == 2 and posts[0]["key"] == posts[1]["key"] and posts[0]["body"] == posts[1]["body"], posts
assert len([node for node in state["nodes"] if node["name"] == "dock-idem"]) == 1, state["nodes"]
assert not os.listdir(sys.argv[2]), os.listdir(sys.argv[2])
PY

run_nodes dock-saved NODE_INTERVAL=60 >"$tmp/node-saved-first.log" 2>&1
python3 - "$tmp/http-nodes.json" <<'PY'
import json, sys
path = sys.argv[1]
state = json.load(open(path, encoding="utf-8"))
node = next(node for node in state["nodes"] if node["name"] == "dock-saved")
state["schedules"][node["id"]] = {"interval": 25, "offset": 4, "enabled": True}
with open(path, "w", encoding="utf-8") as stream:
    json.dump(state, stream)
PY
run_nodes_command down dock-saved
run_nodes dock-saved NODE_INTERVAL=45 >"$tmp/node-saved-restart.log" 2>&1
python3 - "$tmp/http-nodes.json" "$tmp/state/speedtest/node-attempts" <<'PY'
import json, os, sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
node = next(node for node in state["nodes"] if node["name"] == "dock-saved")
creates = [r for r in state["requests"] if r["method"] == "POST" and r["path"] == "/api/nodes" and json.loads(r["body"])["name"] == "dock-saved"]
updates = [r for r in state["requests"] if r["method"] == "PUT" and r["path"] == "/api/schedules/" + node["id"]]
staggers = [r for r in state["requests"] if r["method"] == "POST" and r["path"] == "/api/schedules/stagger"]
assert len(creates) == 1, creates
assert len(updates) == 1, updates
assert len(staggers) == 4, staggers
assert state["schedules"][node["id"]] == {"interval": 25, "offset": 4, "enabled": True}, state["schedules"]
assert not os.listdir(sys.argv[2]), os.listdir(sys.argv[2])
PY
run_nodes_command down dock-saved
run_nodes_command forget dock-saved >"$tmp/node-saved-forget.log" 2>&1
run_nodes dock-saved NODE_INTERVAL=30 >"$tmp/node-saved-recreate.log" 2>&1
python3 - "$tmp/http-nodes.json" "$tmp/docker" "$tmp/state/speedtest/node-attempts" <<'PY'
import json, os, sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
nodes = [node for node in state["nodes"] if node["name"] == "dock-saved"]
posts = [r for r in state["requests"] if r["method"] == "POST" and r["path"] == "/api/nodes" and json.loads(r["body"])["name"] == "dock-saved"]
updates = [r for r in state["requests"] if r["method"] == "PUT" and r["path"] == "/api/schedules/" + nodes[1]["id"]]
events = open(os.path.join(sys.argv[2], "volume-events"), encoding="utf-8").read().splitlines()
assert len(nodes) == 2 and nodes[0].get("deleted") and not nodes[1].get("deleted"), nodes
assert len(posts) == 2 and posts[0]["key"] != posts[1]["key"], posts
assert len(updates) == 1 and json.loads(updates[0]["body"])["interval"] == 30, updates
assert state["schedules"][nodes[1]["id"]]["interval"] == 30, state["schedules"]
assert "remove speedtest-node-dock-saved" in events, events
assert "speedtest-node-dock-saved" in open(os.path.join(sys.argv[2], "volumes"), encoding="utf-8").read().splitlines()
assert not os.listdir(sys.argv[3]), os.listdir(sys.argv[3])
PY

if run_nodes dock-forget NODE_INTERVAL=45 MOCK_PUT_CODES=500 MOCK_PUT_START=7 >"$tmp/node-forget-pending.log" 2>&1; then
  echo 'FAIL: nodes.sh treated a failed initial schedule update as success' >&2
  exit 1
fi
python3 - "$tmp/http-nodes.json" "$tmp/state/speedtest/node-attempts" <<'PY'
import hashlib, json, os, sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
node = next(node for node in state["nodes"] if node["name"] == "dock-forget")
attempt = os.path.join(sys.argv[2], hashlib.sha256(b"dock-forget").hexdigest() + ".json")
assert os.path.exists(attempt), "incomplete create attempt did not retain retry state"
assert not node.get("deleted", False), node
PY
run_nodes_command forget dock-forget >"$tmp/node-forget.log" 2>&1
run_nodes dock-forget NODE_INTERVAL=30 >"$tmp/node-forget-recreate.log" 2>&1
python3 - "$tmp/http-nodes.json" "$tmp/state/speedtest/node-attempts" <<'PY'
import hashlib, json, os, sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
nodes = [node for node in state["nodes"] if node["name"] == "dock-forget"]
posts = [r for r in state["requests"] if r["method"] == "POST" and r["path"] == "/api/nodes" and json.loads(r["body"])["name"] == "dock-forget"]
attempt = os.path.join(sys.argv[2], hashlib.sha256(b"dock-forget").hexdigest() + ".json")
assert len(nodes) == 2 and nodes[0].get("deleted") and not nodes[1].get("deleted"), nodes
assert len(posts) == 2 and posts[0]["key"] != posts[1]["key"], posts
assert not os.path.exists(attempt), "completed create attempt key was not retired"
assert state["schedules"][nodes[1]["id"]]["interval"] == 30, state["schedules"]
PY

before_enrollment_staggers="$(python3 - "$tmp/http-nodes.json" <<'PY'
import json, sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
print(len([r for r in state["requests"] if r["method"] == "POST" and r["path"] == "/api/schedules/stagger"]))
PY
)"
if run_nodes dock-enrollment-retry NODE_INTERVAL=45 MOCK_DOCKER_ENROLL_ON_RUN=0 >"$tmp/node-enrollment-interrupted.log" 2>&1; then
  echo 'FAIL: nodes.sh reported success before enrollment was confirmed' >&2
  exit 1
fi
grep -Fq 'enrollment is not confirmed' "$tmp/node-enrollment-interrupted.log" || { echo 'FAIL: enrollment interruption did not explain the pending retry' >&2; exit 1; }
python3 - "$tmp/http-nodes.json" "$tmp/state/speedtest/node-attempts" "$tmp/docker" <<'PY'
import hashlib, json, os, sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
node = next(node for node in state["nodes"] if node["name"] == "dock-enrollment-retry")
attempt = os.path.join(sys.argv[2], hashlib.sha256(b"dock-enrollment-retry").hexdigest() + ".json")
saved = json.load(open(attempt, encoding="utf-8"))
assert saved["stage"] == "scheduled" and saved["nodeId"] == node["id"] and saved["token"], saved
assert state["schedules"][node["id"]] == {"interval": 45, "offset": 0, "enabled": True}, state["schedules"]
assert "speedtest-node-dock-enrollment-retry" in open(os.path.join(sys.argv[3], "containers"), encoding="utf-8").read().splitlines()
assert open(os.path.join(sys.argv[3], "running.speedtest-node-dock-enrollment-retry"), encoding="utf-8").read().strip() == "false"
run_token = next(line.rstrip("\n").split("\t", 1)[1] for line in open(os.path.join(sys.argv[3], "run-tokens"), encoding="utf-8") if line.startswith("speedtest-node-dock-enrollment-retry\t"))
assert run_token == saved["token"], "first container did not receive the saved enrollment token"
assert os.path.exists(os.path.join(sys.argv[3], "volume-data", "speedtest-node-dock-enrollment-retry")), "retry volume was not retained"
PY
python3 - "$tmp/http-nodes.json" <<'PY'
import json, sys
path = sys.argv[1]
state = json.load(open(path, encoding="utf-8"))
node = next(node for node in state["nodes"] if node["name"] == "dock-enrollment-retry")
state["schedules"][node["id"]] = {"interval": 25, "offset": 4, "enabled": True}
with open(path, "w", encoding="utf-8") as stream:
    json.dump(state, stream)
PY
run_nodes_command down dock-enrollment-retry
python3 - "$tmp/state/speedtest/node-attempts" "$tmp/docker" <<'PY'
import hashlib, os, sys
attempt = os.path.join(sys.argv[1], hashlib.sha256(b"dock-enrollment-retry").hexdigest() + ".json")
assert os.path.exists(attempt), "down discarded the enrollment retry token"
assert "speedtest-node-dock-enrollment-retry" in open(os.path.join(sys.argv[2], "volumes"), encoding="utf-8").read().splitlines()
assert "speedtest-node-dock-enrollment-retry" not in open(os.path.join(sys.argv[2], "containers"), encoding="utf-8").read().splitlines()
PY
run_nodes dock-enrollment-retry NODE_INTERVAL=30 >"$tmp/node-enrollment-retry.log" 2>&1
python3 - "$tmp/http-nodes.json" "$tmp/state/speedtest/node-attempts" "$tmp/docker" "$before_enrollment_staggers" <<'PY'
import hashlib, json, os, sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
node = next(node for node in state["nodes"] if node["name"] == "dock-enrollment-retry")
attempt = os.path.join(sys.argv[2], hashlib.sha256(b"dock-enrollment-retry").hexdigest() + ".json")
posts = [r for r in state["requests"] if r["method"] == "POST" and r["path"] == "/api/nodes" and json.loads(r["body"])["name"] == "dock-enrollment-retry"]
updates = [r for r in state["requests"] if r["method"] == "PUT" and r["path"] == "/api/schedules/" + node["id"]]
run_tokens = [line.split("\t", 1) for line in open(os.path.join(sys.argv[3], "run-tokens"), encoding="utf-8") if line.startswith("speedtest-node-dock-enrollment-retry\t")]
staggers = [r for r in state["requests"] if r["method"] == "POST" and r["path"] == "/api/schedules/stagger"]
assert len(posts) == 1, posts
assert len(updates) == 1 and state["schedules"][node["id"]] == {"interval": 25, "offset": 4, "enabled": True}, updates
assert len(run_tokens) == 2 and run_tokens[0][1] == run_tokens[1][1] != "", run_tokens
assert not os.path.exists(attempt), "retry token was not retired after confirmed enrollment"
assert os.path.exists(os.path.join(sys.argv[3], "volume-data", "speedtest-node-dock-enrollment-retry", "enrolled"))
assert len(staggers) == int(sys.argv[4]) + 1, staggers
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
requests = [r for r in http["requests"] if r["method"] == "POST" and r["path"] == "/api/nodes"]
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
assert len([r for r in http["requests"] if r["method"] == "POST" and r["path"] == "/api/nodes"]) == 2, http
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

init_http_state "$tmp/http-five-default.json"
run_stage "$tmp/stage-five-default" "$tmp/db-five-default.json" "$tmp/http-five-default.json" "$tmp/agent-five-default.log" "$tmp/stage-five-default.log" STAGING_INTERVAL_MINUTES=5
python3 - "$tmp/db-five-default.json" "$tmp/http-five-default.json" <<'PY'
import json, sys
db = json.load(open(sys.argv[1], encoding="utf-8"))
http = json.load(open(sys.argv[2], encoding="utf-8"))
assert len(db["schedules"]) == 3 and len(db["engine_schedules"]) == 3, db
assert db["schedules"] == db["engine_schedules"], db
assert sorted(row["offset"] for row in db["schedules"].values()) == [0, 1, 2], db["schedules"]
assert all(row["interval"] == 5 and row["offset"] < row["interval"] for row in db["schedules"].values()), db["schedules"]
assert len([r for r in http["requests"] if r["method"] == "POST" and r["path"] == "/api/nodes"]) == 3, http
PY

init_http_state "$tmp/http-five-invalid.json"
python3 - "$tmp/db-five-invalid.json" <<'PY'
import json, sys
with open(sys.argv[1], "w", encoding="utf-8") as stream:
    json.dump({"schedules": {}, "engine_schedules": {}, "transactions": []}, stream)
PY
if run_stage "$tmp/stage-five-invalid" "$tmp/db-five-invalid.json" "$tmp/http-five-invalid.json" "$tmp/agent-five-invalid.log" "$tmp/stage-five-invalid.log" STAGING_INTERVAL_MINUTES=5 STAGING_NODES='VM1:0 VM2:10'; then
  echo 'FAIL: staging accepted a node offset that violates the interval constraint' >&2
  exit 1
fi
python3 - "$tmp/http-five-invalid.json" "$tmp/db-five-invalid.json" <<'PY'
import json, sys
http = json.load(open(sys.argv[1], encoding="utf-8"))
db = json.load(open(sys.argv[2], encoding="utf-8"))
assert not any(r["method"] == "POST" and r["path"] == "/api/nodes" for r in http["requests"]), http["requests"]
assert not db["transactions"], db["transactions"]
PY

node_state="$tmp/node-entry"
mkdir -p "$node_state"
if env PATH="$fakebin:$PATH" MOCK_AGENT_LOG="$tmp/node-agent.log" SPEEDTEST_STATE_DIR="$node_state" NODE_INTERVAL=4 \
  NODE_NAME=VM3 CENTER_URL=http://127.0.0.1:18090 bash deploy/docker/node-entrypoint.sh >"$tmp/node-entry-invalid.log" 2>&1; then
  echo 'FAIL: node-entrypoint accepted an invalid interval' >&2
  exit 1
fi
missing_token_state="$tmp/node-entry-missing-token"
mkdir -p "$missing_token_state"
if env PATH="$fakebin:$PATH" MOCK_AGENT_LOG="$tmp/node-agent.log" SPEEDTEST_STATE_DIR="$missing_token_state" NODE_INTERVAL=5 \
  NODE_NAME=VM4 CENTER_URL=http://127.0.0.1:18090 bash deploy/docker/node-entrypoint.sh >"$tmp/node-entry-missing-token.log" 2>&1; then
  echo 'FAIL: node-entrypoint accepted an empty-volume start without an enrollment token' >&2
  exit 1
fi
grep -Fq 'ENROLL_TOKEN' "$tmp/node-entry-missing-token.log" || { echo 'FAIL: missing-token error was not actionable' >&2; exit 1; }
env PATH="$fakebin:$PATH" MOCK_AGENT_LOG="$tmp/node-agent.log" SPEEDTEST_STATE_DIR="$node_state" NODE_INTERVAL=5 \
  NODE_NAME=VM3 CENTER_URL=http://127.0.0.1:18090 ENROLL_TOKEN=test-only bash deploy/docker/node-entrypoint.sh >"$tmp/node-entry-first.log" 2>&1
env PATH="$fakebin:$PATH" MOCK_AGENT_LOG="$tmp/node-agent.log" SPEEDTEST_STATE_DIR="$node_state" NODE_INTERVAL=30 \
  NODE_NAME=VM3 CENTER_URL=http://127.0.0.1:18090 bash deploy/docker/node-entrypoint.sh >"$tmp/node-entry-restart.log" 2>&1
assert_eq "$(grep -c '^enroll$' "$tmp/node-agent.log")" 1 'extra-node enrollment count across restart'
assert_eq "$(grep -c '^run$' "$tmp/node-agent.log")" 2 'extra-node run count'
grep -Fq 'interval: 5m' "$node_state/config.yaml" || { echo 'FAIL: extra-node interval override not persisted' >&2; exit 1; }
! grep -Fq 'interval: 30m' "$node_state/config.yaml" || { echo 'FAIL: restart rewrote extra-node interval' >&2; exit 1; }

echo 'PASS: staging and extra-node defaults, overrides, validation, transaction failures, enrollment retry, idempotency, and schedule preservation'
