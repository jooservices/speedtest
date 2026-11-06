# Observability

How to see what the product is doing and be told when something is wrong. The pieces: **Sentry** for errors and panics, **metrics**
on `/metrics`, **logs** on stdout, shipped with the rest to **OpenObserve**, and **alerts** with a runbook (see
[operations.md](operations.md)). Each piece is optional and off until configured; none can stop the product from running.

## Error tracking (Sentry)

The center and the agent send unexpected errors and panics to Sentry when `SPEEDTEST_SENTRY_DSN` is set; with it empty nothing is sent
and nothing changes.

| Variable | Meaning |
|---|---|
| `SPEEDTEST_SENTRY_DSN` | the project DSN (treat it as a secret; never log it) |
| `SPEEDTEST_SENTRY_ENVIRONMENT` | environment tag; default `APP_ENV`, then `production` |
| `SPEEDTEST_SENTRY_SAMPLE_RATE` | 0 to 1, default 1 (every error) |

Release is the build version (`make release VERSION=...`, `main.version`), so a new deployment shows up as a new release. Every event
carries the tag `component=center|agent`.

**What is reported:** panics (HTTP handlers, the agent), HTTP answers of 500 or more (grouped by method, route template and status, so a
broken endpoint is one issue, not thousands), failed iterations of the background loops (`loop=alerts|notifications|retention`), and the
agent stopping after a lifecycle failure. Failed speed tests are not reported: they are product data, handled by alert rules.

**What is never sent:** request bodies, query strings, cookies, headers, user identity, client IP addresses, breadcrumbs, host name. Text
in messages is scrubbed of passwords and tokens (`password=...`, `Bearer ...`, node tokens), e-mail addresses and IPv4 addresses
(`internal/shared/errtrack`, unit-tested). Route labels come from a fixed vocabulary, so scanners cannot create new issue groups.

**Set it up:** in Sentry create a project "Speedtest" (platform Go) and copy its DSN into the center's `.env`/service environment and,
if you want agent errors too, into each agent's environment (`SPEEDTEST_SENTRY_DSN`). In Sentry add an alert rule "new issue" to your
channel. The center logs `error tracking is on` (never the DSN) at startup, or `error tracking is off` with the reason.

## Logs and metrics in OpenObserve

`deploy/observability` ships both to OpenObserve with Vector and creates the dashboard and alert rules.

| Piece | File | What it does |
|---|---|---|
| Shipping | `vector.yaml` | reads the JSON log lines of the center, agents and the probe from Docker (swap in the `journald` source for a systemd install) and scrapes `/metrics` every 30 s; logs go to the stream `speedtest_logs` (fields `level`, `msg`, `request_id`, `method`, `path`, `status`, `duration_ms`, `service`, `node`), metrics to Prometheus streams named like the metric. It carries Vector unit tests (`vector test`) |
| Runner | `observe.sh` | `env` writes a template `vector.env` (mode 0600, git-ignored) to fill in, `up` starts Vector, `probe` starts the probe, `status`, `down` |
| Probe | `probe.sh` | once a minute: `/readyz` status and time, days until the TLS certificate expires, free disk %, as one JSON log line |
| Provisioning | `provision_openobserve.py` | idempotent; creates the dashboard "Speedtest overview" (12 panels) and 13 alert rules; dry run by default, `--apply` to write; needs `OPENOBSERVE_USER`/`OPENOBSERVE_PASSWORD` (a dedicated user in production, never printed) |

```bash
sg docker -c "deploy/observability/observe.sh env"      # then fill in deploy/observability/vector.env
sg docker -c "deploy/observability/observe.sh up"
sg docker -c "deploy/observability/observe.sh probe"
python3 deploy/observability/provision_openobserve.py --env-file deploy/observability/vector.env --apply
```

OpenObserve only accepts an alert on a stream that already exists, so run the provisioning again a few minutes after the first data
arrives; it lists the alerts it had to skip.

### Alerts

Thirteen rules, each linked to a runbook entry in [operations.md](operations.md#runbook):

| Alert | Fires when | Runbook |
|---|---|---|
| speedtest-center-down | the probe gets no answer from `/readyz` twice in 3 min | center-down |
| speedtest-readyz-failing | `/readyz` answers but not 200 twice in 5 min | readyz-failing |
| speedtest-5xx-rate | more than 0.05 server errors per second for 5 min | server-errors |
| speedtest-error-burst | 20 or more ERROR log lines in 5 min | server-errors |
| speedtest-latency-p95 | p95 request latency above 1 s for 10 min | slow-requests |
| speedtest-no-results | newest result older than 2 h | no-results |
| speedtest-nodes-offline | any node without a heartbeat for 10 min | nodes-offline |
| speedtest-notifications-failing | notification deliveries failed in 15 min | notifications-stuck |
| speedtest-notifications-stuck | a notification waiting more than 10 min | notifications-stuck |
| speedtest-db-pool-exhausted | pool 90% used for 5 min | database-pool |
| speedtest-job-stale | a background loop has not succeeded for 15 min | background-jobs |
| speedtest-disk-low | under 15% disk free | disk-space |
| speedtest-certificate-expiring | certificate expires in under 14 days | certificate-expiry |

**Where alerts go.** OpenObserve needs a destination for every alert. Give `TELEGRAM_BOT_TOKEN` and `TELEGRAM_CHAT_ID` (or
`SPEEDTEST_ALERT_WEBHOOK_URL`) to the provisioning script and run it again: it creates the destination and points every rule at it. Until
then the rules use the placeholder destination `speedtest-unrouted` (a public address that ignores the call, because OpenObserve refuses
private hosts): they are evaluated and their state shows in OpenObserve, but nothing reaches a person. The script prints a warning in
that case. Sentry issue alerts are configured in Sentry (see above) and are independent of this.
