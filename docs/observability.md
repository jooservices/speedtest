# Observability

How to see what the product is doing and be told when something is wrong. The pieces: **Sentry** for errors and panics, **metrics**
on `/metrics`, **logs** on stdout, shipped with the rest to **OpenObserve**, and **alerts** with a runbook (see
[operations.md](operations.md)). Each piece is optional and off until configured; none can stop the product from running.

## Metrics

`GET /metrics` combines the legacy database gauges and an isolated
`github.com/prometheus/client_golang` registry. It does not register anything in
Prometheus's global registry. Responses retain
`text/plain; version=0.0.4; charset=utf-8` and `Cache-Control: no-store`.
When `SPEEDTEST_METRICS_TOKEN` is set, every scrape requires that bearer token,
including loopback requests. Otherwise only direct loopback peers without
`X-Forwarded-For` may scrape. Do not publish this endpoint unauthenticated.
A collection failure returns 503 without a partial successful scrape.

### HTTP, workers, ingestion and database

| Metric | Meaning | Alert / operator use |
|---|---|---|
| `http_requests_total{method,route,status_class}` | Completed requests, including denied requests and recovered panics; status classes such as `2xx`, `4xx`, `5xx`. | Elevated 5xx ratio; unexpected traffic. |
| `http_request_duration_seconds{method,route}` | Request latency histogram in seconds, default Prometheus buckets (0.005 through 10 seconds plus infinity), with `_bucket`, `_sum`, `_count`. | p95 latency from `histogram_quantile(0.95, sum by (le,route) (rate(http_request_duration_seconds_bucket[5m])))`. |
| `http_requests_in_flight` | Requests currently executing, including the current scrape. | Sustained concurrency or saturation. |
| `speedtest_db_pool_acquired_connections` | pgx pool connections held by callers. | Pool exhaustion when approaching max. |
| `speedtest_db_pool_idle_connections` | Immediately available pool connections. | No spare capacity. |
| `speedtest_db_pool_total_connections` | Current pool size including connections being constructed. | Unexpected pool growth. |
| `speedtest_db_pool_max_connections` | Configured pgx connection limit. | Denominator for pool saturation. |
| `speedtest_db_pool_wait_count_total` | Successful acquisitions that had to wait for an empty pool (`EmptyAcquireCount`). | Rising connection contention. |
| `speedtest_db_pool_wait_duration_seconds_total` | Cumulative time spent waiting on those acquisitions (`EmptyAcquireWaitTime`). | Rising wait time per acquisition. |
| `speedtest_job_runs_total{job,status}` | Completed worker passes, `job=retention\|alerts\|notification`, `status=success\|error`. Retention counts each Run; alerts and notification count each outer loop pass, including idle passes. | Repeated worker errors or missing runs. |
| `speedtest_job_last_success_timestamp_seconds{job}` | Unix seconds of last successful pass, initially 0; errors never advance it. Retention skip (keep forever) is successful. Alert success requires both signal draining and offline evaluation; notification success requires every attempted consume/dispatch call to succeed. | `time() - speedtest_job_last_success_timestamp_seconds` exceeding the job interval plus grace; also alert on 0 after startup grace. |
| `speedtest_node_heartbeat_age_seconds{node}` | Seconds since the node heartbeat, -1 if none. | Missing/stale heartbeat (for example >300 s or -1 after enrollment grace). |
| `speedtest_node_last_result_age_seconds{node}` | Seconds since the node's newest result, -1 if none. | Missing results relative to the node's configured schedule. |
| `speedtest_results_ingested_total{status}` | Individual batch items, `status=accepted\|duplicate\|rejected`. Accepted increments after commit, duplicate after replay acknowledgement; validation/conflict/storage failures count rejected. Envelope/auth/rate-limit failures have no item outcomes and are covered by HTTP counters. Earlier committed items remain counted if a later storage error aborts the batch. | Rising rejects; replay storms; ingestion stopping. |
| `speedtest_build_info{version,go_version}` | Constant 1 with linker-stamped center version (`dev` by default) and Go runtime version. | Unexpected build rollout or stale version. |

HTTP instrumentation is the outermost `server.NewHandler` layer; its optional
`Options.Telemetry` is nil by default. Request ID, audit, security, logging and
panic handling retain their existing order. Streaming uses ResponseController
unwrapping. Completed requests become visible on the next scrape; a scrape sees
itself in flight, but not yet in the completed counter.

