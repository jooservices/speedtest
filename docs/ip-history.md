# Public-IP observations and change signals

`internal/center/ipwatch` implements the ingestion `ResultHook` and is wired by
`cmd/center`. It changes no UI flow or HTTP response schema; IP history read APIs
remain JOOST-36. Nodes stay independent, even when their addresses alternate.

## Timeline and counters

The authoritative IP timeline is a node's known-IP results ordered by **stored `(ts,id)`**.
PostgreSQL's microsecond timestamp precision and UUID ordering apply, including
when two input timestamps differ only below that precision. Failed results with
a NULL IP are omitted. The node's current IP is the last known IP in this order,
not the last received observation:

```sql
SELECT host(ip) FROM results WHERE node_id = $1
AND ip IS NOT NULL ORDER BY ts DESC, id DESC LIMIT 1;
```

A `node_ip_history` row opens on the first observation (`from_ip IS NULL`) and
on every consecutive IP difference. Its ID is the result ID at that boundary;
its timestamp is the measurement timestamp. Read APIs representing actual changes
should exclude the initial null-from baseline. Failed tests with a known IP
participate. Failed results with a NULL IP remain stored but do not enter the
timeline, counters, geolocation cache, or change-signal outbox. Late known-IP
results insert, split or merge boundaries: only the new position and its
immediate successor can change. No full history scan occurs during ingestion.
The partial covering `results_node_timeline_idx(node_id,ts,id)` bounds neighbor
queries while skipping NULL-IP results.

`node_ip_addresses` stores per-node/per-IP `tests`, `first_seen`, `last_seen`,
`speed_tests`, `down_sum`, and `up_sum`. Results with known IPs count as tests;
failed samples are excluded from speed sums, matching the preview. NULL-IP
failures do not contribute. Compute averages as
`down_sum / NULLIF(speed_tests,0)` and `up_sum / NULLIF(speed_tests,0)`; null means
no usable speed, zero is a real speed. Numeric sums avoid float accumulation
overflow. First/last are min/max measurement timestamps, including late samples.

For a known IP, ingestion locks the node row **before** inserting the IP
placeholder/result. NULL-IP results skip the IP watcher. Duplicate UUID retries
never invoke the hook; conflicting payloads remain 409.
History, counters, geolocation cache and change signals commit with the result,
or all roll back. Successful locations are stored once in global `ip_addresses`
and reused across nodes. The shared address uses `FOR NO KEY UPDATE` so cache
fills serialize without blocking history foreign-key `KEY SHARE` locks.

## Geolocation and failures

The existing `.env.example` GeoLite2 paths and provider settings configure the
cache → offline → fallback chain. No new environment variables are required.
Providers share their rate limiter/breaker across ingestions. The durable cache
uses the result's **transaction**, never a pool connection that could wait on
its own uncommitted address placeholder. Cache hits make no network request.

Missing/unavailable location returns `geo.ErrNotFound`: keep the result and its
IP history, and log a generic info message. Unsuccessful lookups stay uncached
and may retry on later observations under the provider's rate limit/breaker.
Database/cache errors and cancellation fail the hook, so the agent can retry.
No engine payload, credential, provider token or SQL error is logged by ipwatch.
Late observations log `late IP observation reconciled without change signal`
at info level with node/result IDs; they never emit obsolete alerts. A failed
result with a NULL IP is stored without geolocation or derived IP state.

## Alert-engine handoff (JOOST-29)

A **newest** observation emits `ip_changed` only if its IP differs from the
previous newest observation. First observations and same-IP results do not
signal. A late observation repairs history but neither emits nor retracts an
already committed signal, per Claude's JOOST-35 decision (Jira comment 24580).

The signal is an immutable `ip_change_signals` row containing `result_id`,
`node_id`, measurement `ts`, `from_ip`, `to_ip`, and nullable `consumed_at`.
The result UUID is its deduplication key. `NOTIFY ip_changed` carries that UUID
and is delivered only on commit; notifications are wakeups, not durable queues.
The alert engine must drain pending rows on startup and periodically, even
without notifications, in bounded batches, for example:

```sql
SELECT result_id,node_id,ts,host(from_ip),host(to_ip)
FROM ip_change_signals WHERE consumed_at IS NULL
ORDER BY ts,result_id LIMIT 100 FOR UPDATE SKIP LOCKED;
```

JOOST-29 processes each row and sets `consumed_at` **in the same transaction**
as alert event persistence; event effects are deduplicated by result UUID. On
failure, the transaction rolls back, leaving the signal pending for retry. The
pending partial index supports draining without rescanning consumed rows. See
[the alert engine contract](alerts.md) for its startup and periodic bounded
drain and its notification outbox to JOOST-30. Signals are owned by their
node/result and deleted with them; retention must drain pending signals before
deleting results. History corrections do not rewrite signals.

Operator checks: `/readyz` checks PostgreSQL; monitor pending signal count/age
once JOOST-29 is running, plus ingestion errors and generic location/late logs.
A growing queue indicates an absent or failing consumer, not failed ingestion.

## Migration and verification

Migration 4 creates the partial covering index, aggregates and outbox. It
backfills counters and rebuilds history from existing results in measurement
order, without generating historical change signals or including failed results
with NULL IPs. On large deployments, rehearse migration time and locking on an
operator-approved database copy before deployment.

Stop ingestion for the migration/binary switch and keep it paused until every
center process runs the new hook: an old binary accepting results after backfill
would leave derived state incomplete. After verifying the worktree/test database
identity, `make migrate` applies it.
`make migrate-down` rolls back **one** version: at version 4 it removes the new
index, counters and outbox while retaining results, cached locations and derived
history in the compatible previous schema. Stop ingestion before a rollback;
otherwise the new binary cannot write its required tables. Rolling forward with
`make migrate` reconstructs counters/history but does not recreate old signals.
Do not roll back while unconsumed signals must be retained; export/handle them
under the operator's backup procedure first. Versions 2 and 1 have their own
rollback behavior, described in [the migration runbook](../migrations/README.md).

```bash
python3 scripts/geo-test-fixtures.py
go test -race ./internal/center/ipwatch ./internal/center/agentapi -cover
# Uses the non-live database named by SPEEDTEST_DB_* (APP_ENV must be local or test).
go test -race -p 1 -tags integration ./... -count=1 -coverprofile=.tmp/coverage-all.out
```

Integration tests compare history/counters to an independent full recomputation,
exercise shuffled arrival, equal timestamps, canonical IPv6, failed tests,
concurrent nodes/replays, transaction rollback, real HTTP executable wiring,
commit notifications and populated migration down/up. No staging deployment or
browser flow is part of this backend-only change.
