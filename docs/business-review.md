# Business review for 1.0.0 (BA)

Reviewed 2026-11-05 against the product goal: one owner watches the real speed of several internet
connections around the clock, is told when something degrades, and can prove it with history.
"Covered" means a shipped feature with automated tests; the numbers refer to docs/test-matrix.md.

## Capability coverage

| Business need | Status | Where |
|---|---|---|
| Add a machine as a node (command, SSH wizard), remove it, pause it | Covered | Nodes page, installer, E2E "a node is added…" |
| Each node knows only its own plan (expected down/up, schedule) | Covered | agent config, docs/architecture.md |
| Scheduled measurements, stagger, quiet hours, concurrency limit, data-usage estimate | Covered | Schedules page, docs/api.md |
| Run a test now and watch it finish | Covered | Run test dialog, command polling |
| Results list, filter, detail, CSV/JSON export | Covered | Results page, E2E |
| Statistics (summary, trend, time of day, week heat map, distribution) | Covered | Statistics page, `/api/stats/*` |
| Public IP and location history, change detection, map | Covered | IP & location page |
| Alerts: slow, failing, jitter/loss/ping, IP changed, node offline; acknowledge | Covered | docs/alerts.md |
| Notifications by e-mail, Telegram, Slack, webhook, with retries and a test button | Covered | docs/notifications.md |
| Settings: units, decimals, time zone, retention, secrets write-only, enrolment key | Covered | Settings page |
| Single owner login, password change (Settings > Access), secure first install | Covered | `create-owner`, sign-in rules, SC-AUTH-02 |
| Backup, restore, monitoring, runbook | Covered | docs/operations.md |

## Gaps found and decisions

1. **Forgotten password had no recovery.** With one owner and no e-mail reset, losing the password locked the
   owner out. Decision: add `center reset-owner-password` (shell and database access required, policy enforced,
   all sessions revoked); documented in docs/deployment.md. Done.
2. **Production had no way to create the owner** (the default account is local/test only). Decision: add
   `center create-owner`. Done.
3. **Agent "update available" cannot be shown unless a release directory is published** (`updateVersion` is
   null by design without `SPEEDTEST_RELEASE_DIR`). Decision: acceptable for 1.0.0; publishing steps are in
   docs/release.md. Follow-up if the owner wants automatic version discovery.
4. **Privacy:** the offline MaxMind database answers first; the ip-api.com fallback sends public IPs of the
   owner's own connections to a third party. Decision: documented here; the fallback can be left unconfigured in
   Settings. No change.
5. **Not in 1.0.0 (consciously deferred):** several users or roles, scheduled summary e-mails, high availability.
   (An activity log of owner changes and quiet hours for notifications were added after the first review.) A single-owner monitoring tool does not need them to be useful;
   each can be added without a data migration of existing results.
6. **Real-world acceptance still open:** the multi-connection setup (four uplinks plus a combined node) was
   rehearsed with Docker node containers and the Linux namespace proof, not with the owner's own machines.
   The owner chose to accept that for now.

7. **No way to change the password from the UI** (only the API had it). Added Settings > Access > Change password
   (current password, new password twice, at least 12 characters, other sessions end), covered by SC-AUTH-02.
8. **Acknowledged alerts disappeared from the UI** (it loaded open alerts only), so "All" and "Reopen" never worked.
   The store now loads all alerts (SC-ALR-03).

## Business rules checked

* Expected speed is per node; percentages of expected drive alerts and comparisons across plans.
* A combined node measures a combination of the others, so the scheduler keeps tests from overlapping.
* Results are never deleted before the retention period (default 365 days); failed tests are kept and shown.
* Secrets are never returned by the API once stored; the enrolment key is shown once.
* All times are stored in UTC and shown in the owner's chosen time zone (legacy zone names such as
  Asia/Saigon are mapped to the canonical names).
