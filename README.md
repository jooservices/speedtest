# Speedtest

Monitor the real speed of every internet connection you own, around the clock.

Small agents on your machines run a speed test on a schedule and report to one dashboard. The dashboard stores every result, remembers which public IP each machine used and where it is, and shows averages, trends and the best and worst hours.

**Status: interface preview.** `ui/` is a complete, clickable website (plain HTML, CSS and JavaScript) running on generated sample data. Everything you change is kept in the browser's local storage. The Go backend is a scaffold: it builds, tests and loads its configuration, and includes embedded PostgreSQL migrations and a connection pool; API/agent business logic is not implemented yet.

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
internal/agent/         node agent implementation (placeholder)
internal/center/        command center implementation (placeholder)
internal/shared/config/ configuration loaded from the environment
migrations/             database migrations
deploy/                 deployment artifacts
docs/                   design and process documents
ui/                     the interface preview (unchanged, no build step)
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
database before any migration or database test. `make test-db` requires an
empty application schema and verifies its database name and ownership marker.

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
