# Agent lifecycle and recovery

`agent run` starts the scheduler, heartbeat/configuration/command controller and
reporter together. It loads `.env` with exported values taking precedence, then
loads the node's YAML and private credential. All paths, executable settings,
service addresses and retention caps retain the documented `.env.example`
defaults. No database connection is made from the agent.

Local YAML seeds engine (`ookla`), interval (`60m`), binding (default route),
`server_id` (`0`, automatic), and `parallel_connections` (`0`, engine default).
A valid center configuration replaces engine, binding, server, threads and
schedule atomically; a running measurement retains its original snapshot.
Invalid settings do not advance the ETag or partially replace the previous
configuration. The controller keeps its last known configuration while offline.
LibreSpeed supports source-IP binding; its CLI rejects interface binding rather
than silently measuring the default route. Ookla chooses its own connection
count; the thread setting controls LibreSpeed concurrency.

Each measurement resolves its binding anew, executes the configured engine and
asks the center for the IP observed on that same path. This happens after failed
engine execution too. The center's address is authoritative; the engine's address
is retained for cross-checking and mismatch warnings. If the center is offline,
the engine address is used when available. A failed test without an address
carries `ip: null`. If neither source can identify an otherwise measured run, the
pipeline records `speedtest: public_ip_unavailable` as a failed result, preserving
available metrics. Never attribute it to a previous or invented IP.

A fresh UUID is allocated and the result committed to SQLite before any delivery.
Only the reporter sends results, in FIFO order with the existing bounded batch
and backoff policy. It flushes at startup, after enqueue, and continuously retries
transient outages. Retention caps can explicitly evict old results with warnings;
see [reporting](reporting.md). Credential/terminal protocol or storage errors stop
the agent with exit 1, leaving committed pending results for operator recovery.
Structured JSON logs contain lifecycle events, UUIDs and safe error classes,
never credentials or raw measurements. CLI argument errors return 2.

Manual commands carry lifecycle milestones, not fabricated CLI byte progress:
`queued` (0%), `testing` (20%), `reporting` (80%), and `done` (100%). The engine
CLIs supply final JSON only. Terminal status and result ID are acknowledged only
after the center accepts the result (or recognizes a duplicate). Failed tests
then acknowledge `failed` with their safe engine error and stored result ID.
Rejected results acknowledge failure without claiming a stored result ID.
Pause/resume and successful update_config acknowledge `done`/100 immediately.
Acknowledgements are retried on subsequent command polls (normally every 30s).

The spool journals a manual command and its result UUID atomically, so restart
replays the pending result and resumes acknowledgement without another test.
A command interrupted before any result was committed can be retried. Terminal
history is bounded to 10000 IDs; unacknowledged in-flight work is never evicted.
The scheduler accepts at most 100 queued requests and never overlaps measurements.

SIGTERM/SIGINT cancel engine execution and all network requests, join all workers,
then close the spool. The interrupted measurement is committed as a failed result
before closing. The process exits 0 after normal signal shutdown; it does not wait
for network recovery. Restart using the same private spool directory to resume.
Preserve the spool and its command journal together. Do not delete/edit active
SQLite files. Use HTTPS for the center outside local testing.

## Verification

From the worktree, with native Go and lint tools on PATH:

```sh
python3 scripts/geo-test-fixtures.py
TMPDIR="$PWD/.tmp" GOTMPDIR="$PWD/.tmp" go test -race -coverprofile=.tmp/all.cover ./... ./internal/agent/testdata/fake-speedtest
go vet ./...
golangci-lint run
govulncheck ./...
```

Database integration is separate and serial: verify this worktree's `.env`
points at its own non-live database and has the project ownership marker, then
run `go test -race -p 1 -tags integration ./... -count=1`. See the migration
runbook for the authorized manual guard on the Dev Workspaces VM. No UI or browser
flow changes in this ticket. Offline doubles do not certify staging or real-agent
soak; Claude performs review/QA, and deployment remains the owner's decision.

## Multiple measurement engines

The agent heartbeats its installed Ookla and LibreSpeed executables, resolving
`SPEEDTEST_OOKLA_BINARY` and `SPEEDTEST_LIBRESPEED_BINARY` (defaults `speedtest` and
`librespeed-cli`). The center offers one interval/offset/enabled schedule per
engine; newly enabled LibreSpeed defaults to 60 minutes. Config sync replaces
these schedules without restarting. One scheduler worker runs all engines and
manual primary-engine requests in sequence, including coincident slots; overdue
slots expire rather than building an unbounded queue. Every durable result keeps
the engine used even if configuration changes while the subprocess is running.
Missing/uninstalled binaries produce failed measurements; repair installation and
wait for the next capability heartbeat before enabling the engine.
