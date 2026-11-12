# Application notifications

Speedtest sends product notifications through Telegram, e-mail, Slack and generic
webhooks. Application events stay in the application layer; Prometheus,
Alertmanager, Loki and Grafana are not notification transports
([`guides/notification-layers.md`](../guides/notification-layers.md)). The
catalog in `internal/center/catalog` is the source for event keys, defaults,
channels, thresholds and channel templates. PostgreSQL stores owner overrides.

## Event catalog and settings

Settings → Notifications lists each event and lets the owner set its global
enabled state, severity, channels, thresholds, cooldown, repeat interval and
maintenance window. For node-scoped events such as `test.success`, the owner can
also switch delivery off for an individual node; a node switch cannot re-enable
an event that is globally off. Changes to event settings and templates create
audit entries.

| Category | Events |
| --- | --- |
| Availability | `node.offline`, `node.online`, agent update and version events |
| Tests | `test.failed`, `test.recovered`, `test.success`, `test.manual_finished` |
| Quality | download, upload, ping, jitter and loss alerts; `quality.recovered`; `quality.drop_vs_median` |
| Network | `network.ip_changed`, `network.isp_changed`, `network.server_differs` |
| Bandwidth | 80%/100% budget, projected over-budget and widened/paused schedule events |
| Security | new sign-in IP, failed-login burst, password change, API-token creation and 2FA change |
| Platform | delivery escalation, retention completion and completed export |
| Digests | daily, weekly and monthly ISP catalog entries; scheduling belongs to JOOST-79 |

The default quality limits are download below 65% of expected, upload below
40%, ping above 30 ms, jitter above 10 ms and loss above 2%. Each quality alert
uses two consecutive observed measurements by default. Failed or rate-limited
tests do not count as a recovery measurement. Existing owner alert rules retain
their thresholds and scope on upgrade. A later edit to an alert rule takes
precedence over an older event threshold override.

`test.success` is enabled by default and can be disabled globally or per node.
Success and quality messages include the measured download, upload, ping, jitter
and loss, each configured limit, and whether the measurement is within that
limit. A failed-test message includes its classified failure reason. Recovery
messages include the alert onset in the owner's time zone and the elapsed time.
The node-online event reports the offline duration.

Bandwidth budget events use the event's `monthly_gb` threshold and its `percent` threshold (the share of the budget that raises the event; default 80 for the first event and 100 for the second, so any share can be chosen). Set a positive
value in Settings to enable each budget calculation; a zero or missing budget
does not emit a budget event. Usage is derived from successful/degraded result
byte counts per node and engine in the current calendar month. `bandwidth.interval_widened`
is emitted when a node schedule interval increases or the schedule is paused.
Digest scheduling and reports are handled by JOOST-79.

For immediate events, cooldown/repeat settings suppress duplicate delivery for
the configured interval while keeping each event in the event history. Offline
alerts remain tracked in the open-alert list even when delivery is suppressed;
the repeat interval controls reminder deliveries without leaving duplicate open
alerts behind. Critical events bypass quiet hours when the corresponding quiet
hours setting is enabled.

## Templates

The owner can edit one template per event and channel, preview it with sample
data, or reset it to the catalog default. Unknown, malformed and unsupported
placeholders are rejected when saving or previewing. Webhook templates may use
valid JSON object syntax around placeholders.

Common placeholders include `{node}`, `{event}`, `{severity}`, `{value}`,
`{expected}`, `{pct}`, `{threshold}`, `{download}`, `{upload}`, `{ping}`,
`{jitter}`, `{loss}`, their `*_limit` and `*_ok` values, `{server}`, `{isp}`,
`{ip}`, `{old_ip}`, `{city}`, `{since}`, `{duration}`, `{count}`, `{link}`,
`{center}`, `{failure_reason}`, `{engine}`, `{old_interval}`, `{new_interval}`,
`{format}`, and `{severity_icon}`. Each event exposes only its supported tokens.

Channel rendering is specific to the transport:

- Telegram sends HTML; dynamic values are HTML-escaped and include a severity icon.
- E-mail and Slack send plain text.
- Webhooks receive a JSON object. A non-JSON body is wrapped as `{"text":"…"}`.

