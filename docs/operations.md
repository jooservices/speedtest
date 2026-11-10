# Operations

Runbook for the production center. Deployment steps are in
[deployment.md](deployment.md); this page covers what to watch, how to back up
and what happens when parts fail.

## Security posture

- Every response carries a strict `Content-Security-Policy` (no inline script or
  style elements, only style attributes for chart geometry; `connect-src 'self'`; map tiles only from `*.tile.openstreetmap.org`),
  `X-Content-Type-Options`, `X-Frame-Options: DENY`, `Referrer-Policy`,
  `Permissions-Policy` and `Cross-Origin-Opener-Policy`.
- `Strict-Transport-Security` is sent only when the request is HTTPS (direct TLS
  or `X-Forwarded-Proto: https` from a trusted proxy), so plain-HTTP LAN installs
  are not pinned.
- Terminate TLS in a reverse proxy: see `deploy/speedtest-center.nginx.conf`.
  Outside `APP_ENV=local|test` the center requires HTTPS for sign-in.

## Two-factor sign-in

The owner can turn on an authenticator-app code (TOTP, RFC 6238: SHA-1, 6 digits, 30 seconds, one step of clock skew) in
Settings > Access. It needs `APP_KEY`: the secret is stored AES-GCM encrypted with it and stays hidden after setup.

- **Setup** (`POST /api/auth/2fa/setup`, needs the current password) shows a QR code, the key and an `otpauth://` link once;
  the setting turns on only after a valid code (`POST /api/auth/2fa/enable`), which also returns ten one-time recovery codes
  (shown once, stored as SHA-256 digests).
- **Sign-in**: after the right password the API answers 401 `/problems/two-factor-required`; the UI then asks for the code and
  repeats the request with `code` (an authenticator code or a recovery code). A used code step is never accepted twice and a
  recovery code works once. Wrong codes count against the sign-in rate limit.
- **Turn off** needs the password and a code (`POST /api/auth/2fa/disable`). Turning it on or off is written to the activity log.
- **Lost phone and recovery codes**: on the center host run `[SPEEDTEST_OWNER_EMAIL=you@example.com] speedtest-center disable-2fa`
  (container: `sudo docker exec speedtest-center speedtest-center disable-2fa`). It removes the second factor and signs every
  session out; the password still protects the account. Whoever can run it already controls the host.
- `rotate-app-key` also re-encrypts the stored two-factor secret, so rotating the key does not lock the owner out. A key changed without it makes the secret unreadable: run `disable-2fa`. New-login notifications are part of the notification catalog (JOOST-78).

## Monitoring

`GET /metrics` serves Prometheus text gauges. Access: send
`Authorization: Bearer $SPEEDTEST_METRICS_TOKEN` when that variable is set,
otherwise only a direct loopback peer (not via a proxy) is accepted. The route is
absent if the center has no database.

| Gauge | Alert when |
|---|---|
| `speedtest_nodes_offline` | > 0 for 10 min (no heartbeat for 5 min) |
| `speedtest_last_result_age_seconds` | > 2 × the longest node interval (-1 means no results yet) |
| `speedtest_notification_deliveries_failed` | increases |
| `speedtest_notification_oldest_pending_age_seconds` | > 600 (notifier stuck) |
| `speedtest_notification_outbox_pending` | > 0 for 10 min |
| `speedtest_alerts_open` | informational |

`/healthz` is process liveness; `/readyz` also pings PostgreSQL.

## Backup and restore

```bash
# nightly: the script needs SPEEDTEST_DB_* in its environment. From a systemd timer use the center's env file:
#   [Service]
#   Type=oneshot
#   EnvironmentFile=%h/.local/opt/speedtest-center/.env
#   ExecStart=%h/.local/opt/speedtest-center/backup.sh %h/backups/speedtest
# From cron or a shell: set -a; . ./.env; set +a  (trusted file only), then:
deploy/backup.sh /var/backups/speedtest        # keeps the newest 14 dumps (KEEP=n)
# restore: stop the center first
deploy/restore.sh /var/backups/speedtest/speedtest-<stamp>.dump speedtest
```

