# Production and code audit (2026-11-06)

Independent re-audit of the whole product after the 1.0.0 gate. Evidence is from the repository, the test and scan runs, and the
running staging stack. "Gap" means something an operator would miss in production, not a defect in what exists.

## 1. Is it ready for production?

**Functionally and operationally for one owner: yes. For "we find out about any problem ourselves, at any time": not yet, because the
product is not connected to an error tracker, a log platform or an alerting system.** Details in section 3.

| Area | Status | Evidence |
|---|---|---|
| Functionality | Ready | 38 browser scenarios on a real center and real agents; 66 API operations all tested; every page and control exercised |
| Quality gates | Ready | Go coverage 95% (every file >= 85%, no 0% function), UI coverage 96.7% (every file >= 85%), mutation checks 5/5, lint, govulncheck, gitleaks, Sonar gate OK, duplication 1.5% |
| Security | Ready | strict CSP, CSRF + same-origin on owner writes, hashed node credentials, encrypted secrets, rate limits, `/metrics` protected, audit log of changes |
| Data safety | Ready | backup/restore rehearsed (150k results: 1 s / 3 s), failure rehearsals (DB cut, center restart, clock skew), APP_KEY rotation |
| Deployment | Ready | `docs/deployment.md`, nginx TLS example, `create-owner`, release manifest, staging container soaked 18 h (116 real runs, 0 failures) |
| **Observability** | **Gap** | see section 3 |
| High availability | Accepted | one center process; agents spool and replay while it is down (rehearsed) |

## 2. Code structure against the principles

Measured: 48k lines of Go and JS, 44 packages, 57 small interfaces, 16 functions over 70 lines, Sonar duplication 1.5%.

| Principle | Verdict | Notes |
|---|---|---|
| Single responsibility | Good | one package per concern (auth, nodes, agentapi, ownerapi, stats, alerts, notify, geo, jobs, audit...). Exceptions: `server` is big (auth.go 649 lines, nodes.go 573) and `cmd/center/main.go` (540 lines) mixes wiring with the CLI sub-commands |
| Open/closed | Mixed | new engines, geo providers, notification senders plug in behind interfaces. But the route table in `server.NewHandler` is a 120-line `switch`: every new endpoint edits it. Alert metrics and notification channels are fixed sets repeated in Go, SQL constraints, OpenAPI and UI (adding a channel touches about five places) |
| Liskov | Good | stores are small interfaces with fakes used in tests; no type switches on implementations |
| Interface segregation | Good | handlers receive the narrow interface they use (`OwnerJobsStore` has one method); a few wide ones remain (`NodeCredentialService`) |
| Dependency inversion | Good | constructors and `Options`/`services` structs inject stores; `main` is the only place that knows concrete types |
| DRY | Good, with one pattern | duplication 1.5%. Repeated boilerplate: every owner handler repeats principal lookup + rate limit + method check; problem-writing helpers exist in several shapes |
| KISS / YAGNI | Good | no framework, no ORM, no build step in the UI; deferred features are listed, not half-built. Longest functions (NewHandler, settings.Replace, runWithDatabase, Result.Validate) are linear but should be split |
| Design patterns in use | | Strategy (speed test engines, geo providers), Repository/Store, Middleware chain, Decorator (dashboard cache), Outbox (alerts to notifications), Idempotency keys, Observer (result hook chain), state machine (alert streaks), Template for installer scripts |
| Clean code | Good | gofmt/goimports/golangci-lint clean, comments explain why, tests are named by behaviour, scenarios traceable to requirements |
| Extendability | Good with two cheap fixes | add a route registry for `server` and a small `ownerHandler` helper (auth + rate limit + method) to remove the repeated boilerplate; keep channel/metric definitions in one table that Go, SQL and OpenAPI tests are checked against |

Nothing here blocks production; these are refactors to schedule, not defects.

## 3. Observability: what exists and what is missing

| Need | Today | Gap |
|---|---|---|
| Liveness / readiness | `/healthz`, `/readyz` (pings PostgreSQL) | none, but nothing calls them: no uptime monitor |
| Logs | JSON access logs with request id on stdout (journald/Docker); security-relevant events logged without secrets | no central search, no retention policy, no shipping |
| Metrics | `/metrics` (Prometheus text): 9 gauges (offline nodes, last result age, notification backlog and failures, open alerts) | **no HTTP request rate/latency/error counters, no DB pool stats, no job run results, no Go runtime metrics, no per-node heartbeat age**; nothing scrapes it |
| Error tracking | recovered panics and failed background iterations are logged | **no Sentry (or similar): no grouping, no release tracking, no "new issue" notification; the agent has none either** |
| Alerting on the product itself | product alerts are about the owner's connections only | **nothing tells the owner when the center is down, the database is failing, results stopped arriving, or notifications are stuck** (the gauges exist but have no rules or recipients) |
| Tracing | none | optional for a system this size |
| Audit | owner change log (Settings > Activity log) | done |

The workspace already runs the services for this: **Sentry** (self-hosted, Docker stack and the Sentry VM) and **OpenObserve** are shared services. Speedtest does not use either.

### Recommended plan (in this order)

1. **Sentry in center and agent** (`sentry-go`, DSN from `SPEEDTEST_SENTRY_DSN`, off when empty): capture panics, 5xx handlers, failed background loops, agent run failures; release = build version; scrub request bodies, cookies, tokens and IPs by default. About 1 day including tests and a scrub unit test.
2. **Metrics that answer "is it healthy?"**: request counter and latency histogram by route and status, in-flight requests, DB pool (acquired/idle/wait), job last-success timestamps (retention, alerts loop, notification loop), per-node heartbeat age, ingestion rate and rejected results, Go runtime. About 1 day.
3. **Ship telemetry to OpenObserve**: logs by writing the existing JSON to stdout and tailing with Vector or Fluent Bit (no code change), metrics by scraping `/metrics` with the OpenObserve/Prometheus scrape or OTLP, one dashboard (traffic, errors, latency, stale nodes, backlog). About half a day of configuration.
4. **Alert rules on the product** (OpenObserve alerts or Alertmanager) to Telegram/e-mail: center down (no scrape or `/readyz` failing 2 min), 5xx rate, p95 latency, no result for 2x the longest interval, nodes offline, notification failures, database pool exhausted, disk and certificate expiry. Plus an external uptime probe on `/readyz`. About half a day.
5. **Runbook page** linking each alert to its first diagnostic step (extend `docs/operations.md`).

Total about 3 days. With 1 to 4 done, an issue is seen by Sentry within seconds, a degradation is visible on a dashboard, and an outage reaches the owner's phone without anyone looking.

## 4. Other follow-ups (not blockers)

Route registry and owner-handler helper (section 2); split `cmd/center/main.go` into wiring and commands; keep channel and alert-metric definitions in one table; APP_KEY rotation is a command but not scheduled; multi-uplink acceptance on real machines; scheduled summary reports (deferred by the owner).
