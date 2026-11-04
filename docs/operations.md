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
