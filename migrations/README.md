# PostgreSQL migrations

`00001_initial.sql` creates the initial schema. `00002_node_enrollment_idempotency.sql`
adds encrypted response replay records used by retryable credential issuance.
`00003_nullable_ip_and_server_identity.sql` allows unknown IPs only for failed
results and requires a typed server identity (name or host) for successful ones.
Numeric server IDs remain positive when present but may be unavailable.
`00004_ip_history.sql` adds per-node/address counters, the ordered timeline index
and durable change signals; it backfills history/counters without replaying alerts.
`00005_alert_engine.sql` adds rule revisions, active-event references, result-
keyed event deduplication and the event-keyed notification handoff to JOOST-30.
Its down migration preserves results, cached locations and derived history; see
[IP history rollback and consumer rules](../docs/ip-history.md).
`00007_ssh_installations.sql` adds short-lived host-key probes, installation job
state and replayable progress events, with hashed idempotency keys and an active
installation limit per node. It stores no SSH credentials or enrollment tokens.
`00010_engine_schedules.sql` adds per-engine schedules and result identity;
existing nodes and results backfill to Ookla. `00011_alert_event_payload.sql`
adds defaulted event keys and placeholder payloads to alert history, and
`00012_notification_events.sql` adds owner overrides for event settings,
per-node event enablement and templates. The notification versions are
additive; the 1.0.0 binary continues to use its existing tables and columns.

`00006_notification_deliveries.sql` adds durable per-event/channel attempts and retry scheduling.
SQL files are embedded by
`migrations.Files`; the migration binary needs no source directory at runtime.
Goose records applied versions in `goose_db_version`. Add a new numbered file
with `-- +goose Up` and `-- +goose Down` sections for each schema change; never
rewrite a version that has been released. Each version runs in a transaction.
A PostgreSQL session advisory lock serializes migration processes.

## Run and roll back

Configure the `SPEEDTEST_DB_*` variables documented in `.env.example` in a
private, ignored `.env` in the current directory. Migration commands require
that file; existing process environment values take precedence. Neither
commands nor errors print connection strings, SQL or credentials.

Before **every** database migration, seed or integration test, verify the target
using the Workspaces safety helper:

```bash
/home/joos/Workspaces/scripts/docker/shared check-project speedtest --env-file "$PWD/.env" --mode "wt:<id>"
```

Replace `<id>` with the worktree's own ID, or `--mode test` for `speedtest_test`.
Do not proceed if validation fails. On the Dev Workspaces VM only, Claude's
JOOST-10 unblock comment 24165 authorizes manual verification because this
helper targets the retired Docker stack: verify the configured name is
`speedtest_test` or matches `^speedtest_wt[a-z0-9]{1,12}$`, then check the
connected database's marker:

```sql
SELECT current_database(), shobj_description(oid, 'pg_database')
FROM pg_database WHERE datname = current_database();
-- Expected marker: workspaces-project:speedtest
```

Never run development migrations against the live `speedtest` database.
Provisioning and deploying live databases belong to the operator/deploy flow.
The migration library itself supports production databases; it does not encode
a development machine's service address or database name.

```bash
make migrate       # apply all pending versions; repeated up is a no-op
# Verify the database again before rolling back.
make migrate-down  # roll back exactly the latest applied version
```

Version 1 rollback **deletes all application tables and their data**. Before an
operator-authorized production rollback, take a backup and rehearse restoring
it on an isolated database. Rollback uses dependency order without `CASCADE`;
an unexpected external dependency fails and rolls back the whole transaction.
A failed migration leaves the schema and version unchanged. Resolve the cause
before retrying; do not edit the version journal to mark a failed migration as
applied. At version zero, down returns a safe error. Goose's journal remains so
that a subsequent up can reapply version 1.

## Storage contract

| Table | Purpose |
| --- | --- |
| `users` | UUID identity, case-insensitive unique email, password hash |
| `sessions` | Hashed opaque token and CSRF token, optional user for anonymous bootstrap, expiry |
| `nodes` | Independent node settings, expected speeds, binding, revision and heartbeat |
| `node_credentials` | Hashed single-use enrollment tokens and bearer credentials, expiration/revocation |
| `results` | Client UUID, normalized nullable measurements, original engine JSON and metadata |
| `ip_addresses` | IPv4/IPv6 host address and cached geolocation JSON |
| `node_ip_history` | Measurement-ordered address boundaries per node (initial row has null from_ip) |
| `node_ip_addresses` | Per-node/address counts, first/last measurement time and speed sums |
| `ip_change_signals` | Durable newest-only IP-change handoff to the alert engine |
| `schedules` | One interval/offset configuration per node |
| `alert_rules` | Threshold, selected nodes, channels and severity (`info`, `warn`, `crit`) |
| `alert_events` | Alert history, resolution and acknowledgement |
| `alert_state` | Per-rule/per-node evaluation state |
| `alert_notification_outbox` | One event-keyed, channel-snapshotted handoff to JOOST-30 |
| `maintenance_jobs` | Status of owner-started maintenance work (bulk alert acknowledgement, later IP refresh) polled at `/api/jobs/{id}` |
| `settings` | Public JSON or encrypted secret bytes, revision |
| `api_idempotency` | Hashed request identity and encrypted one-time issuance responses, expiring after 24 hours |
| `agent_commands` | Per-node command lifecycle and progress |
| `installer_probes` | Expiring unauthenticated SSH host-key observations; one use per install |
| `installer_probe_idempotency` | Probe replay identity and request digest, expiring after 24 hours |
| `installer_jobs` | Owner-scoped installation state; at most one active job per node |
| `installer_events` | Safe, replayable plain-language progress events |
| `installer_idempotency` | HMAC-protected install request digest and job link, expiring after 24 hours |
| `notification_event_settings` | Owner overrides for event enabled state, severity and channels |
| `notification_event_node_settings` | Per-node enable overrides for node-scoped events |
| `notification_templates` | Per-event, per-channel owner template overrides |

