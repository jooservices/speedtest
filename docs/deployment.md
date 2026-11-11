# Deployment

How to install and run the command center permanently as a systemd user
service, and how to operate it day to day: environment, migrations, upgrade,
rollback, backup and health checks.

This runbook documents the commands; it does **not** run them. Deployment to
staging and release to production are owner decisions (see
[Environment flow](#environment-flow)). No production step is performed here.

For how the parts fit together, read [architecture.md](architecture.md); for
the full configuration reference, [.env.example](../.env.example) and
[development.md](development.md); for the migration runner and schema,
[migrations/README.md](../migrations/README.md).

## What is deployed

Two small binaries built from this module (`github.com/jooservices/speedtest`,
Go 1.27, native on the Dev Workspaces VM):

| Binary | Source | Purpose |
| --- | --- | --- |
| `speedtest-center` | `./cmd/center` | The command center: serves the embedded web UI, the product API and the health endpoints. |
| `speedtest-migrate` | `./internal/center/db/cmd/migrate` | Applies and rolls back the embedded PostgreSQL migrations. |

The center never applies migrations at startup, so a deploy is: build the new
binaries, run `speedtest-migrate up`, then restart the service.

## Environment flow

The Workspaces rules keep real data safe while work keeps running when the Mac
is shut down:

| Machine | Role | Branch |
| --- | --- | --- |
| macOS | interactive development | `develop` |
| Windows WSL | primary workstation; also **staging**; always online | `develop` |
| ESXi | live production | `master` |

Flow: `develop` → verify on staging (WSL) → merge into `master` → release to
ESXi. The owner decides when to deploy to staging and when to release; the AI
reports results and never decides either. This runbook never runs a production
step, and the deployed unit below is a per-user service on the target machine,
not a statement about production.

## Prerequisites

- Go 1.27 or later (native on the Dev Workspaces VM).
- A systemd user session (`systemctl --user`).
- PostgreSQL, the shared Workspaces service, reachable at the address in `.env`.
- A reverse proxy (nginx in the example) to terminate TLS in front of the
  center, which speaks plain HTTP on loopback.

## Install

Everything lives under one release directory so the service and migrations
always read the same `.env`:

```bash
# 1. Lay out the release directory.
install -d ~/.local/opt/speedtest-center

# 2. Build both binaries straight into it from a clean checkout.
go build -o ~/.local/opt/speedtest-center/speedtest-center ./cmd/center
go build -o ~/.local/opt/speedtest-center/speedtest-migrate ./internal/center/db/cmd/migrate

# 3. Create the environment file from the template (see Environment below).
cp .env.example ~/.local/opt/speedtest-center/.env
chmod 600 ~/.local/opt/speedtest-center/.env
# edit it: set SPEEDTEST_DB_*, SPEEDTEST_HTTP_ADDR, SPEEDTEST_UI_DIR and APP_ENV=production, and a
# 32-byte APP_KEY (without it the center cannot issue node tokens or store integration secrets):
#   echo "APP_KEY=$(openssl rand -base64 32)" >> ~/.local/opt/speedtest-center/.env
# Keep a copy of APP_KEY somewhere safe and separate from the database backups.

# 3b. Apply the schema and create the owner account (production never seeds a default password).
cd ~/.local/opt/speedtest-center && ./speedtest-migrate up
printf '%s\n' 'choose-a-long-unique-passphrase' > /dev/shm/owner-pw && chmod 600 /dev/shm/owner-pw
SPEEDTEST_OWNER_EMAIL=you@example.com ./speedtest-center create-owner < /dev/shm/owner-pw; shred -u /dev/shm/owner-pw
# (prompt-free alternative: type the passphrase on one line after `create-owner`, then press Ctrl-D)
# Forgotten password later: same pattern with `reset-owner-password` (sets a new password, signs every session out).
# Lost authenticator and recovery codes: `./speedtest-center disable-2fa` (see operations.md > Two-factor sign-in).

# 4. Install and enable the user service.
mkdir -p ~/.config/systemd/user
install -m 644 deploy/speedtest-center.service ~/.config/systemd/user/speedtest-center.service
systemctl --user daemon-reload
systemctl --user enable --now speedtest-center

# 5. Verify it is up.
systemctl --user status speedtest-center
curl -sS http://127.0.0.1:8080/healthz
```

`deploy/speedtest-center.service` is the unit; `deploy/speedtest-center.nginx.conf`
is the reverse-proxy example. Both are templates — adjust the paths, hostname
and certificates for the target machine.

## Environment

The binaries read configuration from environment variables only; there is no
automatic `.env` loader in the server. The systemd unit loads the environment
file with `EnvironmentFile=`, and `speedtest-migrate` loads the same file from
its current directory, so keep the file at
`~/.local/opt/speedtest-center/.env`.

Every variable is documented with its default in `.env.example`. The center
uses these:

| Variable | Default | Purpose |
| --- | --- | --- |
| `SPEEDTEST_HTTP_ADDR` | `127.0.0.1:8080` | Listen address for the web UI and API. |
| `SPEEDTEST_UI_DIR` | *(empty)* | Serve `ui/` from disk instead of the embedded UI. |
| `SPEEDTEST_DB_HOST` | `127.0.0.1` | PostgreSQL host. |
| `SPEEDTEST_DB_PORT` | `5432` | PostgreSQL port. |
| `SPEEDTEST_DB_NAME` | `speedtest` | Database name. |
| `SPEEDTEST_DB_USER` | `speedtest` | Database user. |
| `SPEEDTEST_DB_PASSWORD` | *(none)* | Required for a real connection; never commit it. |
| `SPEEDTEST_DB_SSLMODE` | `disable` | One of `disable`, `allow`, `prefer`, `require`, `verify-ca`, `verify-full`. |

`SPEEDTEST_DB_PASSWORD` has no default; set it before connecting to a real
database. The `.env` file must be mode `0600` and is git-ignored. Never commit,
print or log a secret, and never hard-code a service address: point
`SPEEDTEST_DB_*` at the shared Workspaces service, not at a specific machine.

systemd's `EnvironmentFile=` parser is stricter than a shell: values are taken
literally with no shell quoting. Keep the file as plain `KEY=value` lines; if a
password contains `#`, spaces or `$`, prefer characters that survive both
systemd and `godotenv` parsing, or feed the value through a drop-in rather than
the shared file.

## Migrations

Run migrations from the release directory so `speedtest-migrate` finds `.env`:

```bash
cd ~/.local/opt/speedtest-center
./speedtest-migrate up        # apply all pending versions; repeated up is a no-op
./speedtest-migrate down      # roll back exactly the latest applied version
```

From a source checkout, `make migrate` and `make migrate-down` are equivalent.
Before migrating a database that matters, verify the target with the Workspaces
safety helper and never run development migrations against the live `speedtest`
database — see [migrations/README.md](../migrations/README.md). Version 1
rollback deletes all application tables, so take a backup first
(see [Backup and restore](#backup-and-restore)).

## Upgrade

A new release ships new binaries and, when the schema changes, new migrations.
Do it in this order, from a clean checkout of the new code:

```bash
# 1. From a clean checkout of the new code, build the new binaries.
go build -o ~/.local/opt/speedtest-center/speedtest-center.new ./cmd/center
go build -o ~/.local/opt/speedtest-center/speedtest-migrate.new ./internal/center/db/cmd/migrate

# 2. Keep the current binary for rollback, then swap in the new ones.
cd ~/.local/opt/speedtest-center
cp speedtest-center speedtest-center.previous
mv speedtest-center.new speedtest-center
mv speedtest-migrate.new speedtest-migrate

# 3. Apply new migrations (no-op when none exist).
./speedtest-migrate up

# 4. Restart the service on the new binary.
systemctl --user restart speedtest-center
systemctl --user status speedtest-center
```

Keep `speedtest-center.previous` until the new version is verified on staging
and released.

## Rollback

Restore the previous binary and roll the schema back one version at a time:

```bash
cd ~/.local/opt/speedtest-center
cp speedtest-center speedtest-center.broken      # preserve the failing build
cp speedtest-center.previous speedtest-center
./speedtest-migrate down                         # one version at a time, backup first
systemctl --user restart speedtest-center
systemctl --user status speedtest-center
```

Rollback is only safe against the same migration versions the code expects; do
not mix a newer binary with an older schema or vice versa. Rehearse a restore
on an isolated database before a production rollback.

## Backup and restore

Back up the database before every migration and on a schedule. The dump goes to
a file outside the release directory; it contains live data and must be kept as
private as the `.env`:

```bash
set -a; . ~/.local/opt/speedtest-center/.env; set +a   # quote values with shell-special characters
export PGPASSWORD="$SPEEDTEST_DB_PASSWORD"
pg_dump \
  --host="$SPEEDTEST_DB_HOST" --port="$SPEEDTEST_DB_PORT" \
  --username="$SPEEDTEST_DB_USER" --dbname="$SPEEDTEST_DB_NAME" \
  --format=custom --file="speedtest-$(date +%F).dump"
unset PGPASSWORD
```

### Rehearse a restore (always first)

A rehearsal restores into an explicitly named scratch database, never the live
one. The commands below take no database name from `.env` on purpose:

```bash
set -a; . ~/.local/opt/speedtest-center/.env; set +a
export PGPASSWORD="$SPEEDTEST_DB_PASSWORD"
RESTORE_DB=speedtest_restore_check          # scratch name, must differ from the live database
[ "$RESTORE_DB" != "$SPEEDTEST_DB_NAME" ] || { echo "refusing: same as live database"; exit 1; }
createdb --host="$SPEEDTEST_DB_HOST" --port="$SPEEDTEST_DB_PORT" --username="$SPEEDTEST_DB_USER" "$RESTORE_DB"
pg_restore --host="$SPEEDTEST_DB_HOST" --port="$SPEEDTEST_DB_PORT" --username="$SPEEDTEST_DB_USER" \
  --dbname="$RESTORE_DB" --no-owner speedtest-YYYY-MM-DD.dump
# inspect, then clean up
dropdb --host="$SPEEDTEST_DB_HOST" --port="$SPEEDTEST_DB_PORT" --username="$SPEEDTEST_DB_USER" "$RESTORE_DB"
unset PGPASSWORD
```

### Restore the live database (operator-authorised, destructive)

Only when the operator has decided to replace live data. Stop the service first
and type the target database name yourself; nothing here defaults to the value
in `.env`:

```bash
systemctl --user stop speedtest-center
set -a; . ~/.local/opt/speedtest-center/.env; set +a
export PGPASSWORD="$SPEEDTEST_DB_PASSWORD"
TARGET_DB=                                   # type the exact database name to replace
[ -n "$TARGET_DB" ] || { echo "set TARGET_DB explicitly"; exit 1; }
pg_restore --host="$SPEEDTEST_DB_HOST" --port="$SPEEDTEST_DB_PORT" --username="$SPEEDTEST_DB_USER" \
  --dbname="$TARGET_DB" --clean --if-exists --no-owner speedtest-YYYY-MM-DD.dump
unset PGPASSWORD
systemctl --user start speedtest-center
```

The password is passed through the `PGPASSWORD` environment variable, never on
the command line, and is cleared afterwards.

## Health checks

The center exposes two loopback endpoints and writes structured JSON logs to the
journal:

| Check | Command | Healthy |
| --- | --- | --- |
| Process | `curl -sS http://127.0.0.1:8080/healthz` | `200` and `{"status":"ok"}` |
| Readiness | `curl -sS http://127.0.0.1:8080/readyz` | `200` and `{"status":"ok","database":"ok"}` |
| Service | `systemctl --user status speedtest-center` | `active (running)` |
| Logs | `journalctl --user -u speedtest-center -f` | no `ERROR` entries |

`/healthz` reports the process is alive. `/readyz` reports the database as
`not configured` until a connection pool is wired in, then `ok`/`unavailable`
based on a live ping. Do not publish `/healthz` and `/readyz` to the whole internet:
the proxy example allows them only from the monitoring source (edit the `allow`
address); everything else gets 403.

The center currently trusts the reverse proxy for the client address (rate
limits and logs use `X-Forwarded-For`/`X-Real-IP`), so only the proxy may reach
`127.0.0.1:8080`: keep `SPEEDTEST_HTTP_ADDR` on loopback and allow no other
route to that port.

## Reverse proxy

The center serves plain HTTP on `SPEEDTEST_HTTP_ADDR` and must not be exposed
directly. `deploy/speedtest-center.nginx.conf` is an nginx example that
terminates TLS (TLS 1.2/1.3 only), sends HSTS and the security headers, limits
request bodies, forwards the host and client address headers, serves the
installer progress stream (server-sent events) unbuffered, restricts the health
endpoints, and redirects plain HTTP to HTTPS. Adapt the `server_name`, certificate paths and upstream
address, then health-check through the proxy:

```bash
curl -sS https://speedtest.example.com/healthz
```

## Extra node containers ("VMs") on the staging host

`deploy/docker/nodes.sh` starts extra agent-only containers that report to the running staging center, to rehearse
many nodes without more machines:

```bash
sg docker -c "deploy/docker/nodes.sh up dock-a dock-b"   # creates the nodes through the owner API and starts one container each
sg docker -c "deploy/docker/nodes.sh list"
sg docker -c "deploy/docker/nodes.sh down dock-a"        # stop; "forget dock-a" also removes the node from the center
```

Each container has its own volume and configuration and knows only its own name and the center address. They
share the host's uplink, so their speeds compete; use real machines (install wizard, `docs/ssh-installer.md`) for
the multi-connection acceptance. Bridge-per-node networking (`NODE_NETWORK=bridge`) needs a firewall that lets
containers reach the host's center port; on the dev VM that path is closed, so the default is host networking.
`scripts/test-netpath-namespace.sh` can run in a throwaway container with `--cap-add NET_ADMIN --cap-add SYS_ADMIN`
and unconfined seccomp/AppArmor (it passed that way on 2026-11-04).

The script finds an existing center node by name before creating one. After
`down`, a later `up` reuses its volume and leaves its schedule untouched. `forget`
deletes the center node and removes its local container and volume; `purge`
removes only the local volume while keeping the center node. For an incomplete
new-node attempt, an idempotency key and the one-time enrollment token are held in a mode-0600 file below
`${XDG_STATE_HOME:-$HOME/.local/state}/speedtest/node-attempts` (directory mode
0700). The file is removed after the container starts or the node is forgotten.
Keep the host state directory persistent until the attempt completes so a retry
can safely resume the same create request.

## APP_KEY rotation maintenance

Changing APP_KEY alone makes stored integration secrets and idempotency responses
unreadable. Follow [the offline rotation procedure](operations.md#rotate-app_key):
stop all center instances, back up and verify the database and OLD key separately,
run `speedtest-center rotate-app-key` with OLD/NEW via protected stdin or environment,
then update the private APP_KEY and restart only after verified commit. No schema
migration or agent re-enrollment is required. Preserve OLD for pre-rotation dumps;
a restore must use the matching key. On failure keep writers stopped until the
transaction outcome is known; do not restart with a guessed key.

Finish queued/running SSH installations before stopping the center for key rotation.
The command refuses active jobs and atomically clears APP_KEY-HMAC installer
idempotency records, reporting their count. Pre-rotation installation retries are
new requests without replay; node/alert ciphertext replays remain valid. See the
operations runbook for backup, failure recovery and retry semantics.

## Production (speedtest.soulevil.com)

Live since 2026-11-06 (release 1.0.0). All values below are names or addresses; no secret is stored in the repository.

| Part | Where |
| --- | --- |
| Command center | container `speedtest-center` on the monitor VM `192.168.10.10`, host network, port `18090`, `--restart unless-stopped` |
| Settings | `/etc/speedtest/center.env` on that VM (root, mode 0600): `SPEEDTEST_DB_*`, `APP_KEY`, `SPEEDTEST_TRUSTED_PROXY_CIDRS=192.168.10.4/32`, `SPEEDTEST_PUBLIC_URL` |
| Database | `speedtest` database and role on the shared PostgreSQL `192.168.10.12`, `sslmode=require` |
| Agent downloads | `/opt/speedtest/release` on the VM (agent binaries, `release.json`), mounted read-only into the container |
| Edge | HAProxy on gatekeeper `192.168.10.4`: vhost `speedtest.soulevil.com` (backend `speedtest-web`, health check `/readyz`, `/metrics` answers 404), Let's Encrypt certificate renewed by certbot with the standard deploy hook |
| Error tracking | Sentry project `speedtest` in organization `jooservices` on `192.168.10.17`; the DSN is in `center.env` (`SPEEDTEST_SENTRY_DSN`, environment `production`) |
| Geolocation | DB-IP Lite City and ASN databases in `/opt/speedtest/geoip` (CC BY 4.0, attribution: IP Geolocation by DB-IP). The center refreshes them itself (built-in monthly updater, see docs/geolocation.md); `prod.sh` gives the container user write access to that directory |
| Logs and metrics | `speedtest-vector` and `speedtest-probe` containers on the monitor VM (files in `/opt/speedtest/observability`, settings in `vector.env`, root 0600) ship the center logs (stream `speedtest_logs`), its `/metrics` and the uptime probe to OpenObserve `192.168.10.18` with the dedicated account `speedtest@jooservices.com` (the XCrawler pattern: application telemetry in OpenObserve, Loki only for system logs). Dashboard `Speedtest overview` is created by `provision_openobserve.py --apply --dashboard-only`; production alerts stay in Prometheus/Alertmanager |
| System monitoring | Prometheus on the monitor VM scrapes `127.0.0.1:18090/metrics` (job `speedtest`) and probes `https://speedtest.soulevil.com/readyz`; 9 system-level rules in `/etc/prometheus/rules/16-speedtest.yml` (center down, edge down, certificate, 5xx, slow requests, database pool, background jobs, retention, notification backlog) go through the existing Alertmanager. The Prometheus, Alertmanager, Loki and Grafana stack watches the system only |
| Application alerts | Speedtest alerts about the product itself (node offline, failed tests, IP change, later speed and bandwidth rules) are Speedtest's own alert rules and are sent by Speedtest straight to Telegram (Settings > Notifications); they never go through Alertmanager |

Deploy and roll back from the repository root with `deploy/prod/prod.sh deploy|rollback|status|logs`. A deploy builds the
release, ships the image, applies pending migrations when the container starts and waits for `/readyz`; the previous image stays
as `speedtest-center:previous`. Back up `APP_KEY` separately from the database dumps: without it stored integration secrets
cannot be decrypted. Rotate it with `speedtest-center rotate-app-key`.
Backups: take `pg_dump -Fc speedtest` of the database before each deploy that migrates (see [Backup and restore](#backup-and-restore)).
