# Speed test engines

`internal/agent/engine` implements `Engine.Run(ctx, Options) (Result, error)`.
`cmd/agent` composes this library through the local scheduler, network coordinator
and durable reporter; see [agent lifecycle](agent-lifecycle.md). JOOST-16 adds per-run interface/source-IP options to this engine and the
`internal/agent/netpath` coordinator, which also calls the center's existing
`GET /api/agent/ip` endpoint on the same path. JOOST-17 owns result reporting.

The node's `engine` config key selects `ookla` (the default), `librespeed` or `speedtestcli`.
`config.Config.NewEngine()` constructs the selected runner using `.env` values.
The optional node `parallel_connections` value overrides LibreSpeed's
concurrency default; zero leaves the `.env` value in effect.

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
that same interface/source address, including after engine failure. `out.PublicIP` is the center-observed run
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

If a pinned server returns `server_unavailable`, `no_route` or `timeout` while the
caller is still active, the agent retries once with automatic selection under its
own engine timeout and adds a warning to the result. Caller cancellation and an
expired caller deadline do not start another measurement.

## LibreSpeed CLI engine

Install the official `librespeed-cli` executable on each node and configure
`SPEEDTEST_LIBRESPEED_BINARY` if it is not on `PATH`. The runner invokes it with
`--json --concurrent <count>`. `SPEEDTEST_LIBRESPEED_CONCURRENT` defaults to 4;
an optional node `parallel_connections` value overrides it. A positive
`Options.ServerID` adds `--server <id>`. Set
`SPEEDTEST_LIBRESPEED_SERVER_JSON` to an HTTP or HTTPS server-list URL to use a
custom list; blank uses the CLI's built-in list. `SPEEDTEST_LIBRESPEED_TIMEOUT`
defaults to `120s`. The process is started directly without a shell, and output
is capped at 64 KiB per stream.

The parser accepts the one-report JSON object used by older CLI versions and
the one-element array used by newer versions. Speeds in the JSON are already
Mbps. The engine maps download, upload, ping, jitter, byte counts, server URL,
client IP and available ISP text to `Result`. The CLI does not report a server
ID for automatic selection, location, loaded latency, packet loss, result ID or
measurement duration. A pinned run carries its selected ID; for automatic runs,
the center resolves the server ID only when the reported host matches one stored
catalog entry. Missing or ambiguous matches remain unknown. The raw report is
retained. Telemetry is not enabled by the runner.

Server discovery runs `librespeed-cli --list`. Version 1.0.14 prints one row as
`<id>: <name> (<server URL>) [Sponsor: <sponsor>]`; it does not report distance.
The agent retains the row name, URL host and sponsor without inventing city or
coordinate data.

Select LibreSpeed in a node config with:

```yaml
engine: librespeed
parallel_connections: 4
```

## Offline verification

```sh
mkdir -p .tmp
TMPDIR="$PWD/.tmp" GOTMPDIR="$PWD/.tmp" go test -race -coverprofile=.tmp/engine.cover ./internal/agent/engine
go tool cover -func=.tmp/engine.cover
go vet ./...
golangci-lint run
```

No database or live speed test is needed. Ookla tests use JOOST-20's
`testutil.BuildFakeSpeedtest` and `RunFakeSpeedtest`. LibreSpeed parser tests use
the redacted real `--json` sample and both historical JSON shapes; runner tests
use a controlled fake executable. A test-only adapter verifies
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

## speedtest-cli (`engine: speedtestcli`)

Runs the `speedtest-cli` Python client that Ubuntu ships in its official archive
(`apt install speedtest-cli`), which is useful where the official Ookla executable cannot
be installed. It runs `speedtest-cli --json --secure`, adds `--server <id>` for a pinned
server and `--source <ip>` for a source-address binding; an interface binding is refused
with `invalid_options` (the client cannot bind an interface, so it would silently measure
the default route). Settings: `SPEEDTEST_SPEEDTESTCLI_BINARY` (default `speedtest-cli`) and
`SPEEDTEST_SPEEDTESTCLI_TIMEOUT` (default 120 s).

Mapping: `download` and `upload` (bits per second) become Mbps; `ping` is the latency;
`bytes_received` and `bytes_sent` are the data used; `server.sponsor`/`name`/`country`/`host`/`id`
identify the server (the id may be a string); `client.ip` and `client.isp` are reported as the
engine's external IP and ISP. Jitter, packet loss and latency under load are not reported by
this client and stay unavailable, never zero. Results are labelled by the engine in the raw JSON.