### Cardinality and scrape limits

Route labels are a finite allowlist of documented API paths after replacing UUID
and numeric segments with `{id}`. Exact UI assets and `/` share `ui`; health and
metrics retain their paths. All unknown paths, including arbitrary `/api/...`
scanner paths, share `other`. Query strings never enter labels. HTTP methods are
limited to the nine standard methods plus `other`. Status labels are classes,
not error text. No IP, user, query, secret or result ID is a label.

Per-node gauges read PostgreSQL at scrape time with a three-second timeout and
**at most 200 nodes**, ordered by stable node UUID. The first 200 nodes (enabled
and disabled) are exported; additional nodes are intentionally omitted. Deleted
nodes disappear on the next scrape. Ages of missing timestamps are -1; future
timestamps can produce negative ages other than -1. Use fleet-level legacy
gauges for larger fleets or change this explicit limit after capacity review.
Pool counters are cumulative within this process; process restart resets them,
as it does for HTTP, ingestion and worker counters. Worker success timestamps
are process-local and reset to 0 on restart. Instrumentation does not change
scheduling, draining, retries, transactions or notification behavior.

### Preserved fleet gauges

All names and values below remain unchanged; they are read from PostgreSQL at
scrape time and retain their existing missing-value conventions.

| Metric | Meaning | Alert / operator use |
|---|---|---|
| `speedtest_nodes_enabled` | Enabled node count. | Fleet inventory changes. |
| `speedtest_nodes_offline` | Enabled nodes without a heartbeat in five minutes. | Offline nodes. |
| `speedtest_last_result_age_seconds` | Age of newest fleet result, -1 if none. | Ingestion stopped. |
| `speedtest_results_total` | Stored results (gauge, despite the historical name). | Unexpected data loss/growth. |
| `speedtest_notification_outbox_pending` | Alert events awaiting delivery creation. | Stuck outbox. |
| `speedtest_notification_deliveries_pending` | Pending notifications. | Delivery backlog. |
| `speedtest_notification_deliveries_failed` | Deliveries that exhausted retries. | Failed notification transport. |
| `speedtest_notification_oldest_pending_age_seconds` | Oldest pending delivery age, 0 if none. | Delivery latency. |
| `speedtest_alerts_open` | Currently active rule/node alert states. | Unresolved fleet alerts. |

### Runtime and process collectors

The default Go and process collectors export the following families. Summaries
also expose `_sum`/`_count`; build and runtime information is diagnostic rather
than a separate paging alert. These are the default collector families for the
pinned client version on Linux; platform-specific process availability can vary.

