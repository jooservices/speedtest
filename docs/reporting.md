# Reliable agent result reporting

`internal/agent/report` provides durable storage and delivery. `agent run` composes
it with scheduling, control and measurement in `internal/agent/runloop`; see
[the lifecycle runbook](agent-lifecycle.md). The demo UI is unchanged.

## Composition

Load `.env` with the agent launcher (or export its variables), call
`SpoolConfigFromEnv`, then `Open(config, logger)`. Close the spool on shutdown.
Create `NewReporter(spool, &Client{BaseURL: centerURL, Credential: nodeCredential})`.
The center URL and node credential come from the node's existing enrollment
configuration; never put a credential in the spool or a URL. Use HTTPS outside
local tests. An optional `Client.HTTP` carries the node's configured transport;
otherwise requests have a 30-second timeout. Redirects are refused, preserving
the destination of the bearer credential.

1. Convert an engine measurement with `FromEngine`. This allocates a UUIDv4
   before any storage or HTTP call; `Options.ID` can retain an existing UUID.
   Failed tests may have an unknown IP (encoded as null); successful tests require
   an IP. `Options.IP` is the authoritative center-observed IP when available;
   the engine address is retained separately as `engineIp`. Measurements preserve null versus zero. Server identity requires a name or
   host for successful tests; the numeric server ID can be absent. Array-shaped
   LibreSpeed raw JSON is preserved under `raw.reports` to keep raw an object.
2. Call `Enqueue(result)` to commit without waiting for connectivity. Retain the
   result UUID on enqueue errors and retry that same result. Identical pending
   UUID/content is a no-op; conflicting content fails.
3. Run `Flush(ctx)` immediately at startup and after enqueuing. It drains the FIFO
   until empty, canceled or a fatal error occurs. One worker per spool; concurrent
   enqueue is safe, including while a request is blocked. `Report` is the
   synchronous enqueue-plus-flush convenience method; retry delivery through
   `Flush`, never by creating a new UUID for the same measurement.

## Durability and delivery

SQLite (`modernc.org/sqlite`, pure Go, no CGO) uses `synchronous=FULL` and DELETE
rollback journaling. Result insertion, retention and quarantine moves are
transactions. A result becomes visible to delivery only after commit. A crash
before acknowledgement commit replays the same persisted UUID and payload.
The filesystem must honor SQLite's locking and fsync requirements; keep the
spool on local durable disk, with one agent owner, rather than a network share.
See [SQLite atomic commit](https://www.sqlite.org/atomiccommit.html).

POST `/api/agent/results` follows `docs/openapi.yaml`: at most 100 results and
8 MiB per batch, accounting for JSON escaping. The complete 207 response must
have exactly one outcome for each input index, with matching UUID and valid
outcome/status pairs, before any result is removed. Accepted (201) and duplicate
(200) are acknowledged; rejected (422 or conflicting-content 409) move to the
bounded `quarantine` table for operator inspection. Logs contain UUID/status,
never payloads, credentials or server-supplied diagnostic text.

This is at-least-once transport. Exactly one stored center row relies on the
JOOST-23 node-scoped UUID constraint and content comparison defined in the API
contract. HTTP fixtures simulate committed ingestion with a lost response and
verify one row after retry. They do not certify a production center deployment.

Transport failures, 429, 5xx and malformed/oversized 207 responses retry with
exponential backoff: 1 second doubling to 5 minutes; uniform jitter draws from
50–100% of each capped delay. Success resets backoff. Cancellation interrupts
waiting. Authentication, malformed batch and other terminal HTTP errors stop
flushing and leave the pending queue intact. Correct the credential/configuration
or server error, then call `Flush` again. `WithBatchSize` and `WithBackoff` allow
the composing lifecycle to supply its policy without changing wire semantics.

## Retention and operations

`.env.example` documents `SPEEDTEST_SPOOL_DIR`, `SPEEDTEST_SPOOL_MAX_RESULTS`
(default 10000), `SPEEDTEST_SPOOL_MAX_BYTES` (67108864) and
`SPEEDTEST_SPOOL_MAX_AGE` (168h). Invalid environment limits fail fast. Caps
bound encoded JSON payload bytes, not exact SQLite disk size. Pending and
quarantine each have these limits; SQLite metadata and reusable free pages add
overhead. Never edit or delete an active SQLite file or journal.

Open, enqueue and each flush attempt enforce limits. `Prune` is also available
for an idle lifecycle timer. Age is measured from the result's UTC measurement
timestamp. Oldest FIFO entries are dropped until limits hold, including a lone
oversized entry, with a structured warning and drop count. Retention is an
explicit loss policy: delivery is guaranteed only while entries remain within
the configured caps. An in-flight acknowledgement for an entry already evicted
by a producer is harmless. Warn-level delivery retries and retention/rejection
logs distinguish network outage, bounded data loss and invalid results.

Inspect pending/quarantine counts with a read-only SQLite client while the
agent is stopped. Do not paste payloads into logs or Jira. Back up the entire
spool only while stopped; restore it to a private directory and restart flushing.
Replay after restoration is safe under the center UUID contract. There is no
PostgreSQL migration in this change. Rolling back the library requires stopping
the reporter and preserving its spool until a compatible version resumes it.

## Verification

```bash
GOFLAGS=-mod=mod go test -race -coverprofile=.tmp/report.cover ./internal/agent/report
GOFLAGS=-mod=mod go test -race ./...
GOFLAGS=-mod=mod go tool cover -func=.tmp/report.cover
```

Use `-mod=mod` when an old ignored local `vendor/` directory is present; the
repository does not track vendor files. Tests use private temporary local
SQLite files and `httptest` HTTP servers; no external database is required.
The outage clock advances six hours without sleeping. Tests cover lost response,
restart, duplicate delivery, FIFO and byte-bounded batches, concurrent producers,
invalid outcomes, size/count/age eviction and storage/HTTP failure paths.

The private `command_state` table retains bounded lifecycle progress. A manual
result and its command association commit in one transaction (`AppendWithState`).
After the entire batch response is validated, the delivery observer commits the
terminal acknowledgement before removing the result. A crash in between replays
the same UUID; terminal acknowledgements remain idempotent. The journal contains
no credentials. Preserve it with the spool when backing up or restoring.