`backup.sh` writes a custom-format dump atomically and verifies it can be listed;
the password goes through `PGPASSWORD`, never argv. `restore.sh` is destructive
and refuses to run unless the database name is repeated as the second argument.
Rehearsed on 2026-11-03 with 150,000 results and 10 nodes: backup 1 s (4.1 MB),
restore 3 s, row counts equal before and after. Run a restore into a scratch
database after any schema migration. Secrets are stored encrypted with `APP_KEY`:
back up `APP_KEY` separately (never in the same place as the dumps), or restored
integration secrets cannot be decrypted.

## Statistics analytics

Statistics use the existing results, cached geolocation and alert
tables; this feature adds no schema objects. The events endpoint returns at most
500 newest matching marks and sets `truncated` when more exist. ISP attribution
uses the current cached IP organisation when the test engine did not report an
ISP, so refreshing cached geolocation can change historical attribution. The UI
labels this source and explains that unknown ISP rows are excluded by a filter.

The guarded `TestStatisticsQueriesMeetLatencyTargetOnOneYearDataset` integration
test measures all five analytics queries and event marks against 87,600 synthetic
results, two engines, two ISPs and 600 linked alerts, for both the default
primary-engine/all-ISP query and a selective Ookla/Carrier A query. Five samples
are logged per operation; the empirical p50 and p95 (the maximum of five
samples for p95) must both stay below 300 ms. The test logs
`EXPLAIN (ANALYZE, BUFFERS)` plans for summary and events in both filter cases.
Run it on the disposable worktree/test database only. A rollback is a code
rollback; no database migration is involved.

## Capacity

`make perf` (tests/perf/dashboard.mjs) runs a real center with 50 nodes and about
70,000 results and drives 10 concurrent users through the dashboard's API calls
for 30 s. Result on 2026-11-03: 1,470 requests, p50 16 ms, p95 262 ms, center CPU
18% of one core, no errors; gate is p95 < 300 ms and CPU < 50%. The dashboard
snapshot is shared for 5 s between requests (`dashboard.Cached`), so extra tabs do
not multiply the fleet aggregation. Agent ingestion at 1,000 results/min is
covered by the Go load test; the 5% CPU gate measures the center process during
the paced 60-second workload, excluding startup/shutdown, the generator and PostgreSQL. Linux
`/proc/<pid>/stat` user + system ticks (all process threads) are sampled after
readiness and before shutdown using `getconf CLK_TCK`; the test logs measured
CPU seconds and percentage. This avoids charging startup to ingestion when
other suites compete for CPU, without changing 1,000 committed results/minute
or the 5% threshold. Two full-suite runs on dev-workspaces on 2026-11-03
committed 1,000 results in 60.001/60.002 s with 2.69/2.50 CPU seconds
(4.483%/4.167% of one core). These are development VM measurements, separate
from the staging dashboard benchmark above.

## Failure behaviour

| Failure | Behaviour |
|---|---|
| Center down | Agents keep testing, spool results on disk and upload them when it returns; results are idempotent by id. Rehearsed with a closed/rebound HTTP listener and fresh production handlers: three queued results delivered once in FIFO order, spool empty and PostgreSQL count 3; a second flush delivered nothing. |
| PostgreSQL down | `/readyz` fails, API calls return problem+json 5xx, the pool reconnects on its own when the database is back. Rehearsed on 2026-11-03 through a local TCP fault proxy: existing connections cut and new connections refused; readiness 200 → 503 → 200, authenticated config/commands returned problem+json 5xx, then the same pool reconnected and accepted a result. The shared PostgreSQL service was never restarted; this tests transport loss, not PostgreSQL crash recovery. |
| Agent offline for hours | The node shows offline (`speedtest_nodes_offline`), the offline alert fires, backlog uploads on return. |
| Clock skew on an agent | Measured with −10 and +10 minute timestamps: ingestion accepted both (item status 201), PostgreSQL preserved the exact agent timestamp, and last-result-age was respectively 600 and −600 seconds. Heartbeat last_seen_at used center time in both cases. There is no skew rejection or correction; future results can mask ingestion staleness until center time catches up. Keep NTP running on nodes. |
| Notification target down | Deliveries retry with backoff and end `failed` (see gauge); the alert itself is still recorded. |

### Reproduce the failure rehearsals

