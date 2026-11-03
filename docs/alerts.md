# Alert rules and event lifecycle

The alert engine is part of the command center. It evaluates each newly stored
result in the same PostgreSQL transaction as ingestion, so a failed evaluation
rolls back both the result and its derived alert state. Duplicate result UUIDs
do not run the hook again.

## Rules and thresholds

The owner API exposes `GET`/`POST /api/alerts/rules` and `GET`/`PUT`/`DELETE
/api/alerts/rules/{id}`. Reads require a signed-in session. Writes also require
same-origin and CSRF checks. Rule creation requires an `Idempotency-Key`; the
same principal, key and normalized body replay the original response for 24
hours. Rule updates and deletes require the current strong `If-Match` ETag.

Numeric measurements are compared with the configured operator and threshold:

- `down_pct` and `up_pct` are the measured Mbps divided by that node's own
  expected Mbps, multiplied by 100. A zero expected value or missing measurement
  cannot match.
- `ping`, `jitter` and `loss` use milliseconds or percent as stored.
- `failed` matches a failed result; `forTests` is the required consecutive count.
- `ip_changed` is a one-shot event for a new newest IP observation. Its required
  form is `op="="`, `value=1`, `forTests=1`.
- `offline` is checked by a periodic job. A node is offline after three enabled
  schedule intervals without a heartbeat; its required rule form is also
  `op="="`, `value=1`, `forTests=1`.

For streak rules, every matching result advances the persisted `alert_state`
row. The event opens on the Nth consecutive match. The first non-match resets
the streak and resolves its active event. The open event ID is stored with the
streak, so a center restart does not create a second event. A rule replacement
resets its streak and resolves any active event before applying the new
configuration. Deleting a rule preserves its history with `ruleId=null`.

`GET /api/alerts/events` returns newest first. `state=open` (the default) means
unacknowledged activity and includes resolved events; `state=all` also includes
acknowledged events. `severity` filters the importance (`info`, `warn`, `crit`),
and `page`/`size` provide pagination. `PUT /api/alerts/events/{id}/ack` accepts
`{"acked":true}` to acknowledge or `{"acked":false}` to reopen attention. This
does not change the underlying rule streak. Event writes also require the
current `If-Match` ETag.

## IP changes and notification handoff

`ipwatch` records newest-only IP changes in `ip_change_signals`. The alert
engine consumes each signal while writing its events, then marks the signal
consumed in the same transaction. Startup and the 15-second background loop
drain pending signals in batches of 100, so a restart or failed transaction
leaves work available for retry. Late IP observations continue to repair
history without creating alerts, as specified in [IP history](ip-history.md).

Each created alert event inserts one `alert_notification_outbox` row keyed by
`event_id` in the same transaction. The row snapshots the selected channels.
JOOST-30 claims pending rows and creates its channel delivery records in its
own transaction before marking a handoff consumed. This is the durable,
deduplicated boundary between deciding that an alert exists and sending it;
external delivery retries belong to JOOST-30.

The offline check runs immediately at startup and every 15 seconds. It uses the
node's last heartbeat, or creation time if the node has never checked in, and
the node's own schedule. Disabled nodes or schedules are not reported offline;
an existing offline event resolves when the node returns or is disabled.

Migration 5 adds rule revisions, active event references, trigger-result
deduplication and the notification outbox. `make migrate-down` removes those
additions while retaining the pre-existing rules, events and streak state. Do
not roll it back while alert outbox rows still need JOOST-30 to consume them.

## Acknowledge all and jobs

`POST /api/alerts/acknowledgements` (CSRF, same origin and an `Idempotency-Key` required) acknowledges every event that is unacknowledged at that moment and answers `202` with `Location: /api/jobs/{id}`. Events that arrive afterwards are untouched. The work is one UPDATE inside a transaction together with its `maintenance_jobs` row, so the job is already `succeeded` when it is returned; a failure records `failed` with a short message (never the database error) and acknowledges nothing. Repeating the request is harmless and creates another job. `GET /api/jobs/{id}` (session) returns `{id, status, error}` for any recorded job.
