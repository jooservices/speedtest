# Speedtest

Monitor the real speed of every internet connection you own, around the clock.

Small agents on your machines run a speed test on a schedule and report to one dashboard. The dashboard stores every result, remembers which public IP each machine used and where it is, and shows averages, trends and the best and worst hours.

**Status: interface preview and backend foundation.** `ui/` is a complete, clickable website (plain HTML, CSS and JavaScript) running on generated sample data. Everything you change is kept in the browser's local storage. The command center serves the UI, checks database readiness, provides owner authentication and session endpoints, and supports node enrollment, credential lifecycle and authenticated agent ingestion endpoints (results, configuration, heartbeat, commands and observed IP). The agent can enroll, run scheduled speed tests, heartbeat every 30 s, sync its schedule with ETags, and handle `run_now`, `pause`, `resume` and `update_config`; spooled result reporting is being added. Other product API areas are still being implemented. PostgreSQL migrations, a connection pool, and geolocation modules are available.

## View the preview

```bash
python3 -m http.server 8765 --directory ui
# open http://127.0.0.1:8765/
```

Opening `ui/index.html` directly also works. Dark is the default theme; use the sun/moon button to switch. "Reset demo data" (sidebar or Settings → Data) restores the sample data.

## What is in the preview

| Page | What you can do |
|---|---|
| Dashboard | Fleet tiles (speed versus expected, latency, success rate, IP changes), one card per node, comparison chart (speed or % of expected, chart or table), latest results, open alerts |
| Nodes | List, guided "Add a node" wizard (install over SSH or copy one command), node detail with settings, agent controls and removal |
| Results | Filter, search, sort, paginate, open full detail, export CSV |
| Statistics | Per-node summary (average, median, slow/fast 5%, variation, stability), trend with optional combined total, time of day, weekday × hour heatmap, distribution |
| IP & location | Map of current locations, address-change timeline, every address seen with geolocation, lookup tool |
| Schedules | Interval and start minute per node, keep tests from overlapping, upcoming runs, data-usage estimate |
| Alerts | Activity with acknowledge, rule editor (download, upload, latency, jitter, loss, failed tests, IP change) |
| Settings | Theme, units, time zone, test engine, location service, notifications, retention, export, enrolment key |

Manual "Run test" shows live progress and adds a real entry to the data, so every chart updates.

## Product model

- A **node** is one machine running the agent. Each node is independent and only knows its own configuration: name, expected download/upload speed, schedule, parallel connections. It does not know or care what it is attached to (a single ISP line, a load balancer, a VPN). "WAN" is just a label the user chooses.
- The **command center** receives results, stores them, looks up the geolocation of every new public IP once and remembers it, computes statistics and alerts, and serves the UI.
- Users add nodes from the UI. The UI installs the agent over SSH (or shows one command to paste), so no technical knowledge is needed.

## Planned architecture (not built yet)

```
node (agent) ──HTTPS──► command center ── API + scheduler + database ── web UI
```

See [`docs/architecture.md`](docs/architecture.md) for the full component
diagram and a walkthrough of the agent, command center, database, geolocation
and UI.

- Agent: small single binary running the speed test program (Ookla CLI by default) bound to the right network interface, reporting results and heartbeats.
- Geolocation: MaxMind GeoLite2 City + ASN offline database first (free, no rate limit), free web service as fallback; results cached per IP.
- Keep tests from overlapping so a node that measures a combination of connections is never skewed by other nodes testing at the same moment.

## Repository layout

```
cmd/agent/              node agent entry point
cmd/center/             command center entry point
internal/agent/         node agent implementation
internal/agent/config/  loads the node's own config.yaml
internal/agent/enroll/  exchanges an enrollment token for a 0600 credential
internal/agent/engine/  executes Ookla or LibreSpeed speed tests
internal/agent/netpath/ validates a node's local network binding
internal/agent/sched/   runs tests on the node's local schedule (interval + offset, quiet hours)
internal/agent/control/ heartbeats, syncs configuration (ETag) and acknowledges commands
internal/center/auth/   Argon2id password hashes, owner sessions and login limits
internal/center/server/ command center HTTP handlers and lifecycle
internal/center/db/     PostgreSQL pool and migration helpers
internal/center/geo/    offline and online geolocation
internal/shared/config/ command-center configuration loaded from the environment
migrations/             database migrations
deploy/                 deployment artifacts
docs/                   design and process documents
ui/                     the interface preview (unchanged, no build step)
ui.go                   embeds ui/ for the command center binary
```