Use only a configured, ownership-verified test/worktree database from `.env`,
never the live database. Do not run database suites concurrently. These tests
verify `db.CheckTestDatabase` and the Workspaces ownership marker before reset.

```bash
python3 scripts/geo-test-fixtures.py
go test -race -p 1 -tags integration ./internal/center/server -run 'Test(DatabaseDisconnectRecovery|CenterRestartSpool|AgentClockSkew|MetricsGaugeTransitions)Integration' -count=1 -v
go test -race -p 1 -tags integration ./... -count=1 -v
```

`TestMetricsGaugeTransitionsIntegration` scrapes the production `/metrics`
handler over seeded PostgreSQL rows: offline nodes 0 → 1 → 0, last result age
−1 → 90 → 150 → 0 seconds, failed deliveries 0 → 1 → 0. The collector uses a
controlled clock so exact age assertions do not depend on machine load.
The restart rehearsal composes the real SQLite reporter, HTTP client, center
handlers and PostgreSQL; it complements the JOOST-45 agent harness and the
JOOST-48 browser suite without changing browser flows or the UI.

## Map tiles

The IP & location page uses Leaflet (vendored in `ui/assets/vendor/leaflet`, BSD-2-Clause, no CDN) with
OpenStreetMap tiles. The tile URL is the `map-tiles` meta tag in `ui/index.html`; set `content=""` to draw pins
on a plain background with no outside request, or point it at your own tile server (then allow that host in
`img-src` in `internal/center/server/headers.go`). Respect the OpenStreetMap tile usage policy; a single owner's
traffic is well within it. The demo preview keeps the illustrative drawn map.

## Rotate APP_KEY

Run `center rotate-app-key` offline from the release directory, using its `.env`
for database configuration. **Back up the database first**, verify the dump, and
keep the current APP_KEY separately in protected storage. Stop every center
instance and background writer before rotation, and leave them stopped until
APP_KEY is updated. Agents can keep spooling while the center is unavailable.
Never run a center with the old key after a successful rotation. Finish all queued
or running SSH installation jobs before stopping writers; rotation refuses while
`installer_jobs.status` is `pending` (queued) or `running`.

The command accepts two base64-encoded 32-byte keys (optional `base64:` prefix),
OLD then NEW on separate stdin lines. Prepare the input in a private 0600 file
using a secret manager or protected editor; do not type keys in shell commands,
arguments, history, logs or terminal recordings. Disable shell tracing.

```bash
systemctl --user stop speedtest-center
# Take and verify the database backup as described above before proceeding.
./speedtest-center rotate-app-key < /secure/path/rotation-keys
# On success: update APP_KEY in the private .env to NEW, then remove the input file.
systemctl --user start speedtest-center
systemctl --user status speedtest-center
```

Alternatively inject **both** `OLD_APP_KEY` and `NEW_APP_KEY` through a protected
process environment; both default to empty and stdin is used when both are empty.
A partial environment pair, invalid encoding/length, identical keys, or any argv
is refused. The command never prints keys, plaintext, row identifiers or database
diagnostics. Environment values override `.env`; clear obsolete inherited APP_KEY
before restarting. Do not store rotation variables in the long-running service.

One transaction locks `installer_jobs`, `installer_idempotency`, `settings` and
`api_idempotency`, checks that no installation is active, clears installer HMAC
idempotency records, reseals every encrypted value with a fresh AES-GCM nonce, and decrypts the **stored** replacement using
NEW before commit. Settings use no associated data; idempotency responses retain
their principal/operation/key-hash associated data, including expired records and
alert-rule responses. The `enrollment_key` setting is a SHA-256 hash, so it stays
unchanged, as do plaintext settings, revision/timestamps and other metadata.
Working memory is bounded to 100 records; locks last until commit, so schedule a
maintenance window proportional to stored secret/replay volume.

Installer request hashes use HMAC-SHA256 keyed by APP_KEY and cannot be rekeyed
because SSH credentials are never persisted. Rotation deletes **all**
`installer_idempotency` records, including expired rows, in the same transaction.
These records normally protect retries for 24 hours. A client retry of an
installation request made before rotation is treated as a **new request**, without
replay; it must satisfy normal probe validation. Installation history is retained.
`installer_probe_idempotency` uses unkeyed SHA-256 and stays unchanged. Node and
alert ciphertext replay records are re-encrypted and retain replay behavior.
Reversing rotation cannot restore deleted installer retry records.

