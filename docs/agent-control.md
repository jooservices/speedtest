# Agent control and local scheduling

`agent run` requires a private credential, a valid local node configuration and
`SPEEDTEST_AGENT_CENTER` set to the same origin used for enrollment. The address
has no runtime default; `.env.example` provides a placeholder. Only absolute
HTTP/HTTPS addresses without userinfo, query or fragment are accepted. Production
should use HTTPS. Export the `.env` values before invoking the binary; the
systemd template optionally reads `/etc/speedtest-agent/.env`. Keep that file
private and readable by the service account. Engine variables are documented in
[engine.md](engine.md).

The agent immediately heartbeats and starts a control sync, then repeats both
every 30 seconds. Heartbeats contain `agent` (including version), nonnegative
process uptime in seconds and the last test start in RFC3339 UTC, or null before
the first test. Slow config/command requests cannot block the heartbeat loop.
Every HTTP request has a 10-second timeout and a 1 MiB response bound. Redirects
are refused to keep the credential on the configured origin; it is redacted from
transport/server error messages.

Configuration uses `GET /api/agent/config` and the server's opaque ETag in
`If-None-Match`. A 304 keeps the running configuration; a valid 200 replaces the
schedule without restarting. Invalid responses retain both the prior schedule
and ETag. The wire interval must be 5–1440 whole minutes and offset must be
nonnegative and smaller than the interval. Quiet hours use HH:mm in their
explicit IANA timezone (UTC default), start inclusive and end exclusive; a window
can cross midnight. Equal nonempty endpoints are invalid.

Slots follow `Unix epoch + offset + k * interval`; a random delay under five
seconds is added when waiting for a slot. The offset grid never drifts with test
duration. Quiet hours and current configuration are rechecked when a timer
fires. Tests execute synchronously, so a long test cannot overlap another test.
Missed scheduled slots are skipped and logged rather than queued. Even a test
that finishes with no elapsed clock time consumes its slot exactly once.

Commands are polled oldest first, up to 100 per response, and acknowledged via
`POST /api/agent/commands/{id}/ack`:

| Command | Local action |
| --- | --- |
| `run_now` | Request an immediate manual test, bypassing enabled/quiet hours. One pending request is coalesced while busy; it runs after the current test. |
| `pause` | Disable scheduled tests, preserving interval, offset and quiet hours. |
| `resume` | Re-enable the retained schedule. |
| `update_config` | Fetch unconditionally and apply a validated configuration; failure is acknowledged as failed. |

Successful acknowledgements describe the local control action; `run_now`
acknowledges the request, not a reported measurement. Measurement reporting and
progress/result integration belong to JOOST-17; production control endpoints
belong to JOOST-23. Unsupported commands receive a failed acknowledgement.
Processed command IDs and their terminal acknowledgements are retained in memory
(up to 10,000). Failed acknowledgements are retried on redelivery without
re-execution. Only acknowledged history can be evicted; if all records are
unacknowledged, new commands are deferred at the center until room is available.
Deduplication and remote configuration survive network outages within the running
process, not process restarts. A restart bootstraps the interval from config.yaml
until the first successful center sync.

If the center is unavailable, scheduled tests continue on the last known schedule
and the control loop logs warnings. The service does not exit or discard its
schedule. Check the journal for `heartbeat failed`, `config sync failed`,
`command poll failed` and `command ack failed`; verify the configured origin,
connectivity and credential without logging the credential. `speed test started`,
`speed test finished` and `speed test failed` describe measurements. `speed test
overran its slot; skipped next slot(s)` includes the missed-slot count; `speed
test slot expired; skipped` identifies a delayed timer. Check engine duration and
interval before changing the schedule. SIGTERM cancels HTTP and engine work and
waits for both loops to stop.

No UI, database schema or production endpoint changes are part of this module.
See [offline protocol verification](testing-harness.md) for deterministic tests.
