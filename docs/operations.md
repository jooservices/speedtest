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
# nightly (cron/systemd timer), environment as for the center
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
covered by the Go load test; it is close to the 5% CPU target, so watch CPU if the
fleet grows beyond ~50 nodes on a 30-minute schedule.

## Failure behaviour

| Failure | Behaviour |
|---|---|
| Center down | Agents keep testing, spool results on disk and upload them when it returns; results are idempotent by id. |
| PostgreSQL down | `/readyz` fails, API calls return problem+json 5xx, the pool reconnects on its own when the database is back. Not rehearsed against the shared server; rehearse before relying on it. |
| Agent offline for hours | The node shows offline (`speedtest_nodes_offline`), the offline alert fires, backlog uploads on return. |
| Clock skew on an agent | Results carry the agent timestamp; keep NTP running on nodes. Large skew shows as results in the future/past on charts. |
| Notification target down | Deliveries retry with backoff and end `failed` (see gauge); the alert itself is still recorded. |