## Development

Prerequisites: Go 1.27 or later (installed natively on the Dev Workspaces VM).

Copy `.env.example` to `.env` and adjust it; every variable is documented there
with its default, and no secret is committed.

```bash
make build        # go build ./...
make test         # go test ./...
make lint         # golangci-lint run
make ci           # vet + test + build + lint
make migrate      # apply embedded PostgreSQL migrations using .env
make migrate-down # roll back latest migration (deletes version 1 data)
make test-db      # migration/constraint integration tests on a verified test DB
make run-center   # start the command center
go run ./cmd/center seed-admin # create the default administrator in local/test only
make run-agent    # start a node agent
```

Configuration is read from environment variables (documented in `.env.example`; see
[`docs/development.md`](docs/development.md)). The command center, seed command and
migration commands load the current directory's `.env` (existing environment
values take precedence). Service endpoints are never hard-coded to Docker or a
specific machine. Set `APP_ENV=local` or `APP_ENV=test` before running
`go run ./cmd/center seed-admin`; the command refuses staging, production and
unknown environments, and it does not replace an existing administrator
password. The local/test default login is documented in
`JOOservices/guides/default-credentials.md`.
For local/test startup and seeding, the configured database must be
`speedtest_test` or this worktree's `speedtest_wt*` database and carry the
Workspaces ownership marker; the live `speedtest` database is refused.
Read [the migration runbook](migrations/README.md) and verify the non-live
database before any migration or database test. `make test-db` verifies the
database name and ownership marker, resets its application tables and migration
journal, and runs self-contained suites serially.
Use only a disposable test/worktree database; never run integration suites
concurrently against the same database.

### Agent commands

The agent is a single binary with four subcommands:

```bash
agent enroll --center <url> --token <token> --name <name>   # enroll once, store the credential
agent run                                                   # run as a background service
agent version                                               # print the version
agent uninstall                                             # remove config and credential
```

`agent enroll` accepts the command-center URL with `--center`, exchanges the
one-time token, then writes the credential to `SPEEDTEST_AGENT_CREDENTIAL`
(`/etc/speedtest-agent/credential` by default) with mode 0600. It never logs the
token or credential. `agent run` loads node values from the YAML file at
`SPEEDTEST_AGENT_CONFIG` (`/etc/speedtest-agent/config.yaml` by default),
refuses to start if the credential file is group or world-readable, and logs
structured JSON to stdout for the journal. When the YAML file is missing, these
defaults apply:

```yaml
engine: ookla
name: node
expected_download_mbps: 0
expected_upload_mbps: 0
interval: 30m
# Optional: choose one local path; omit both to use the default route.
# network_interface: enp1s0
# source_ip: 192.0.2.10
```

`network_interface` and `source_ip` are mutually exclusive. The selected
interface must exist, be up, and have an IP address; the selected source IP
must be assigned locally. See [Linux multi-uplink routing](docs/multi-wan.md)
for policy-routing prerequisites and verification.

Only the configuration and credential file paths and center address are agent
environment overrides; node values come from `config.yaml`. Malformed YAML,
unknown keys, and invalid values fail fast with a configuration error.

### Agent control loop

`agent run` also reads `SPEEDTEST_AGENT_CENTER` (the same origin given to
`agent enroll`) and connects to it as a background service:

- **Heartbeat** every 30 s with the agent version, process uptime and the last
  test time.
- **Configuration sync** using `GET /api/agent/config` with `If-None-Match`;
  an unchanged configuration returns 304; a changed schedule is applied
  without restart. The schedule (interval, offset, enabled) and quiet hours come
  from the center; the local `config.yaml` only seeds the bootstrap interval.
