# Application notification catalog

Speedtest owns its product notifications (Telegram, e-mail, Slack, webhook). They
never go through Prometheus, Alertmanager, Loki or Grafana
(`guides/notification-layers.md`). Defaults live in `internal/center/catalog`;
owner overrides and custom templates live in PostgreSQL.

## Event catalog

Every event has: key, category, default on/off, severity, channels, placeholders
and one default body per channel. The Settings → Notifications page lists them
and can turn each event on or off. Node-scoped events (`test.success`) also
accept a per-node enable override.

| Category | Keys (examples) |
| --- | --- |
| Availability | `node.offline`, `node.online`, agent update / version events |
| Tests | `test.failed`, `test.recovered`, `test.success` (on by default), `test.manual_finished` |
| Quality | `quality.download_low`, `quality.upload_low`, `quality.ping_high`, `quality.jitter_high`, `quality.loss_high`, `quality.recovered` |
| Network | `network.ip_changed`, `network.isp_changed`, `network.server_differs` |
| Bandwidth / security / platform / digests | catalogued for Settings; digest scheduling is JOOST-79 |

`quality.drop_vs_median` and `network.server_differs` are marked `engineScoped`
for a future emitter once JOOST-71 lands. Do not edit `internal/center/stats` or
schedules tables from this feature.

Production defaults already match the owner rules: download below 65% of
expected, upload below 40%, ping above 30 ms, jitter above 10 ms, loss above 2%,
quiet hours 23:00–07:00 Asia/Bangkok with critical still delivered.

## Templates

Placeholders use `{name}` only. Unknown names are rejected on save. Shared
tokens include `{node}`, `{value}`, `{expected}`, `{pct}`, `{threshold}`,
`{download}`, `{upload}`, `{ping}`, `{jitter}`, `{loss}`, their `*_limit` /
`*_ok` companions, `{server}`, `{isp}`, `{ip}`, `{old_ip}`, `{city}`, `{since}`,
`{duration}`, `{count}`, `{link}`, `{center}`, plus `{severity_icon}`.

Per-channel formatting:

- Telegram: HTML-escaped values, severity icon
- Email / Slack: plain text
- Webhook: JSON object (`{"text":"..."}` when the body is not already JSON)

API (session + CSRF on writes):

- `GET /api/notifications/events`
- `GET|PATCH /api/notifications/events/{key}`
- `GET|PUT /api/notifications/templates/{key}/{channel}`
- `POST /api/notifications/templates/{key}/{channel}/previews`
- `POST /api/notifications/templates/{key}/{channel}/resets`

Template changes write an audit entry. Reset deletes the override so the catalog
default applies again.

## Delivery path

Alert rules still snapshot channels into `alert_notification_outbox`. Catalog
events (`test.success`, recovery, `node.online`) insert an `alert_events` row
with `event_key` + `payload` (already resolved/acked so they do not stay in open
activity) and enqueue the same outbox. The worker renders the effective template
for each delivery channel. Quality and success messages always include ping,
jitter and loss with their configured limits.

Quiet hours, retries and test messages behave as before (see delivery worker in
`internal/center/notify`). Test messages ignore quiet hours.

## Migrations

- `00010_notification_events.sql` — override tables (settings, templates, node settings)
- `00011_alert_event_payload.sql` — additive `event_key` / `payload` on `alert_events`

Both are backward compatible with a running 1.0.0 binary (defaults / unused
columns).

## Verification

```bash
make ci
go test -race ./internal/center/catalog ./internal/center/notify/... ./internal/center/alerts -count=1
go test -tags integration -p 1 -parallel 1 -race ./internal/center/notify ./internal/center/alerts -count=1
node scripts/test-matrix.mjs --write
```

E2E: `SC-SET-09` edits a template, previews it and resets; `SC-SET-04` still
covers a delivered test message on a configured channel.
