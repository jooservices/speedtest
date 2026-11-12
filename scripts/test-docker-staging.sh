#!/usr/bin/env bash
# Tests the staging interval defaults of deploy/docker/entrypoint.sh, node-entrypoint.sh and nodes.sh.
# psql, curl, docker and the speedtest binaries are fakes that only log their arguments, so no database,
# Docker daemon or network is needed. Each script runs under env -i so the caller's STAGING_* values never leak in.
set -euo pipefail

here="$(cd "$(dirname "$0")/.." && pwd)"
mkdir -p "$here/.tmp"
work="$(mktemp -d "$here/.tmp/docker-staging-test.XXXXXX")"
trap 'rm -rf "$work"' EXIT

bin="$work/bin"
mkdir -p "$bin"
export PATH="$bin:$PATH"

fail() {
	printf 'FAIL: %s\n' "$*" >&2
	exit 1
}

# Fakes: every call is appended to $FAKE_LOG_DIR/calls.log.
# shellcheck disable=SC2016 # the fake's body must expand when the fake runs, not here
fake_logger='printf "%s %s\n" "$(basename "$0")" "$*" >> "$FAKE_LOG_DIR/calls.log"'
printf '#!/usr/bin/env bash\n%s\n' "$fake_logger" >"$bin/psql"
printf '#!/usr/bin/env bash\n%s\n' "$fake_logger" >"$bin/docker"
printf '#!/usr/bin/env bash\n%s\n' "$fake_logger" >"$bin/speedtest-migrate"
printf '#!/usr/bin/env bash\n%s\n' "$fake_logger" >"$bin/speedtest-center"
printf '#!/usr/bin/env bash\n%s\n' "$fake_logger" >"$bin/speedtest-agent"
cat >"$bin/curl" <<'EOF'
#!/usr/bin/env bash
printf 'curl %s\n' "$*" >> "$FAKE_LOG_DIR/calls.log"
method=GET url="" headers=0 code=0
while [ $# -gt 0 ]; do
	case "$1" in
	-X) method="$2"; shift ;;
	-D) headers=1 ;;
	-w) code=1; shift ;;
	http://*) url="$1" ;;
	esac
	shift