| Metric | Meaning | Alert / operator use |
| `go_gc_duration_seconds` | A summary of the wall-time pause (stop-the-world) duration in garbage collection cycles. | Runtime/process saturation or restart diagnosis. |
| `go_gc_gogc_percent` | Heap size target percentage configured by the user, otherwise 100. This value is set by the GOGC environment variable, and the runtime/debug.SetGCPercent function. Sourced from /gc/gogc:percent. | Runtime/process saturation or restart diagnosis. |
| `go_gc_gomemlimit_bytes` | Go runtime memory limit configured by the user, otherwise math.MaxInt64. This value is set by the GOMEMLIMIT environment variable, and the runtime/debug.SetMemoryLimit function. Sourced from /gc/gomemlimit:bytes. | Runtime/process saturation or restart diagnosis. |
| `go_goroutines` | Number of goroutines that currently exist. | Runtime/process saturation or restart diagnosis. |
| `go_info` | Information about the Go environment. | Runtime/process saturation or restart diagnosis. |
| `go_memstats_alloc_bytes` | Number of bytes allocated in heap and currently in use. Equals to /memory/classes/heap/objects:bytes. | Runtime/process saturation or restart diagnosis. |
| `go_memstats_alloc_bytes_total` | Total number of bytes allocated in heap until now, even if released already. Equals to /gc/heap/allocs:bytes. | Runtime/process saturation or restart diagnosis. |
| `go_memstats_buck_hash_sys_bytes` | Number of bytes used by the profiling bucket hash table. Equals to /memory/classes/profiling/buckets:bytes. | Runtime/process saturation or restart diagnosis. |
| `go_memstats_frees_total` | Total number of heap objects frees. Equals to /gc/heap/frees:objects + /gc/heap/tiny/allocs:objects. | Runtime/process saturation or restart diagnosis. |
| `go_memstats_gc_sys_bytes` | Number of bytes used for garbage collection system metadata. Equals to /memory/classes/metadata/other:bytes. | Runtime/process saturation or restart diagnosis. |
| `go_memstats_heap_alloc_bytes` | Number of heap bytes allocated and currently in use, same as go_memstats_alloc_bytes. Equals to /memory/classes/heap/objects:bytes. | Runtime/process saturation or restart diagnosis. |
| `go_memstats_heap_idle_bytes` | Number of heap bytes waiting to be used. Equals to /memory/classes/heap/released:bytes + /memory/classes/heap/free:bytes. | Runtime/process saturation or restart diagnosis. |
| `go_memstats_heap_inuse_bytes` | Number of heap bytes that are in use. Equals to /memory/classes/heap/objects:bytes + /memory/classes/heap/unused:bytes | Runtime/process saturation or restart diagnosis. |
| `go_memstats_heap_objects` | Number of currently allocated objects. Equals to /gc/heap/objects:objects. | Runtime/process saturation or restart diagnosis. |
| `go_memstats_heap_released_bytes` | Number of heap bytes released to OS. Equals to /memory/classes/heap/released:bytes. | Runtime/process saturation or restart diagnosis. |
| `go_memstats_heap_sys_bytes` | Number of heap bytes obtained from system. Equals to /memory/classes/heap/objects:bytes + /memory/classes/heap/unused:bytes + /memory/classes/heap/released:bytes + /memory/classes/heap/free:bytes. | Runtime/process saturation or restart diagnosis. |
| `go_memstats_last_gc_time_seconds` | Number of seconds since 1970 of last garbage collection. | Runtime/process saturation or restart diagnosis. |
| `go_memstats_mallocs_total` | Total number of heap objects allocated, both live and gc-ed. Semantically a counter version for go_memstats_heap_objects gauge. Equals to /gc/heap/allocs:objects + /gc/heap/tiny/allocs:objects. | Runtime/process saturation or restart diagnosis. |
| `go_memstats_mcache_inuse_bytes` | Number of bytes in use by mcache structures. Equals to /memory/classes/metadata/mcache/inuse:bytes. | Runtime/process saturation or restart diagnosis. |
| `go_memstats_mcache_sys_bytes` | Number of bytes used for mcache structures obtained from system. Equals to /memory/classes/metadata/mcache/inuse:bytes + /memory/classes/metadata/mcache/free:bytes. | Runtime/process saturation or restart diagnosis. |
| `go_memstats_mspan_inuse_bytes` | Number of bytes in use by mspan structures. Equals to /memory/classes/metadata/mspan/inuse:bytes. | Runtime/process saturation or restart diagnosis. |
| `go_memstats_mspan_sys_bytes` | Number of bytes used for mspan structures obtained from system. Equals to /memory/classes/metadata/mspan/inuse:bytes + /memory/classes/metadata/mspan/free:bytes. | Runtime/process saturation or restart diagnosis. |
| `go_memstats_next_gc_bytes` | Number of heap bytes when next garbage collection will take place. Equals to /gc/heap/goal:bytes. | Runtime/process saturation or restart diagnosis. |
| `go_memstats_other_sys_bytes` | Number of bytes used for other system allocations. Equals to /memory/classes/other:bytes. | Runtime/process saturation or restart diagnosis. |
| `go_memstats_stack_inuse_bytes` | Number of bytes obtained from system for stack allocator in non-CGO environments. Equals to /memory/classes/heap/stacks:bytes. | Runtime/process saturation or restart diagnosis. |
| `go_memstats_stack_sys_bytes` | Number of bytes obtained from system for stack allocator. Equals to /memory/classes/heap/stacks:bytes + /memory/classes/os-stacks:bytes. | Runtime/process saturation or restart diagnosis. |
| `go_memstats_sys_bytes` | Number of bytes obtained from system. Equals to /memory/classes/total:byte. | Runtime/process saturation or restart diagnosis. |
| `go_sched_gomaxprocs_threads` | The current runtime.GOMAXPROCS setting, or the number of operating system threads that can execute user-level Go code simultaneously. Sourced from /sched/gomaxprocs:threads. | Runtime/process saturation or restart diagnosis. |
| `go_threads` | Number of OS threads created. | Runtime/process saturation or restart diagnosis. |
| `process_cpu_seconds_total` | Total user and system CPU time spent in seconds. | Runtime/process saturation or restart diagnosis. |
| `process_max_fds` | Maximum number of open file descriptors. | Runtime/process saturation or restart diagnosis. |
| `process_network_receive_bytes_total` | Number of bytes received by the process over the network. | Runtime/process saturation or restart diagnosis. |
| `process_network_transmit_bytes_total` | Number of bytes sent by the process over the network. | Runtime/process saturation or restart diagnosis. |
| `process_open_fds` | Number of open file descriptors. | Runtime/process saturation or restart diagnosis. |
| `process_resident_memory_bytes` | Resident memory size in bytes. | Runtime/process saturation or restart diagnosis. |
| `process_start_time_seconds` | Start time of the process since unix epoch in seconds. | Runtime/process saturation or restart diagnosis. |
| `process_virtual_memory_bytes` | Virtual memory size in bytes. | Runtime/process saturation or restart diagnosis. |
| `process_virtual_memory_max_bytes` | Maximum amount of virtual memory available in bytes. | Runtime/process saturation or restart diagnosis. |

