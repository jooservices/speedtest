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
