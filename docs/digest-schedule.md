# Durable daily and weekly digest schedule

`internal/center/report.DigestScheduler` identifies the owner-local periods that
a digest should cover and claims each one durably in PostgreSQL. It does not
render or send a message: the center passes a `DigestEnqueue` callback that
publishes the digest inside the same database transaction that completes the
period. Notification formatting and channel selection stay in
`internal/center/notify` (JOOST-127).

## Due periods

The owner time zone comes from the `timezone` field in the `display` settings
JSON. A browser-only or invalid value falls back to UTC, because the center
cannot read a browser preference. The flat `timezone` setting remains reserved
for agent quiet hours and does not control digest scheduling.

- The daily digest becomes due at 08:00 owner-local time for the preceding local
  day. Before 08:00 that day is still waiting for its run.
- The weekly digest becomes due on Monday at 08:00 owner-local time for the
  preceding Monday-Sunday week. It stays due later in the same week if a center
  was stopped across Monday morning.
- A daylight-saving change makes the preceding day 23 or 25 hours and the
  preceding week 167 or 169 hours. The period envelope comes from
  `report.NewPeriod`, not from a fixed 24-hour or 7-day duration.

`digest_schedule_state` persists each kind's initialization time, time zone and
last materialized local date. The first scheduling decision starts at the latest
period already due, so it does not backfill from project inception. Later runs
materialize missed dates in batches of at most 32 per kind and advance the
watermark in the same transaction. Remaining backlog is picked up by later loop
iterations. A time-zone preference change establishes a new initialization
boundary; already-created periods retain their first-claim time-zone snapshot.

The scheduler checks ready retry rows and materializes every due period after
its watermark, so periods missed while centers are stopped are recovered after
a restart, including a weekly period whose Monday 08:00 boundary arrives while
the loop is waiting. A failed run is retried after five minutes rather than
waiting for the next day.

## Claim, lease and completion

`digest_periods` has one row per `(kind, local_date)`. The primary key is the
period identity, so changing the owner time zone cannot create a second
completed row for the same local date and kind. The first claim stores the IANA
time-zone snapshot; a retry keeps that snapshot, so its period bounds stay
stable.

A claim takes a 15-minute lease and records the attempt count and next retry
time. The lease is renewed while the enqueue callback runs. A failed enqueue
releases the row with a five-minute retry delay and leaves `completed_at` null.
A center that loses its lease cannot complete the period and its enqueue
transaction rolls back, so another center can retry without a duplicate
completion. Successful enqueue and the completion marker commit together.

A late measurement is not a new period: once `completed_at` is set, repeated
scheduler runs skip that row. Late data appears in live reports and the next
period's digest, not as a second digest for the completed period.

## Verification

Focused tests cover the local 08:00 boundary, Monday weeks, both daylight-saving
changes, concurrent centers, a failed enqueue followed by a restart, an expired
lease after a crash, lease loss, and a time-zone change that must keep one
completed period.

```bash
go test ./internal/center/report -run 'TestAC1|TestDigestTimezone' -count=1
go test -race -p 1 -tags integration ./internal/center/report -run 'TestAC1DigestScheduler|TestAC2DigestScheduler|TestAC2ConcurrentDigest' -count=1
```