- **Commands** are polled and acknowledged (`run_now`, `pause`, `resume`,
  `update_config`); a command is executed once and redeliveries are only
  re-acknowledged.
- **Offline behavior**: if the center is unreachable the agent keeps the last
  known schedule and only logs.

The local scheduler runs one test at a time on the slot grid
`epoch + offset + k*interval` with a small jitter, honors quiet hours and the
enabled flag, and never stacks a test that overruns its slot (the missed slot is
skipped and logged). See [agent control and scheduling](docs/agent-control.md)
for configuration, command semantics and operational checks.

The command center listens on `SPEEDTEST_HTTP_ADDR` (default
`127.0.0.1:8080`). Its UI is embedded in the binary by default. For development, set
`SPEEDTEST_UI_DIR=./ui` or run `go run ./cmd/center -ui-dir ./ui` to serve
the files from disk instead. Until a database pinger is wired into the server,
`/readyz` reports that the database is not configured.

Set `engine: librespeed` in a node's config to use `librespeed-cli`. Its
optional `parallel_connections` key overrides the default configured in `.env`;
the LibreSpeed binary, server-list URL and timeout are also configured there.
See [`docs/engine.md`](docs/engine.md) for the engine settings and result
mapping.

### `make ci` prerequisites

`make ci` runs the same checks as GitHub Actions, including `golangci-lint`
v2.15.0. That binary must be on `PATH` (the other targets only need Go). Install
it into a local, git-ignored directory if it is not already installed:

```bash
mkdir -p .tmp/bin
curl -fL https://github.com/golangci/golangci-lint/releases/download/v2.15.0/golangci-lint-2.15.0-linux-amd64.tar.gz -o .tmp/golangci-lint-2.15.0-linux-amd64.tar.gz
curl -fL https://github.com/golangci/golangci-lint/releases/download/v2.15.0/golangci-lint-2.15.0-checksums.txt -o .tmp/golangci-lint-2.15.0-checksums.txt
(cd .tmp && sha256sum --ignore-missing -c golangci-lint-2.15.0-checksums.txt)
tar -xzf .tmp/golangci-lint-2.15.0-linux-amd64.tar.gz -C .tmp/bin --strip-components=1 golangci-lint-2.15.0-linux-amd64/golangci-lint
PATH="$PWD/.tmp/bin:$PATH" make ci
```

For another OS/architecture, see [development.md](docs/development.md#make-ci-prerequisites).

`.tmp/` is git-ignored, so the local binary is never committed. The workflow
`.github/workflows/ci.yml` runs vet, test and `golangci-lint` on every push and
pull request using the same pinned version.

## Documentation

- [`docs/architecture.md`](docs/architecture.md) — how the agent, command
  center, database, geolocation and UI fit together, with a component diagram.
- [`docs/development.md`](docs/development.md) — setup, `.env`, database
  provisioning, running the center and agent locally, and running tests.
- [`docs/glossary.md`](docs/glossary.md) — definitions of node, result,
  expected speed, IP history, rule and event.
- [`docs/api.md`](docs/api.md) and [`docs/openapi.yaml`](docs/openapi.yaml) —
  the API contract for the agent and UI.
- [`docs/ui-decisions.md`](docs/ui-decisions.md) — the reasoning behind the UI
  choices and the open questions.
- [`docs/definition-of-done.md`](docs/definition-of-done.md) — what "done"
  means for a ticket in this project.

### Geolocation lookup

`internal/center/geo` provides offline GeoLite2 City/ASN lookups, a weekly,
checksum-verified updater, an `ip_addresses` result cache and optional ip-api /
IPinfo Lite fallbacks. The resolver is not yet wired to command-center handlers.
This does not change the demo UI. See
[configuration, lifecycle and verification](docs/geolocation.md).

See [`docs/engine.md`](docs/engine.md) for the Ookla engine API, environment configuration, units, errors and offline runner tests.

Release packaging and the one-line agent installer: [`docs/release.md`](docs/release.md).

See [reliable result reporting](docs/reporting.md) for the JOOST-17 SQLite spool,
UUID replay contract, retention settings and integration API. This library is
available for lifecycle composition; `agent run` does not schedule or submit
measurements yet.