The template API uses the existing owner session. Write requests require a
same-origin `Origin` and CSRF token.

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/api/notifications/events` | List the effective catalog settings |
| `GET`, `PATCH` | `/api/notifications/events/{key}` | Read or update an event |
| `GET`, `PUT` | `/api/notifications/events/{key}/nodes/{id}` | Read or set a node override |
| `GET`, `PUT` | `/api/notifications/templates/{key}/{channel}` | Read or save a template |
| `POST` | `/api/notifications/templates/{key}/{channel}/previews` | Render sample data |
| `POST` | `/api/notifications/templates/{key}/{channel}/resets` | Remove the override |
| `POST` | `/api/notifications/events/{key}/tests/{channel}` | Queue a sample event delivery |

## Channel setup

Enable a channel in Settings with `notify.<channel>.on`. E-mail `to` is one
mailbox. Telegram `to` is the numeric chat ID. Slack and webhook `to` must be
empty; put the destination URL in the write-only `secret`. Telegram's secret is
the bot token. E-mail's secret is the SMTP password. Credentials and destination
URLs are never returned by the API. Disabled or incomplete channels return 422
when a test delivery is requested.

E-mail can use the shared development Mailpit without authentication. For an
authenticated SMTP server, set the username in `.env` and the password in
Settings. SMTP upgrades to STARTTLS when advertised; Go refuses plaintext SMTP
authentication except on localhost.

The center reads these `.env` values (defaults are in `.env.example`):

| Variable | Default | Purpose |
| --- | --- | --- |
| `SPEEDTEST_SMTP_ADDR` | `127.0.0.1:11025` | SMTP address |
| `SPEEDTEST_SMTP_FROM` | `speedtest@example.com` | Sender mailbox |
| `SPEEDTEST_SMTP_USER` | empty | Optional SMTP username |
| `SPEEDTEST_TELEGRAM_URL` | `https://api.telegram.org` | Telegram API origin; trusted gateway override |

SMTP sends plain UTF-8 text. Telegram uses `sendMessage`; Slack and JSON
webhooks receive `{"text":"…"}`. Redirects are refused, non-2xx HTTP responses
fail, and Telegram also requires `ok:true`. Each attempt has a ten-second
deadline. Logs and API responses contain neither credentials nor raw provider
responses.

## Delivery and operations

When `APP_KEY` enables Settings, the center starts the notification worker. It
drains up to 100 outbox/delivery pairs, then waits one second. Workers may run
concurrently; row locks with `SKIP LOCKED` prevent simultaneous claims. Alert
rules snapshot their selected channels into `alert_notification_outbox`.
Catalog events create an `alert_events` history row and use the same durable
outbox. Disabled or cooldown-suppressed events remain in history without an
outbox delivery. One logical delivery is created per event and channel.

Each retry reads current channel enablement and credentials, so an operator can
repair Settings before retry. Pending deliveries retry after 1, 2, 4, 8, 16, 32,
then 60 minutes (capped); after 24 hours they become terminal `failed` without
another send. Success becomes `sent`. A process crash after provider acceptance
but before the database commit can cause a duplicate message; exactly-once
delivery to an external SMTP/HTTP provider cannot be guaranteed.

- `POST /api/notifications/test/{channel}` queues a channel test and returns 202
  with a `Location` such as `/api/notifications/deliveries/{id}`.
- `GET /api/notifications/deliveries` lists recent deliveries. `page` defaults
  to 1; `size` defaults to 25 and accepts 1–100; optional `event` filters by
  alert-event UUID. Pages are capped at 1,000,000.
- `GET /api/notifications/deliveries/{id}` reads safe delivery metadata. The
  message body, credentials and internal `next_attempt_at` are not exposed.

Poll the returned `Location` modestly, such as once per second. The routes use
the owner rate limiter. Worker storage failures log a fixed, secret-free message;
provider failures are recorded as `notification attempt failed`, and expiry as
`delivery expired after 24 hours`. Inspect the delivery API, correct Settings,
and allow the next retry before expiry. Do not edit consumed outbox rows to retry.

Settings → Notifications → Quiet hours delays non-critical alert deliveries
between the configured times in the owner's time zone. The window may cross
midnight. Events are recorded immediately and the worker checks again every
minute. Test messages ignore quiet hours. “Critical alerts are still delivered”
is on by default. Both quiet-hour endpoints must be set or both left empty; a
delivery expires after 24 hours, so the quiet window should be shorter.

## Migrations and compatibility

Notification schema changes are additive. The running 1.0.0 center can continue
writing results and alerts: new columns have defaults and new tables are not
required by that binary. Existing migration numbers from develop through 20 are
preserved; notification migrations begin at 21. The event-default migration
adds only missing global alert rules and leaves existing owner rules intact.

## Verification

Use the worktree `.env` and its disposable database for migrations and integration
tests. Do not run destructive migration or test commands against the live
`speedtest` database. Target package checks during development; the shared CI
gate owns full integration, E2E and UI-coverage runs.

```bash
go test -race ./internal/center/catalog ./internal/center/notify/... ./internal/center/alerts ./internal/center/server ./internal/center/ownerapi -count=1
go test -tags integration -race -run '<focused notification regression>' ./internal/center/notify ./internal/center/alerts ./internal/center/server ./internal/center/ownerapi -count=1
E2E_ONLY=SC-SET-09 node tests/e2e/main.mjs
```

`SC-SET-09` covers global and per-node event settings, template edit, preview and
reset. `SC-SET-04` covers test delivery. The JOOST-78 scenario extends
`SC-SET-09` to edit and restore the consecutive-test threshold for a quality
event.

## IP change text
The default template of `network.ip_changed` says what changed without opening the dashboard: `Now {ip} (was {old_ip}) in {city}. ISP {isp}.`, so every delivered message carries the new and the old address (JOOST-91 added the same text to the alert event, `Changed from <old> to <new>`).