### Verification

`internal/center/telemetry` parses actual text scrapes and checks isolated
registries, counts, histograms, in-flight requests, all documented routes, scanner
cardinality, collector failures, node deletion, missing values and the 200-node
cap. `TestOperationalMetricsEndToEndIntegration` runs an authenticated ingestion,
replay, invalid-item, heartbeat and operator scrape flow through a real HTTP
listener and the guarded worktree PostgreSQL database. Worker integration tests
assert success and error observations. `TestOperationalMetricsCombinedAndOutermost`
checks legacy values, content type and recovered panic counting.

Run the serial database suites only after verifying the configured worktree DB:
`go test -race -p 1 -tags integration ./...`. Browser UI behavior is unchanged;
shared JOOST-45/48 scenarios continue to cover node and result flows. No schema
migration or configuration default is added by telemetry. To disable it in an
embedded server, omit `Options.Telemetry`; keep the legacy Metrics source.

## Who alerts on what

| Layer | Tool | Examples | Delivery |
| --- | --- | --- | --- |
| System | Prometheus, Alertmanager, Loki, Grafana (monitor VM) | container or edge down, certificate expiry, 5xx rate, latency, database pool, stuck background loops, host and container metrics | Alertmanager template to Telegram |
| Application | Speedtest's own alert engine | node offline, failed tests in a row, speed below expected, IP change, bandwidth budget | Speedtest to Telegram, Slack, e-mail or webhook (Settings > Notifications) |
| Errors and logs | Sentry, OpenObserve | exceptions, request logs, dashboards | Sentry issue alerts; OpenObserve search and dashboards |

Do not add application rules to Prometheus: the speed and node rules belong to Speedtest alert rules.

## Error tracking (Sentry)

The center and the agent send unexpected errors and panics to Sentry when `SPEEDTEST_SENTRY_DSN` is set; with it empty nothing is sent
and nothing changes.

| Variable | Meaning |
|---|---|
| `SPEEDTEST_SENTRY_DSN` | the project DSN (treat it as a secret; never log it) |
| `SPEEDTEST_SENTRY_ENVIRONMENT` | environment tag; default `APP_ENV`, then `production` |
| `SPEEDTEST_SENTRY_SAMPLE_RATE` | 0 to 1, default 1 (every error) |

Release is the build version (`make release VERSION=...`, `main.version`), so a new deployment shows up as a new release. Every event
carries the tag `component=center|agent`.

**What is reported:** panics (HTTP handlers, the agent), HTTP answers of 500 or more (grouped by method, route template and status, so a
broken endpoint is one issue, not thousands), failed iterations of the background loops (`loop=alerts|notifications|retention`), and the
agent stopping after a lifecycle failure. Failed speed tests are not reported: they are product data, handled by alert rules.

