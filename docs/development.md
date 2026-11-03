# Development

How to set up, configure, run and test Speedtest locally. Read
[architecture.md](architecture.md) for how the parts fit together.

## Prerequisites

- Go 1.27 or later, installed natively on the Dev Workspaces VM.
- Node and Python only for optional tooling and the UI preview; the Go binaries
  need nothing else.
- PostgreSQL for anything that touches the database; use the shared native
  service on the Workspaces Dev VM, or your own instance elsewhere. Configure
  its address through `.env`.

## Setup

From an existing repository checkout:

```bash
cp .env.example .env
```

`make build` and `make test` work with no further setup. The `.env` file is
only needed when you override a default or set the database password.

## Configuration (`.env`)

Every variable is documented with its default in `.env.example`; the value
shown after `=` there is the default used when the variable is unset or empty.
Only `SPEEDTEST_DB_PASSWORD` has no default — set it before connecting to a real
database, and never commit the real value (`.env` is git-ignored).

The binaries read configuration from **environment variables only**; there is
no automatic `.env` file loader. To run with a `.env` file, export it into the
shell first. Values containing shell-special characters must be quoted (for
example, use single quotes around a literal password); source only a trusted
file because the shell executes it:

```bash
set -a; . ./.env; set +a
make run-center
```

Or set the variables individually for a single command:

```bash
SPEEDTEST_NODE_NAME=vm1 SPEEDTEST_NODE_EXPECTED_DOWNLOAD_MBPS=300 make run-agent
```

Service addresses are never hard-coded to Docker or to a specific machine.

## Database provisioning

The command center stores everything in PostgreSQL, provided by the shared
Workspaces service. There are three databases per environment:

- `speedtest` — live data, never deleted.
- `speedtest_test` — wiped on every test run.
- `speedtest_wt<id>` — one per ticket worktree.

### Workspaces Dev VM

The shared helper tools are not yet adapted to this VM's native services.
Create databases directly with `createdb`/`psql` on native PostgreSQL at
`127.0.0.1:15435`: `speedtest`, `speedtest_test` and `speedtest_wt<id>`.
Mark each with `COMMENT ON DATABASE <name> IS 'workspaces-project:speedtest'`
(substitute its actual name). Credentials live in the private Workspaces root
`.env.docker`; `.env` holds other credentials and `.env.homelab` is for homelab
access, not development. Never print or commit credentials. Set the worktree's
`SPEEDTEST_DB_*` variables in its private `.env`, including port `15435` and its
own `speedtest_wt<id>` database name.

### Anywhere else

Run your own PostgreSQL and point `SPEEDTEST_DB_*` in `.env` at it. Use the same
live, test and worktree naming scheme and ownership comments above.

### Safety

Migrate or test only on `speedtest_test` or `speedtest_wt<id>`, never on
`speedtest`. Verify the configured database name before any database operation.
Unit tests need no database. Database-backed tests carry the `integration` build
tag, run serially (`-p 1`, they share one database) and refuse any database other
than `speedtest_test` or `speedtest_wt<id>` (guard in `internal/center/db`,
helpers in `internal/center/db/dbtest`). Migrations in `migrations/` are applied
by the embedded runner (`make migrate`, `make migrate-down`; see
`migrations/README.md`).

## Running the center and the agent locally

```bash
make migrate      # apply the schema to the database in .env (a test or worktree database)
make run-center   # HTTP server on SPEEDTEST_HTTP_ADDR (default 127.0.0.1:8080); Ctrl+C stops it
```

The center serves the embedded UI at `/` (set `SPEEDTEST_UI_DIR` to serve `ui/`
from disk while editing it), `GET /healthz` (process) and `GET /readyz`
(readiness; reports the database as "not configured" until a pool is wired), and
writes JSON access logs with a request id. API paths return RFC 9457
`application/problem+json` errors. The business endpoints of `docs/openapi.yaml`
are implemented by later tickets.

The agent is configured by `config.yaml` (default `/etc/speedtest-agent/config.yaml`)
plus the `SPEEDTEST_AGENT_CONFIG` and `SPEEDTEST_AGENT_CREDENTIAL` path overrides:

```bash
go run ./cmd/agent enroll --center http://127.0.0.1:8080 --token <token> --name vm1
make run-agent    # agent run: needs an enrolled credential file with mode 0600
go run ./cmd/agent version
go run ./cmd/agent uninstall
```

Agent building blocks: `internal/agent/config`, `internal/agent/enroll`,
`internal/agent/engine` (Ookla runner, see [engine.md](engine.md)); offline test
helpers (fake speedtest binary, recording center double) are described in
[testing-harness.md](testing-harness.md). Geolocation is in
[geolocation.md](geolocation.md).

### UI preview

The approved interface preview needs no backend. Serve it statically:

```bash
python3 -m http.server 8765 --directory ui
# open http://127.0.0.1:8765/
```

Opening `ui/index.html` directly also works.

## Running tests

```bash
make test     # fetches the geolocation test fixtures, then go test ./...
make vet      # go vet ./...
make build    # go build ./...
make lint     # golangci-lint run
make ci       # vet + test + build + lint, the same checks as CI
make test-db  # integration tests (-tags integration -p 1) on the database in .env

```

The Go tests today exercise only configuration loading and need no database.
Database-backed tests, when they exist, run against `speedtest_test` or the
worktree database after verifying its name — never against `speedtest`.

### `make ci` prerequisites

`make ci` includes `golangci-lint` v2.15.0, which must be on `PATH` (the other
targets only need Go). Install it into the local, git-ignored `.tmp/` directory
if it is not already installed:

```bash
mkdir -p .tmp/bin
curl -fL https://github.com/golangci/golangci-lint/releases/download/v2.15.0/golangci-lint-2.15.0-linux-amd64.tar.gz -o .tmp/golangci-lint-2.15.0-linux-amd64.tar.gz
curl -fL https://github.com/golangci/golangci-lint/releases/download/v2.15.0/golangci-lint-2.15.0-checksums.txt -o .tmp/golangci-lint-2.15.0-checksums.txt
(cd .tmp && sha256sum --ignore-missing -c golangci-lint-2.15.0-checksums.txt)
tar -xzf .tmp/golangci-lint-2.15.0-linux-amd64.tar.gz -C .tmp/bin --strip-components=1 golangci-lint-2.15.0-linux-amd64/golangci-lint
PATH="$PWD/.tmp/bin:$PATH" make ci
```

`.tmp/` is git-ignored, so the local binary is never committed. CI
(`.github/workflows/ci.yml`) runs vet, test and `golangci-lint` with the same
pinned version on every push and pull request.

For another OS/architecture, select its archive from the same release and
replace `linux-amd64` in the download and extraction commands. Verify against
the same published checksums file (on macOS, use `shasum -a 256 -c` with the
matching archive line). Windows uses the release ZIP and SHA-256 verification.

The composed agent loads the current directory's `.env` on `agent run` (exported
values take precedence). See [agent lifecycle](agent-lifecycle.md) for durable
spooling, command progress, atomic configuration and signal behavior. The local
`config.yaml` accepts optional `server_id: 0` (automatic selection) and
`parallel_connections` (1–64 when set), alongside engine, interval and binding.
The center's effective configuration supersedes these bootstrap measurement
settings after a valid ETag update.