done
if [ "$headers" = 1 ]; then printf 'HTTP/1.1 200 OK\r\nETag: "7"\r\n\r\n'; exit 0; fi
if [ "$code" = 1 ]; then printf '200'; exit 0; fi
case "$url" in
*/healthz) ;;
*/api/auth/me | */api/auth/login) printf '{"data":{"csrfToken":"csrf-test"}}\n' ;;
*/api/nodes) printf '{"data":{"token":"enroll-test","node":{"id":"node-%s"}}}\n' "$RANDOM" ;;
*) printf '{"data":{}}\n' ;;
esac
EOF
chmod +x "$bin"/*

# new_case NAME: a fresh log and state directory for one run.
new_case() {
	case_dir="$work/$1"
	mkdir -p "$case_dir/state"
	FAKE_LOG_DIR="$case_dir"
	export FAKE_LOG_DIR
	: >"$FAKE_LOG_DIR/calls.log"
	printf 'STAGING_OWNER_EMAIL=owner@example.test\nSTAGING_OWNER_PASSWORD=test-only\n' >"$case_dir/stg.env"
	chmod 600 "$case_dir/stg.env"
}

has() { grep -qF -- "$1" "$2" || fail "$3 (expected '$1' in $2)"; }

# entrypoint [NAME=value ...]: runs the center entrypoint with fake binaries; output in $case_dir/out.txt.
entrypoint() {
	env -i PATH="$PATH" FAKE_LOG_DIR="$FAKE_LOG_DIR" STAGING_STATE_DIR="$case_dir/state" \
		SPEEDTEST_DB_HOST=127.0.0.1 SPEEDTEST_DB_PORT=5432 SPEEDTEST_DB_NAME=speedtest SPEEDTEST_DB_USER=test \
		SPEEDTEST_DB_PASSWORD=test-only STAGING_OWNER_EMAIL=owner@example.test STAGING_OWNER_PASSWORD=test-only \
		APP_KEY=test-only "$@" bash "$here/deploy/docker/entrypoint.sh" >"$case_dir/out.txt" 2>&1
}

# node_entrypoint [NAME=value ...]: runs one extra node container entrypoint.
node_entrypoint() {
	env -i PATH="$PATH" FAKE_LOG_DIR="$FAKE_LOG_DIR" STAGING_STATE_DIR="$case_dir/state" \
		NODE_NAME=VM9 CENTER_URL=http://127.0.0.1:18090 ENROLL_TOKEN=test-only "$@" \
		bash "$here/deploy/docker/node-entrypoint.sh" >"$case_dir/out.txt" 2>&1
}

# nodes_up [NAME=value ...]: runs "nodes.sh up VM1" against the fake center and docker.
nodes_up() {
	env -i PATH="$PATH" FAKE_LOG_DIR="$FAKE_LOG_DIR" STG_ENV_FILE="$case_dir/stg.env" "$@" \
		bash "$here/deploy/docker/nodes.sh" up VM1 >"$case_dir/out.txt" 2>&1
}

# Criterion 1: with no override every new staging node starts with a 60-minute interval.
test_default_interval_is_60() {
	new_case default
	entrypoint || fail "entrypoint failed with defaults: $(cat "$case_dir/out.txt")"
	for name in VM1 VM2 VM3; do
		has 'interval: 60m' "$case_dir/state/agents/$name/config.yaml" "$name config must be 60 minutes"
	done
	has 'interval_minutes=60, offset_minutes=0' "$FAKE_LOG_DIR/calls.log" "VM1 schedule must be 60 minutes"

	node_entrypoint || fail "node-entrypoint failed with defaults: $(cat "$case_dir/out.txt")"
	has 'interval: 60m' "$case_dir/state/config.yaml" "extra node config must be 60 minutes"

	nodes_up || fail "nodes.sh failed with defaults: $(cat "$case_dir/out.txt")"
	has 'NODE_INTERVAL=60' "$FAKE_LOG_DIR/calls.log" "extra node container must get NODE_INTERVAL=60"
	if grep -qF -- '-X PUT' "$FAKE_LOG_DIR/calls.log"; then fail "the default interval must not save a schedule on the center"; fi
}

# Criterion 1: explicit STAGING_INTERVAL_MINUTES and NODE_INTERVAL still win over the default.
test_explicit_interval_wins() {
	new_case override
	entrypoint STAGING_INTERVAL_MINUTES=15 || fail "entrypoint failed with override: $(cat "$case_dir/out.txt")"
	has 'interval: 15m' "$case_dir/state/agents/VM1/config.yaml" "staging override must reach the agent config"
	has 'interval_minutes=15, offset_minutes=0' "$FAKE_LOG_DIR/calls.log" "staging override must reach the schedule"

	node_entrypoint NODE_INTERVAL=15 || fail "node-entrypoint failed with override: $(cat "$case_dir/out.txt")"
	has 'interval: 15m' "$case_dir/state/config.yaml" "NODE_INTERVAL override must reach the agent config"

	nodes_up NODE_INTERVAL=15 || fail "nodes.sh failed with override: $(cat "$case_dir/out.txt")"
	has 'NODE_INTERVAL=15' "$FAKE_LOG_DIR/calls.log" "NODE_INTERVAL override must reach the container"
	has '"interval":15' "$FAKE_LOG_DIR/calls.log" "NODE_INTERVAL override must reach the center schedule"
}

# Criterion 2: the effective interval and offset reach both schedule tables and the agent config.
test_interval_reaches_center_and_agent() {
	new_case both-tables
	entrypoint STAGING_INTERVAL_MINUTES=45 || fail "entrypoint failed: $(cat "$case_dir/out.txt")"
	has "UPDATE schedules SET interval_minutes=45, offset_minutes=15, revision=revision+1" "$FAKE_LOG_DIR/calls.log" \
		"legacy schedules row must get the interval and offset"
	has "UPDATE engine_schedules SET interval_minutes=45, offset_minutes=15, revision=revision+1" "$FAKE_LOG_DIR/calls.log" \
		"engine_schedules row must get the interval and offset"
	has "AND engine='ookla'" "$FAKE_LOG_DIR/calls.log" "engine_schedules update must target the ookla row"
	has 'interval: 45m' "$case_dir/state/agents/VM2/config.yaml" "agent config must carry the same interval"

	new_case nodes-center
	nodes_up NODE_INTERVAL=45 || fail "nodes.sh failed: $(cat "$case_dir/out.txt")"
	has '-X PUT' "$FAKE_LOG_DIR/calls.log" "nodes.sh must save the schedule on the center"
	has '"interval":45,"offset":0' "$FAKE_LOG_DIR/calls.log" "center schedule must carry the agent interval"
}

# Criterion 3: offsets stay below the interval, so the default node set works at the 5-minute minimum.
test_offsets_valid_at_minimum_interval() {
	new_case minimum
	entrypoint STAGING_INTERVAL_MINUTES=5 || fail "default node set must start at 5 minutes: $(cat "$case_dir/out.txt")"
	offsets="$(grep -o 'offset_minutes=[0-9]*' "$FAKE_LOG_DIR/calls.log" | cut -d= -f2 | sort -u)"
	[ -n "$offsets" ] || fail "no offsets were written"
	for off in $offsets; do
		[ "$off" -lt 5 ] || fail "offset $off must be below the 5-minute interval"
	done

	new_case default-count
	entrypoint || fail "default node set must start at 60 minutes: $(cat "$case_dir/out.txt")"
	has 'offset_minutes=40' "$FAKE_LOG_DIR/calls.log" "three default nodes must be spread across the 60-minute interval"

	new_case bad-offset
	if entrypoint STAGING_INTERVAL_MINUTES=5 STAGING_NODES='VM1:0 VM2:10'; then
		fail "an offset at or above the interval must be refused"
	fi
	has 'must be below the 5-minute interval' "$case_dir/out.txt" "refusal must name the bad offset"
	if grep -q 'UPDATE' "$FAKE_LOG_DIR/calls.log"; then fail "no schedule may be written for a refused node set"; fi
}

# Review fix: values spliced into SQL or sent as JSON are validated before any write or request.
test_rejects_unsafe_settings() {
	for bad in "5' OR '1'='1" 3 2000; do
		new_case "bad-interval-$bad"
		if entrypoint STAGING_INTERVAL_MINUTES="$bad"; then fail "STAGING_INTERVAL_MINUTES=$bad must be refused"; fi
		has 'STAGING_INTERVAL_MINUTES must be' "$case_dir/out.txt" "refusal of $bad must name the setting"
		if grep -q 'UPDATE' "$FAKE_LOG_DIR/calls.log"; then fail "nothing may be written for STAGING_INTERVAL_MINUTES=$bad"; fi
	done

	new_case bad-engine
	if entrypoint STAGING_ENGINE="ookla'; DROP"; then fail "an unknown STAGING_ENGINE must be refused"; fi
	if grep -q 'UPDATE' "$FAKE_LOG_DIR/calls.log"; then fail "nothing may be written for an unknown engine"; fi

	new_case bad-node-interval
	if nodes_up NODE_INTERVAL="5; rm -rf /"; then fail "a non-numeric NODE_INTERVAL must be refused"; fi
	has 'NODE_INTERVAL must be a whole number' "$case_dir/out.txt" "refusal must name NODE_INTERVAL"
	if grep -qF -- 'docker run' "$FAKE_LOG_DIR/calls.log"; then fail "no container may start for a refused NODE_INTERVAL"; fi

	new_case bad-expected
	if nodes_up EXPECTED_DOWN=abc; then fail "a non-numeric EXPECTED_DOWN must abort before the node is created"; fi
	if grep -qF -- '-X POST http://127.0.0.1:18090/api/nodes -H' "$FAKE_LOG_DIR/calls.log"; then fail "no node may be created from a bad request body"; fi
}

test_default_interval_is_60
test_explicit_interval_wins
test_interval_reaches_center_and_agent
test_offsets_valid_at_minimum_interval
test_rejects_unsafe_settings
printf 'docker staging interval tests: 5 passed\n'