Success prints `APP_KEY rotation committed: N values verified; M APP_KEY-HMAC
idempotency records cleared (pre-rotation retries are new requests); update APP_KEY
before restarting the center`. Exit 1 means failure; an error before commit rolls
back every change. If the connection fails during commit, the outcome can be
uncertain: keep writers stopped and verify which key decrypts stored data before
retrying or restoring. Never blindly retry or switch APP_KEY in that situation.
After a confirmed rollback keep OLD. After confirmed success use NEW and verify
readiness, notification delivery and an idempotent node operation. To reverse a
confirmed rotation while writers remain stopped, run the same command with NEW
then OLD and restore APP_KEY to OLD only after it commits. For disaster recovery,
restore the pre-rotation dump **together with OLD**; retain the corresponding key
for older backups even after successful rotation. Never restore a dump with a
mismatched key.

Reproduce automated coverage on an ownership-verified disposable worktree DB:

```bash
go test -race -p 1 -tags integration ./cmd/center -run 'TestRotateAppKey' -count=1 -v
# Three consecutive operator-process and consumer E2E runs:
go test -race -p 1 -tags integration ./cmd/center -run 'TestRotateAppKey(Command|Consumers)EndToEndIntegration$' -count=3 -v
```

`TestRotateAppKeyCommandEndToEndIntegration` builds and runs the real command with
stdin and environment keys, verifies decryption through a fresh process boundary,
reverses rotation and checks wrong-key refusal. `TestRotateAppKeyIntegration`
checks all ciphertext formats, expired records, multi-batch processing and hash
preservation. `TestRotateAppKeyConsumersEndToEndIntegration` checks the real node
idempotency service, notification settings consumer and installer retry semantics
after changing keys. Its installer consumer uses a stopped worker pool to test
persistence/retries without performing an unrelated remote SSH installation.
`TestRotateAppKeyRollbackIntegration` injects corrupted data, write
failure and stored-byte verification failure and asserts the entire snapshot is
unchanged. This operator CLI flow has its own process E2E; it changes no browser
flow in JOOST-45/JOOST-48.

`TestRotateAppKeyInstallerPolicyIntegration` verifies active-job refusal, atomic
HMAC invalidation (including failure rollback), cleared counts and retained history.

## Activity log

Every successful owner change (adding, editing or removing a node, schedules, alert rules and acknowledgements, settings, enrolment key,
test notifications, password change) is recorded in `audit_log` with who (the signed-in e-mail), what (a plain sentence plus an action
code such as `node.update`), the item id and the address it came from. Request bodies, passwords, keys and message contents are never
stored. Read it in Settings > Activity log or with `GET /api/audit?action=node.`. Entries follow the results retention period and are
removed by the same nightly job; with retention set to "keep everything" they are kept. A failure to write an entry is logged and never
blocks the change itself. Sign-ins and sign-outs are not recorded (they carry no change).

## Runbook

Each alert in [observability.md](observability.md#alerts) links here. Commands assume the systemd install; for the Docker staging stack
use `sg docker -c "docker logs speedtest-staging"` and `deploy/docker/stg.sh status`.

### center-down
The probe cannot reach `/readyz`. 1) `systemctl --user status speedtest-center` and `journalctl --user -u speedtest-center -n 100`. 2) Is
the process listening (`ss -ltnp | grep 8080`) and does `curl -sS http://127.0.0.1:8080/healthz` answer? 3) A crash loop with
`invalid configuration` means a bad `.env` edit; `database identity` errors mean the wrong database name. 4) Restart with
`systemctl --user restart speedtest-center`. Agents keep their results and deliver them after the center is back (nothing is lost).

### readyz-failing
The center runs but the database check fails. 1) `curl -sS http://127.0.0.1:8080/readyz` (the reason is in the body). 2) `psql` to the
database with the center's settings: is PostgreSQL up, is the password still valid, is the disk full (see disk-space)? 3) Check
`speedtest_db_pool_*` on the dashboard. The pool reconnects by itself when the database returns.