**What is never sent:** request bodies, query strings, cookies, headers, user identity, client IP addresses, breadcrumbs, host name. Text
in messages is scrubbed of passwords and tokens (`password=...`, `Bearer ...`, node tokens), e-mail addresses and IPv4 addresses
(`internal/shared/errtrack`, unit-tested). Route labels come from a fixed vocabulary, so scanners cannot create new issue groups.

**Set it up:** in Sentry create a project "Speedtest" (platform Go) and copy its DSN into the center's `.env`/service environment and,
if you want agent errors too, into each agent's environment (`SPEEDTEST_SENTRY_DSN`). In Sentry add an alert rule "new issue" to your
channel. The center logs `error tracking is on` (never the DSN) at startup, or `error tracking is off` with the reason.

## Logs and metrics in OpenObserve

`deploy/observability` ships both to OpenObserve with Vector and creates the dashboard and alert rules.

| Piece | File | What it does |
|---|---|---|
| Shipping | `vector.yaml` | reads the JSON log lines of the center, agents and the probe from Docker (swap in the `journald` source for a systemd install) and scrapes `/metrics` every 30 s; logs go to the stream `speedtest_logs` (fields `level`, `msg`, `request_id`, `method`, `path`, `status`, `duration_ms`, `service`, `node`), metrics to Prometheus streams named like the metric. It carries Vector unit tests (`vector test`) |
| Runner | `observe.sh` | `env` writes a template `vector.env` (mode 0600, git-ignored) to fill in, `up` starts Vector, `probe` starts the probe, `status`, `down` |
| Probe | `probe.sh` | once a minute: `/readyz` status and time, days until the TLS certificate expires, free disk %, as one JSON log line |
| Provisioning | `provision_openobserve.py` | idempotent; creates the dashboard "Speedtest overview" (12 panels) and 13 alert rules; dry run by default, `--apply` to write; needs `OPENOBSERVE_USER`/`OPENOBSERVE_PASSWORD` (a dedicated user in production, never printed) |

```bash
sg docker -c "deploy/observability/observe.sh env"      # then fill in deploy/observability/vector.env
sg docker -c "deploy/observability/observe.sh up"
sg docker -c "deploy/observability/observe.sh probe"
python3 deploy/observability/provision_openobserve.py --env-file deploy/observability/vector.env --apply
```

OpenObserve only accepts an alert on a stream that already exists, so run the provisioning again a few minutes after the first data
arrives; it lists the alerts it had to skip.

### Alerts

Thirteen rules, each linked to a runbook entry in [operations.md](operations.md#runbook):

| Alert | Fires when | Runbook |
|---|---|---|
| speedtest-center-down | the probe gets no answer from `/readyz` twice in 3 min | center-down |
| speedtest-readyz-failing | `/readyz` answers but not 200 twice in 5 min | readyz-failing |
| speedtest-5xx-rate | more than 0.05 server errors per second for 5 min | server-errors |
| speedtest-error-burst | 20 or more ERROR log lines in 5 min | server-errors |
| speedtest-latency-p95 | p95 request latency above 1 s for 10 min | slow-requests |
| speedtest-no-results | newest result older than 2 h | no-results |
| speedtest-nodes-offline | any node without a heartbeat for 10 min | nodes-offline |
| speedtest-notifications-failing | notification deliveries failed in 15 min | notifications-stuck |
| speedtest-notifications-stuck | a notification waiting more than 10 min | notifications-stuck |
| speedtest-db-pool-exhausted | pool 90% used for 5 min | database-pool |
| speedtest-job-stale | a background loop has not succeeded for 15 min | background-jobs |
| speedtest-disk-low | under 15% disk free | disk-space |
| speedtest-certificate-expiring | certificate expires in under 14 days | certificate-expiry |

**Where alerts go.** OpenObserve needs a destination for every alert. Give `TELEGRAM_BOT_TOKEN` and `TELEGRAM_CHAT_ID` (or
`SPEEDTEST_ALERT_WEBHOOK_URL`) to the provisioning script and run it again: it creates the destination and points every rule at it. Until
then the rules use the placeholder destination `speedtest-unrouted` (a public address that ignores the call, because OpenObserve refuses
private hosts): they are evaluated and their state shows in OpenObserve, but nothing reaches a person. The script prints a warning in
that case. Sentry issue alerts are configured in Sentry (see above) and are independent of this.
