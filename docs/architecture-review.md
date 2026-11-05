# Architecture and infrastructure review for 1.0.0 (SA)

Reviewed 2026-11-05. Findings are ordered by importance; "Fixed" items shipped with tests.

## Structure

* Go module with three binaries (center, agent, migrate). `internal/center/*` has one package per concern
  (auth, nodes, agentapi, ownerapi, stats, dashboard, alerts, notify, geo, ipapi, ipwatch, jobs, installer,
  settings, schedules, metrics, server); `internal/agent/*` mirrors it (config, engine, sched, report, control,
  runloop, update, netpath, enroll). HTTP handlers live in `server` and depend on small interfaces, so stores
  are replaceable in tests. No import cycles; `internal/shared/config` is the only shared package.
* The UI is plain HTML/JS with no build step, one global `ST` namespace, a store that talks to the API and
  pages that draw. A strict CSP is possible because there is no inline script. Leaflet is vendored and pinned.
* PostgreSQL schema is versioned with embedded goose migrations (00001–00007), forward-first; run a backup
  before `migrate up` (docs/operations.md).

## Findings

| # | Finding | Severity | Status |
|---|---|---|---|
| 1 | The browser's legacy time zone name (Asia/Saigon) made statistics return 500 because PostgreSQL knows only canonical names | High | Fixed: server maps legacy aliases, test added |
| 2 | Trend timestamps were emitted with a local offset, breaking the "UTC Z" contract | Medium | Fixed |
| 3 | Connection pool hard-coded to 4 | Medium | Fixed: `SPEEDTEST_DB_MAX_CONNS`, default 10, range 1–100 |
| 4 | HTTP server had only a header timeout: a slow sender could hold connections | Medium | Fixed: 60 s read timeout, 2 min idle timeout; no write timeout on purpose (CSV export and installer progress are streams) |
| 5 | Production could not create or recover the owner account | High | Fixed: `create-owner`, `reset-owner-password` |
| 6 | Staging bootstrap is not crash-idempotent between node enrolment and its marker | Low | Accepted (staging only) |
| 7 | Secrets are encrypted with `APP_KEY` (AES-GCM); there is no key rotation procedure | Medium | Accepted for 1.0.0; documented: keep `APP_KEY` safe, rotation would need re-entering secrets |
| 8 | Rate limiters and login throttling live in memory and reset on restart | Low | Accepted (single process, single owner) |
| 9 | Single center process, no failover | Low | Accepted by scope; agents spool results while it is down and replay in order (rehearsed) |
| 10 | The server package is large (many handlers in one package) | Low | Accepted: boundaries are by interface, tests are per handler |

## Security posture

Sessions are HttpOnly, SameSite=Lax cookies, Secure outside local/test; owner writes need same-origin and a
CSRF token; node credentials and tokens are stored hashed; the CSP forbids inline script; `/metrics` is
loopback or bearer-token only; SSH installer stores no credentials; gitleaks, govulncheck, golangci-lint and
Sonar are clean at the release gate.

## Operability

Structured JSON access logs with request ids, `/healthz`, `/readyz`, `/metrics` gauges for stale data and
failed notifications, documented backup/restore (rehearsed), failure rehearsals (database cut, center restart,
clock skew) with measured results in docs/operations.md.

## Open follow-ups (not blockers)

APP_KEY rotation tooling; scheduled summary reports; richer agent version discovery; run the multi-uplink
acceptance on the owner's real machines when convenient.