### server-errors
5xx answers or ERROR lines. 1) Sentry shows the grouped issue with its release; open it first. 2) In OpenObserve logs:
`SELECT * FROM "speedtest_logs" WHERE level='ERROR' ORDER BY _timestamp DESC LIMIT 50`; use `request_id` to follow one request.
3) After a deployment, roll back to the previous binary (docs/deployment.md) if the errors started with it.

### slow-requests
p95 above 1 s. 1) Dashboard: is the database pool saturated, is CPU high? 2) The statistics and dashboard queries are the heaviest;
check the data volume (`SELECT count(*) FROM results`) and the retention period (Settings > Data). 3) `EXPLAIN ANALYZE` the query named
in the slow log line; `make perf` reproduces the load.

### no-results
No node reported for two hours. 1) Nodes page: are all nodes offline (center or network problem) or one (see nodes-offline)? 2) Is the
schedule paused by quiet hours or by every node being paused (Schedules page)? 3) `speedtest_results_ingested_total{status="rejected"}`
rising means agents are sending results the center refuses: read the center's WARN lines.

### nodes-offline
A node stopped sending heartbeats. 1) On the node: `systemctl status speedtest-agent`, `journalctl -u speedtest-agent -n 100`. 2) Can it reach
the center URL (`curl -sS <center>/healthz`)? A changed IP, a firewall or an expired certificate are the usual causes. 3) A rejected
credential shows as `401` in the agent log: re-enrol it from the node's page or run `speedtest-agent enroll --repair`.

### notifications-stuck
Messages are failing or waiting. 1) Settings > Notifications > Send a test for the channel: the answer names the failure class.
2) `curl -sS .../metrics | grep notification` for backlog, failures and the age of the oldest message. 3) Wrong chat id, revoked token
or an unreachable SMTP/webhook host are the usual causes; fix the setting and the held messages are retried by themselves
(they expire after 24 h). Notification quiet hours also hold alert messages on purpose.

### database-pool
The pool is almost fully used. 1) `SPEEDTEST_DB_MAX_CONNS` (default 10): raise it if PostgreSQL has room (`SHOW max_connections`).
2) A slow query holding connections shows in `pg_stat_activity`. 3) Restart is a last resort and only hides the cause.

### background-jobs
A loop (alerts, notifications, retention) has not succeeded for 15 minutes. 1) Sentry has the failure with `loop=` tag; the log line
names the step. 2) Most causes are the database (see readyz-failing). 3) The nightly retention job is checked separately: Settings > Data
shows its last run.

### disk-space
Less than 15% free. 1) `df -h` and `du -sh` of the PostgreSQL data directory, the backups directory and the journal. 2) Lower the
retention period (Settings > Data) and run `VACUUM`; prune old dumps (`KEEP=` in `deploy/backup.sh`); `journalctl --vacuum-size=500M`.

### certificate-expiry
The TLS certificate expires in under 14 days. Renew it (certbot/ACME or your CA), reload nginx (`nginx -t && systemctl reload nginx`),
and check `curl -vI https://<host>/readyz`. The probe's `cert_days` confirms the new date within a minute.

## Per-engine measurement operations

In Node → Settings → Test engines, set each installed engine's interval and enabled
switch, then save. An uninstalled engine explains how to repair installation.
Ookla remains the new-node default at 30 minutes. LibreSpeed starts disabled until
selected, with a 60 minute default. When several nodes share a public IP, increase
their Ookla intervals: the Ookla CLI can rate limit by public IP. Choose one engine
in dashboard, results or statistics before interpreting trends; different engines
are never averaged together. Alert rules without an engine evaluate the primary
engine only.

Migration 00010 keeps the single-row legacy `schedules` table, backfills
`engine_schedules`, and adds `results.engine NOT NULL DEFAULT 'ookla'`. New code
mirrors Ookla writes transactionally; the old binary can still read its schedule
and insert results. Upgrade center writers before configuring additional engines.
On a verified disposable worktree database, `make migrate-down` rolls back 00010;
legacy schedules and result rows survive, while per-engine schedules, capabilities
and result engine labels are removed. Stop new agents before rollback, and use the
old binary afterwards. `make migrate` reapplies and backfills the legacy state.
Rollback deliberately cannot reconstruct removed LibreSpeed schedule/identity
metadata. Production rollback/deployment remains an owner-controlled operation.
