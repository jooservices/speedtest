# Ookla measurement engine

`internal/agent/engine` implements `Engine.Run(ctx, Options) (Result, error)`.
This is a library for the agent scheduler; `cmd/agent` still does not schedule
tests. JOOST-16 adds per-run interface/source-IP options to this engine and the
`internal/agent/netpath` coordinator, which also calls the center's existing
`GET /api/agent/ip` endpoint on the same path. JOOST-17 owns result reporting.

Export the values from `.env` using the application's existing launcher, then:

```go
cfg, err := engine.ConfigFromEnv()
if err != nil { return err }
runner, err := engine.NewOokla(cfg)
if err != nil { return err }
result, err := runner.Run(ctx, engine.Options{ServerID: 12345})
```

For a complete path-bound run, construct the coordinator with the node's
configured binding and center URL:

```go
run, err := netpath.NewRunner(runner, centerURL, credential, netpath.Binding{
    Interface: cfg.NetworkInterface,
    SourceIP:  cfg.SourceIP,
})
if err != nil { return err }
out, err := run.Run(ctx, engine.Options{ServerID: 12345})
```

The binding fields are mutually exclusive. The coordinator validates the
selected local interface/address before invoking the engine, passes
`--interface <name>` or `--ip <address>`, then fetches the center-observed IP on
that same interface/source address. `out.PublicIP` is the center-observed run
address; `out.Measurement.ExternalIP` preserves Ookla's value. Differences are
reported in `out.Warnings`. If the echo request fails, the measurement remains
available with a public-IP detection warning.

`SPEEDTEST_OOKLA_BINARY` defaults to `speedtest` on PATH; point it at the official
Ookla executable installed by the operator, not Python's `speedtest-cli`.
`SPEEDTEST_OOKLA_ARGS` defaults to
`["--accept-license","--accept-gdpr","-f","json"]`. Setting it replaces the
arguments (including those consent flags), primarily for wrappers and test
executables. It must be a JSON string array; `[]` passes no base arguments.
There is no shell expansion. A positive `Options.ServerID` appends
`--server-id <id>`; zero uses automatic server selection, negative is invalid.
`Options.InterfaceName` appends `--interface <name>` and `Options.SourceIP`
appends `--ip <address>`; setting both or supplying an invalid source IP is
rejected. The node coordinator checks that the selected value is present on an
up local interface before each run. See [Linux multi-uplink routing](multi-wan.md)
for the host routing prerequisites.
Do not place credentials in arguments or the executable path.

`SPEEDTEST_OOKLA_TIMEOUT` defaults to `120s`. A positive `Options.Timeout`
overrides that value for one call; the caller's earlier deadline always wins.
Negative timeouts are rejected. Cancellation kills the launched process; a
100 ms pipe-wait bound prevents inherited wrapper pipes delaying return.
Wrappers must exec the actual CLI so cancellation reaches the measurement
process. Calls use independent buffers and argument slices and may run concurrently;
coordinating tests to avoid network overlap belongs to the scheduler.

## Measurements and failures

Bandwidth in bytes/s becomes Mbps (`bandwidth * 8 / 1e6`). Idle ping, jitter,
loaded latency low/iqm/high and packet loss retain ms and percent. Absent/null
measurements stay nil, including packet loss on a successful test; zero is a
valid measurement. Download/upload byte counts are separate. Duration in seconds
is the sum of the CLI's download and upload `elapsed` milliseconds, excluding
setup, server selection and idle-ping phases. Timestamp is normalized to UTC.

Server id/name/location/host, ISP, external IP and result id/URL are retained.
Raw JSON owns a copy of the original bytes and preserves every other engine
value, including loaded jitter, interface details and server country/port.
A final result requires timestamp, positive server ID, download/upload bandwidth
and idle ping. Negative metrics/counts/durations, loss over 100%, malformed,
truncated, multi-document and oversized JSON are invalid results. Unknown fields
are accepted for forward compatibility. Output memory is bounded to 64 KiB per
stream; excess stdout causes `invalid_result`. Stderr is drained but never stored
in Result or copied into its error text. Raw engine JSON can contain identifying
IP/interface information: retain it as result data, not application logs.

On failure, both the Go error and `Result.Error` contain `speedtest: <class>`.
`errors.As(err, *RunError)` exposes the class. Classes are `timeout`, `no_route`,
`server_unavailable`, `license`, `binary_missing`, `canceled`, `invalid_result`,
`execution` and `invalid_options`. Context errors remain detectable with
`errors.Is`. CLI diagnostics are classified by recognizable English phrases;
unrecognized exits are `execution`. Recognizable JSON failures on stdout work
even with exit code zero and remain in Raw; measurements remain nil on failure.

Operators should check binary installation for `binary_missing`, consent flags
for `license`, network/DNS for `no_route`, the pin/server for `server_unavailable`,
and reachability or timeout configuration for `timeout`. `execution` and
`invalid_result` require checking the installed CLI version/output locally;
do not log unredacted diagnostics. These errors are returned to the caller;
this library does not create logs or retry.

## Offline verification

```sh
mkdir -p .tmp
TMPDIR="$PWD/.tmp" GOTMPDIR="$PWD/.tmp" go test -race -coverprofile=.tmp/engine.cover ./internal/agent/engine
go tool cover -func=.tmp/engine.cover
go vet ./...
golangci-lint run
```

No database or live speed test is needed. Tests use JOOST-20's
`testutil.BuildFakeSpeedtest` and `RunFakeSpeedtest`. A test-only adapter verifies
the exact default Ookla flags before forwarding the pin to the fake, because
that fake intentionally supports only its own scenario flags. Direct fake
executions cover custom arguments, zero values, packet loss, crash, concurrency
and cancellation. Testdata provenance is documented in
`internal/agent/engine/testdata/README.md`; legacy fixtures verify absence of
optional metrics and modern fake output verifies loaded latency.

Browser E2E, database migration and deployment checks do not apply to this
package-only change. Real installed Ookla execution on an authorized node and
Claude's review/QA remain release/staging verification; these offline tests do
not claim to measure a live connection.
