# Changelog

All notable changes to Speedtest are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/). Changes are added under
`Unreleased` as they merge and moved under a version heading at release time
(see [docs/release.md](docs/release.md)).

## [Unreleased]

### Added
- Monthly ISP and plan report (JOOST-79): per ISP and node, delivered against advertised speed, p95, worst hours, failures, downtime and IP changes; preview and CSV/PDF download on the ISPs page and `GET /api/reports/monthly`; optional e-mail of the previous month on the first of each month (`SPEEDTEST_MONTHLY_REPORT_EMAIL=true`).
- Optional two-factor sign-in with an authenticator app (TOTP) and ten one-time recovery codes, in Settings > Access; the operator command `disable-2fa` is the recovery path.
- Geolocation: ipwho.is, ipapi.co and ipgeolocation.io providers (off by default), consensus mode, and a built-in monthly DB-IP Lite database updater.

## [1.0.0] - 2026-11-06

First production release: command center, agents, web UI, alerts and notifications, audit log, Sentry error tracking,
Prometheus metrics, OpenObserve dashboard and alerts, and the production deploy script (`deploy/prod/prod.sh`).

### Added
- Production deployment of the command center as a container (`deploy/prod`).

### Added
- Advertise published agent releases by platform and show updates for older stable agents.
- Release packaging (`make release`) and the one-line agent installer served by the
  command center at `/install.sh`.
