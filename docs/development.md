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
The current Go tests do not connect to PostgreSQL. Migrations in `migrations/`
are plain SQL applied in lexical order; the runner lands in a later ticket
(see `migrations/README.md`).

## Running the center and the agent locally

Both binaries are scaffolds today: they load and report their configuration and
exit, without an HTTP server, database access or a test loop.

```bash
make run-center   # loads and prints the command-center configuration
make run-agent    # loads and prints the node-agent configuration
```

With no `.env`, the documented defaults apply and both exit `0`:

```
center: configuration loaded
center: would listen on :8080 (HTTP server not implemented yet)
center: database host 127.0.0.1 port 5432
...
```

The password is intentionally never printed.

### UI preview

The approved interface preview needs no backend. Serve it statically:

```bash
python3 -m http.server 8765 --directory ui
# open http://127.0.0.1:8765/
```

Opening `ui/index.html` directly also works.

## Running tests

```bash
make test     # go test ./...
make vet      # go vet ./...
make build    # go build ./...
make lint     # golangci-lint run
make ci       # vet + test + build + lint, the same checks as CI
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