All IDs are UUIDs. A result ID has **no server default**, so retries cannot
accidentally obtain another identity. The primary key rejects duplicate UUIDs;
the ingestion API must compare payloads and return duplicate/conflict outcomes.
That query/ingestion behavior is outside this ticket.

Every timestamp, including the goose journal, is `timestamptz`. `db.Open` sets
UTC on every pool connection; PostgreSQL stores instants and accepts explicit
offsets. Mbps fields are `double precision`, latency is milliseconds, loss is
percent, duration is seconds and byte counts are `bigint`. Failed measurements
can remain SQL NULL, preserving the difference from a real zero. `raw` retains
the original JSON. Successful/degraded results require down/up/ping/server ID;
failed results require an error. Nonfinite speeds/latencies are rejected.

`results(node_id, ts)` supports per-node ranges and `results(ts)` supports fleet
ranges/retention. Additional foreign-key/history indexes support cleanup and
bounded queries. Query code and performance endpoints are future tickets.
Nodes own their credentials, results, schedules, history, alerts and commands;
deleting a node cascades its owned rows. IP cache rows remain reusable.
Deleting an alert rule keeps events with a NULL rule ID. Command result links
must belong to the same node and are cleared when that result is deleted.

No plaintext secret column is provided. Authentication code must hash passwords
and token values before insertion; settings code must encrypt recoverable
secrets using the application's key before storing `secret_ciphertext`. SQL
cannot prove that supplied hash/ciphertext bytes were cryptographically derived.
Settings public JSON and secret bytes are mutually exclusive per key. Selected
node UUID arrays in alert rules are deliberately a configuration value; the
future API validates existence/uniqueness, as well as channel uniqueness and
other cross-field rules. No WAN, peer-node or load-balancer relationship exists.

`server_id IS NULL` means automatic server selection in node configuration;
`interval`/`offset` wire fields map to `schedules.interval_minutes` and
`offset_minutes`. Node APIs join schedules; they must not duplicate schedule
state in nodes. Global settings and quiet hours are keyed JSON values rather
than schema columns; defaults are applied by the settings implementation.

## Verification

No local database server or runtime container is started. After the safety
check, run against the worktree/test database named in `.env`:

```bash
make test-db
go test -race -p 1 -tags integration ./... -count=1 -coverprofile=.tmp/coverage-db.out
go tool cover -func=.tmp/coverage-db.out
make ci
```

Create `.tmp/` first for the coverage artifact. Integration tests fail closed
without `.env`, a permitted database name and the server-side ownership marker.
Each suite resets the project tables and goose journal after validating the
database name and ownership marker, and leaves the schema empty on cleanup.
The geo suite applies its own schema before testing. These are destructive
fixtures: use only a disposable test/worktree database. Always use `-p 1` and
`-count=1`; never run separate integration commands or parallel tests against
the same database concurrently. Migration suites apply version 1, insert
synthetic fixtures across the application tables, roll back populated data, reapply
concurrently and clean up.
They verify UUID uniqueness, foreign keys, bounds, nulls, IPv6, UTC precision,
indexes, cancellation and transactional failure on an external view dependency.
Never point this destructive rehearsal at data you need to keep.

`pgx/v5` is required by the ticket; goose supplies versioning/locking/atomicity;
`godotenv` parses `.env` without fragile shell interpolation. All are pinned in
`go.mod`/`go.sum`. Their transitive dependencies do not add database servers or
runtime services.

This migration ticket implemented no HTTP endpoint, agent runtime or UI flow.
Owner authentication is implemented separately in `internal/center/auth` and
`internal/center/server`; remaining browser E2E, real-agent staging soak and
screenshots belong to their implementation/QA tickets. No deployment is
performed by database migrations.

## Version 3 rollback

Upgrade the center before starting agents that can send the new nullable fields.
The migration retains existing successful rows: an older row without a named
server receives a display name derived from its existing numeric server ID.
No IP is inferred or fabricated. The IP foreign key still protects known values.

Version 3 down is deliberately lossless: it refuses when rows have null IPs or
successful rows have null numeric server IDs. It never deletes such measurements
or invents replacement values. Before an authorized downgrade, stop agents and
retain/export incompatible rows through the operator's backup process. On the
verified disposable test database, the integration suite removes only its own
fixtures to demonstrate down/up with compatible populated data. The refused down
is transactional: data and the migration version stay unchanged. After resolving
compatibility, run `make migrate-down`; reapply with `make migrate`.

## Version 6 rollback

Stop the center's notification workers before an authorized rollback. Verify the
non-live target as above, then `make migrate-down` rolls back version 6 only.
This deletes delivery history and retry state; alert events, the outbox and
Settings remain. Consumed outbox rows remain consumed, so reapplying version 6
with `make migrate` does not resend previously consumed events. Preserve delivery
history through the operator backup process when needed. On a disposable test
DB, `TestNotificationMigrationRollback` rehearses populated down/up and verifies
that events and consumed markers survive without replaying old deliveries.
