# Offline agent testing harness

The Go-only helpers in `internal/agent/testutil` need no database, network speed
measurement, Ookla installation or external service. HTTP traffic stays on an
`httptest` loopback server. No dependencies or environment settings are added.

## Fake speedtest executable

`BuildFakeSpeedtest(t testing.TB) string` compiles
`internal/agent/testdata/fake-speedtest` once per test process. It resolves the
module from the helper's source location, so callers can live in any package
directory. Compiled bytes are cached; each caller receives its own executable in
`t.TempDir()`, automatically removed by Go testing. A previous test's cleanup
cannot invalidate a later test's copy. Set `TMPDIR` and `GOTMPDIR` to a worktree
`.tmp` directory when temporary files must stay inside the checkout.

`RunFakeSpeedtest(t, ctx, scenario, overrides...)` returns stdout and the process
error. A cancelled/deadline context kills the fake. It uses the same build helper.

| `--scenario` | Behavior |
| --- | --- |
| `success` (default) | 300/150 Mbps, 8 ms latency, 1 ms jitter |
| `slow` | 8/3 Mbps, 250 ms latency, 60 ms jitter |
| `loss` | 120/100 Mbps, 45 ms latency, 22 ms jitter, 12% packet loss |
| `timeout` | No output; waits until cancellation/termination |
| `crash` | No JSON, diagnostic on stderr, exit code 2 |

Overrides: `--down`, `--up` (decimal Mbps), `--ping`, `--jitter` (ms),
`--loss` (percent) and `--server-id` (integer). Explicit zero overrides work.
Flags are test inputs, not application configuration. Unknown flags/scenarios
exit 2. This fake accepts its own flags; it is not a complete Ookla argument parser.

Successful JSON follows the Ookla `--format=json` result shape:
`type`, RFC3339 UTC `timestamp`, `ping{jitter,latency,low,high}`,
`download`/`upload{bandwidth,bytes,elapsed,latency{iqm,low,high,jitter}}`, `isp`,
`interface{internalIp,name,macAddr,isVpn,externalIp}`,
`server{id,host,port,name,location,country,ip}` and `result{id,url,persisted}`.
Bandwidth is **bytes/second**, bytes are byte counts, elapsed is milliseconds.
`packetLoss` is omitted when unavailable (success/slow); loss and explicit
`--loss` include it, even for zero. Addresses and references are synthetic;
no request is sent to any listed host. The result ID is intentionally fixed for
repeatable fixtures; tests requiring distinct result IDs must allocate them in
their agent result input.

```go
out, err := testutil.RunFakeSpeedtest(t, context.Background(), "slow", "--down", "5")
if err != nil {
    t.Fatal(err)
}
var result struct { Download struct { Bandwidth int64 } }
if err := json.Unmarshal(out, &result); err != nil {
    t.Fatal(err)
}
if result.Download.Bandwidth != 625000 {
    t.Fatalf("unexpected bandwidth: %d", result.Download.Bandwidth)
}
```

## In-process center double

`NewCenter(options...)` exposes `URL` and `Server`; register `t.Cleanup(c.Close)`.
Options `WithEnrollmentToken`, `WithCredential` and `WithNodeID` replace the
synthetic defaults `test-enrollment-token`, `test-node-credential`, `test-node-1`.

- `POST /api/agent/enroll` accepts the configured token and records successful
  `EnrollRequest`s; returns 201 with node ID, bearer credential and minimal config.
- `POST /api/agent/results` requires that bearer, records accepted `ResultInput`s
  and returns 207 with ordered outcomes. Repeated IDs return duplicate/200
  without a second record. Empty or over-100 batches return 422.
- `POST /api/agent/heartbeat` requires that bearer, records `Heartbeat`s and
  returns 200 with the server's RFC3339 receive time.
- `GET /api/agent/config` requires that bearer; `WithConfig(etag, config)` sets
  the effective schedule and quiet hours. Matching `If-None-Match` returns 304.
  `ConfigRequests()` records the conditional headers.
- `GET /api/agent/commands` returns commands set with `WithCommands(...)` or
  `QueueCommand(...)`; polling intentionally redelivers them for deduplication
  tests. `POST /api/agent/commands/{id}/ack` records acknowledgements in `Acks()`.
  The double does not remove acknowledged commands or validate progress.

Malformed JSON returns 400; wrong tokens/credentials return 401. Success uses
the API envelope; errors use an RFC9457 problem. `Enrollments()`, `Results()` and
`Heartbeats()` return ordered slice snapshots protected by a mutex, safe during
concurrent requests. Nested pointers/raw values are read-only to callers.

This is a recording double, not the production center: it does not implement
full schema validation, single-use token expiry, persistence, UUID-content
conflict detection, rate limits or the rest of the agent API.

```go
c := testutil.NewCenter()
t.Cleanup(c.Close)
// Configure the agent client with c.URL, enroll with the synthetic token,
// then send a result and heartbeat using the returned bearer credential.
// After the client's HTTP calls complete:
if len(c.Enrollments()) != 1 || len(c.Results()) != 1 || len(c.Heartbeats()) != 1 {
    t.Fatal("agent did not complete the reporting cycle")
}
```

Run self-tests (Go skips `testdata` in `./...`, so name the fake explicitly):

```sh
go test -cover ./... ./internal/agent/testdata/fake-speedtest
go test -race ./... ./internal/agent/testdata/fake-speedtest
```

Scheduler tests inject `sched.Clock` and a jitter function, advance virtual time,
and assert exact offset slots, quiet windows, live configuration changes,
overrun logging, stale timer rejection and single-run serialization. Controller
HTTP tests cover ETags, offline retention, acknowledgement retries, bounded
unacknowledged history, heartbeat isolation, cancellation and unsafe responses.
`cmd/agent` also runs the real control/scheduler wiring against the center double
and fake executable without a database or external speed measurement:

```sh
go test -race -v ./cmd/agent -run TestRunAgentControlAndEngine
go test -race -cover ./internal/agent/control ./internal/agent/sched
```
