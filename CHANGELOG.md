# Changelog

All notable changes to Speedtest are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/). Changes are added under
`Unreleased` as they merge and moved under a version heading at release time
(see [docs/release.md](docs/release.md)).

## [Unreleased]

### Added
- Monthly ISP and plan report (JOOST-79): per-ISP/per-node metrics, destination-attributed IP changes, an explicit unknown-attribution row, a Unicode PDF and CSV export, a populated UI preview, and optional previous-month e-mail with a renewable lease and stale-claim recovery.
- Bandwidth accounting by result, node, engine, recorded ISP and UTC period, with monthly projections, optional shared budgets, threshold events and interval cost advice.
- Optional two-factor sign-in with an authenticator app (TOTP) and ten one-time recovery codes, in Settings > Access; the operator command `disable-2fa` is the recovery path.
- Geolocation: ipwho.is, ipapi.co and ipgeolocation.io providers (off by default), consensus mode, and a built-in monthly DB-IP Lite database updater.
- Fixed failure classes on test results; Ookla rate limits use capped exponential backoff, and rate-limited tests are counted separately from success rates and ignored by failure alerts.

### Changed
- New nodes, missing agent intervals, and newly enabled engine schedules default to 60 minutes; existing schedules retain their configured values. Docker staging scripts keep their current default until JOOST-89.
- Results and Alerts ISP filters stay page-scoped; rolling ISP trends use in-range hourly samples, and plan edits preserve explicitly chosen expected speeds.

### Fixed
- Installation event streams revalidate the bearer token that opened them, so expiry and revocation stop the stream at its next 10-second check.

## [1.0.0] - 2026-11-06

First production release: command center, agents, web UI, alerts and notifications, audit log, Sentry error tracking,
Prometheus metrics, OpenObserve dashboard and alerts, and the production deploy script (`deploy/prod/prod.sh`).

### Added
- Production deployment of the command center as a container (`deploy/prod`).

### Added
- Advertise published agent releases by platform and show updates for older stable agents.
- Release packaging (`make release`) and the one-line agent installer served by the
  command center at `/install.sh`.
