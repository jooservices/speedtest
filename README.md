# Speedtest

Monitor the real speed of every internet connection you own, around the clock.

Small agents on your machines run a speed test on a schedule and report to one dashboard. The dashboard stores every result, remembers which public IP each machine used and where it is, and shows averages, trends and the best and worst hours.

**Status: interface preview and backend skeleton.** `ui/` is a complete, clickable website (plain HTML, CSS and JavaScript) running on generated sample data. Everything you change is kept in the browser's local storage. The agent can enroll and run as a service but does not execute speed tests or report results yet. The command center embeds and serves the UI and exposes process/readiness checks; the product HTTP API and readiness database ping are not wired yet. PostgreSQL migrations, a connection pool, and geolocation modules are available.

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
make run-agent    # start a node agent
```

Configuration uses the environment; migration commands also load the current
directory's `.env` (existing environment values take precedence). Service
endpoints are never hard-coded to Docker or a specific machine.
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
name: node
expected_download_mbps: 0
expected_upload_mbps: 0
interval: 30m
```

Only the configuration and credential file paths are agent environment
overrides; node values come from `config.yaml`. Malformed YAML, unknown keys,
and invalid values fail fast with a configuration error.

The command center listens on `SPEEDTEST_HTTP_ADDR` (default
`127.0.0.1:8080`). Its UI is embedded in the binary by default. For development, set
`SPEEDTEST_UI_DIR=./ui` or run `go run ./cmd/center -ui-dir ./ui` to serve
the files from disk instead. Until a database pinger is wired into the server,
`/readyz` reports that the database is not configured.

### `make ci` prerequisites

`make ci` runs the same checks as GitHub Actions, including `golangci-lint`
v2.15.0. That binary must be on `PATH` (the other targets only need Go). Install
it into a local, git-ignored directory if it is not already installed:

```bash
mkdir -p .tmp/bin
curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh \
  | sh -s -- -b .tmp/bin v2.15.0
PATH="$PWD/.tmp/bin:$PATH" make ci
```

`.tmp/` is git-ignored, so the local binary is never committed. The workflow
`.github/workflows/ci.yml` runs vet, test and `golangci-lint` on every push and
pull request using the same pinned version.

See [`docs/ui-decisions.md`](docs/ui-decisions.md) for the reasoning behind the UI choices and the open questions.

### Geolocation lookup

`internal/center/geo` provides offline GeoLite2 City/ASN lookups, a weekly,
checksum-verified updater, an `ip_addresses` result cache and optional ip-api /
IPinfo Lite fallbacks. The resolver is not yet wired to command-center handlers.
This does not change the demo UI. See
[configuration, lifecycle and verification](docs/geolocation.md).

See [`docs/engine.md`](docs/engine.md) for the Ookla engine API, environment configuration, units, errors and offline runner tests.
