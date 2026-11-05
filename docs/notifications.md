# Notification delivery

The center starts `internal/center/notify.Store.Loop` whenever `APP_KEY` enables
Settings. Apply migration 6 before starting the upgraded center. Each loop drains
at most 100 outbox/delivery pairs, then waits one second. Shutdown cancels network
requests. Workers may run concurrently: `FOR UPDATE SKIP LOCKED` excludes another
worker from the claimed event or delivery.

## Settings and transports

Enable channels in Settings (`notify.<channel>.on`). Email `to` is one mailbox;
Telegram `to` is the numeric chat ID. Slack and generic webhook `to` must be empty:
their URL belongs in the write-only `secret`. Telegram secret is the bot token;
email secret is the SMTP password. The sender-only `NotificationSecret` method
reads AES-GCM ciphertext; it is absent from the public Settings Store interface.
`NotificationConfig` uses the claimed delivery transaction for settings reads,
so concurrent workers cannot exhaust the pool waiting for another connection.
No API returns stored credentials, destination URLs, response bodies or raw
transport errors. Disabled/missing destinations return 422 on a test request.
Email supports unauthenticated shared Mailpit; a password is optional. For an
authenticated SMTP deployment, set the username in `.env` and the password in
Settings. SMTP upgrades to STARTTLS when advertised; plaintext SMTP authentication
is refused by Go's SMTP client except on localhost.

Environment defaults (see `.env.example`):

| Variable | Default | Purpose |
| --- | --- | --- |
| `SPEEDTEST_SMTP_ADDR` | `127.0.0.1:11025` | Shared development Mailpit SMTP; override for deployment |
| `SPEEDTEST_SMTP_FROM` | `speedtest@example.com` | Sender mailbox |
| `SPEEDTEST_SMTP_USER` | empty | Optional SMTP authentication username |
| `SPEEDTEST_TELEGRAM_URL` | `https://api.telegram.org` | Telegram API origin; trusted test gateway override |

SMTP delivers plain UTF-8 text. Telegram uses `sendMessage` (`chat_id`, `text`);
Slack and JSON webhooks receive `{"text":"..."}`. HTTP redirects are refused;
non-2xx responses fail, and Telegram additionally requires `ok:true`. Each attempt
has a 10-second deadline including settings lookup and sending. Messages include
what happened, the node name, the UTC start time and `centerUrl/#/alerts`.

## Durability and retries

The alert engine snapshots selected channels into `alert_notification_outbox`.
Consumption creates one unique delivery per event/channel and sets `consumed_at`
in the same transaction. Any fanout error rolls back both operations. Later rule
edits cannot change the selected channels. Destination credentials and enabled
switches are read from current Settings for each attempt; disabling a channel
prevents sending, while retrying allows an operator to repair its configuration.

Pending deliveries retry after 1, 2, 4, 8, 16, 32, then 60 minutes (capped).
After 24 hours from creation they become terminal `failed` without sending.
Success becomes terminal `sent` and clears the previous safe error. Expiry keeps
the actual attempt count and last-attempt timestamp. A row lock spans one attempt
so concurrent workers cannot send the same delivery simultaneously.

Exactly once applies to logical event/channel creation. SMTP/HTTP cannot commit
atomically with PostgreSQL. A process crash after remote acceptance but before
commit may cause a repeated external message on restart. Tests have null event IDs
and use the same retry path. No new dependency is required.

## API and operations

- `POST /api/notifications/test/{channel}` requires a session, CSRF token and
  same-origin Origin; returns 202 and `Location: /api/notifications/deliveries/{id}`.
- `GET /api/notifications/deliveries` requires a session; supports `page`, `size`
  (1–100, default 25) and optional alert `event` UUID; newest `createdAt,id` first.
  Notification pages are bounded to 1,000,000 to avoid arithmetic overflow.
- `GET /api/notifications/deliveries/{id}` requires a session; missing IDs return 404.

Poll the Location at a modest rate (for example once per second). These routes
use the existing owner rate limiter. Every successful envelope includes `meta`;
list metadata contains the pagination object. Delivery metadata contains no
message payload or credential. The internal `next_attempt_at` is not exposed by
the existing Delivery API schema.

Operators inspect pending/failed deliveries through these APIs. Worker storage
failures emit a fixed, secret-free log message; provider failures are recorded as
`notification attempt failed`, and give-up as `delivery expired after 24 hours`.
Correct Settings and wait for the next attempt before 24h, or queue another test.
Do not edit consumed outbox rows to retry notifications.

## Verification

Use only the worktree `.env` and a verified disposable database. Never run
integration commands concurrently against the same database.

```bash
make ci
go test -race ./... -count=1
go test -tags integration -p 1 -parallel 1 -race ./... -count=1
go test -tags integration -p 1 -parallel 1 -race ./internal/center/notify \
  -count=1 -v -coverprofile=.tmp/notify-coverage.out
go tool cover -func=.tmp/notify-coverage.out
go test -tags integration -p 1 -parallel 1 -race ./cmd/center \
  -run TestNotificationCenterEndToEndIntegration -count=3 -v
```

The real-center end-to-end scenario starts the production composition and checks
owner login, Settings encryption, rejected unconfigured tests, CSRF, 202/Location,
background test delivery, alert outbox delivery, polling and secret-free logs/API.
It complements JOOST-45's future shared harness; it does not depend on that ticket
being implemented. HTTP sender tests use `httptest`; SMTP protocol tests use a
local fake server, while `TestSharedMailpitSMTP` uses only the shared development
Mailpit and explicitly skips if it is unreachable. No real SMTP account or real
Telegram/Slack/webhook destination is used. This ticket changes no UI files;
JOOST-48 owns the shared browser harness.

## Quiet hours for messages

Settings > Notifications > Quiet hours holds alert messages between two times of day (the window may cross midnight and is read in
the owner's time zone, stored with it as `notifyQuietTimezone`). Alerts are still recorded and appear in the dashboard immediately; only
their delivery waits, and the worker looks again every minute until the window ends. Test messages ("Send a test") are never held.
"Critical alerts are still delivered" (default on) lets `crit` alerts through; turn it off to hold those too. A message that has waited
24 hours expires like any other delivery, so avoid windows longer than that. Both ends must be set or both empty.
