# Speedtest API contract

[openapi.yaml](openapi.yaml) is the OpenAPI 3.1 source of truth for JOOST-9.
It defines the product API endpoints; the static `ui/` preview still runs on
local demo state. The command center also exposes the operational `/healthz`
and `/readyz` checks outside that product API contract. Operational `GET /metrics`
uses Prometheus text exposition and separate loopback/bearer access rules; see
[the metric catalog and alert signals](observability.md#metrics). When database settings
are configured, readiness pings PostgreSQL and reports `ok` or `unavailable`;
without database settings it reports `not configured`.
See the [OpenAPI specification](https://spec.openapis.org/oas/v3.1.0) and
[Redocly lint command](https://redocly.com/docs/cli/commands/lint).

## Authentication and authorization

UI requests use the opaque `speedtest_session` cookie, with `HttpOnly`,
`SameSite=Lax`, `Path=/`, and `Secure` over HTTPS. The session is stored server-side;
passwords use argon2id. Every authenticated UI session has the single owner's
permissions; this contract does not introduce roles or SSO.

The center stores only SHA-256 hashes of the opaque session and CSRF bearer
values. `csrfToken` is a per-session synchronizer token; the server reissues it
from `auth/me` without storing its plaintext. Expired sessions are pruned when a
new anonymous session is created. Login limits each remote client IP
and normalized account email to 5 attempts per 15-minute sliding window by
default (`SPEEDTEST_LOGIN_MAX_ATTEMPTS`, `SPEEDTEST_LOGIN_WINDOW`). The limit is
in process memory. By default, the direct TCP peer is the client IP. Configure
`SPEEDTEST_TRUSTED_PROXY_CIDRS` to accept forwarded client and HTTPS scheme headers
from explicitly trusted reverse proxies; forwarded headers from other peers are ignored.
Login failures use the same 401 problem for an unknown email
and an incorrect password. New passwords must contain at least 12 Unicode
characters and at most 1024 UTF-8 bytes.

Owner API access tokens are separate named credentials created and revoked at
`/api/access-tokens` from an authenticated session. A token is returned only in
its creation response; PostgreSQL stores its SHA-256 digest, and token metadata
never includes the bearer value or digest. Tokens can expire and successful
uses update `lastUsedAt`. Revocation blocks new requests immediately; an open
installation event stream closes at its next 10-second credential check. Keep
the bearer value in a secret store and send it as
`Authorization: Bearer <token>` over HTTPS. Bearer requests do not need the
session cookie, Origin or CSRF header.
The per-token request bucket defaults to 120 requests per minute with a burst
of 20, configured by `SPEEDTEST_API_TOKEN_REQUESTS_PER_MINUTE`.

The scopes are `read` (safe owner operations), `read-write` (read plus
non-destructive changes), and `full` (all operations, including destructive
changes). Every operation declares its required scope in
`docs/openapi.yaml` using `x-access-token-scope`; a token must meet that
operation's scope before its handler runs. Agent credentials and auth/session
endpoints are not API-token operations. Creating and revoking a token creates a
safe audit entry. Audit rows retain the authentication method, the non-secret
credential ID for bearer writes, and the created token ID as the creation
target, so operators can distinguish tokens owned by the same account. MCP
tool writes retain the incoming client address using the same trusted-proxy
rules as REST writes. Access logs omit request headers and query data; bearer
values are never written to the audit log or application logs.

The Streamable HTTP MCP endpoint is `/mcp`. Its tools are generated from the
OpenAPI operations marked with `x-mcp-tool`, including their input schemas and
scope requirements; it does not return integration secrets. The separate
`speedtest-mcp` executable exposes the same remote tools over stdio for MCP
clients. See [MCP setup and tool reference](mcp.md).

Run `go run ./cmd/center seed-admin` to ensure the documented default owner is
present in local or test environments. `APP_ENV` must be `local` or `test`; the
command refuses staging, production and unknown values. It inserts only when the
email does not already exist, hashes the password with Argon2id, and never resets
an existing password. Local/test commands require `speedtest_test` or a
`speedtest_wt*` database with the Workspaces ownership marker. Keep the default
credential limited to local/test data.

1. `GET /api/auth/me` without a cookie establishes an anonymous session and returns
   `user: null`, `csrfToken`, `expiresAt`, and `Set-Cookie`.
2. Send that cookie and `X-CSRF-Token` with `POST /api/auth/login`. Login failures
   are generic; rate limits apply per IP and account. Success rotates the session
   and CSRF token and returns the authenticated `Session`.
3. Every UI write, including login/logout, requires the session-bound CSRF header
   and same-origin Origin validation. An expired/revoked cookie returns 401, including on
   `auth/me`; clear it and bootstrap again. Password changes invalidate other
   sessions; fetch `auth/me` for the rotated CSRF token. Logout expires the cookie.

Agents never use sessions. `POST /api/nodes` creates a node and discloses a
single-use enrollment token, expiring in 24 hours. `POST /api/agent/enroll`
exchanges it for a long-lived **node bearer credential**. Send
`Authorization: Bearer <node-credential>` on all other node agent operations.
The credential authorizes only that node, including command acknowledgements;
foreign resources return 404. Store credentials in a `0600` agent file and
hash them at the center. Compare securely, rate limit per node, and reject
revoked credentials on every request. Rotation revokes the old credential
immediately; the owner securely delivers the new credential to the node.
A lost enrollment response requires a newly issued one-time token.

Node creation and credential issuance require an `Idempotency-Key`. The center
replays the same response for the same owner, operation, key and request body
for 24 hours; reusing the key with a different body returns 409. Replay payloads
are encrypted with the 32-byte base64 `APP_KEY`; the enrollment token and node
credentials remain hashed in `node_credentials`. Set `APP_KEY` in the center's
private `.env` before enabling issuance. The Settings enrollment key is issued
once by `POST /api/settings/enrolment-key`, stored as a hash in the private
Settings secret slot, and accepted only
by `POST /api/agent/tokens`. The latter can create a node and one-time token
without a UI session, for install-script bootstrap. It cannot read or modify
existing nodes. The node bearer middleware checks revocation on each agent API
request and applies a process-local token bucket per node (default 120 requests
per minute with a burst of 20; configure `SPEEDTEST_AGENT_REQUESTS_PER_MINUTE`).

Rotating the Settings enrollment key does not revoke already issued single-use
tokens or existing node credentials. Long-lived keys and credentials are
disclosed only at issuance, never through a general GET.
The public `/install.sh` contains no credentials. All other endpoints require
session or node authentication as declared in OpenAPI; public login/bootstrap
and token exchange are explicit exceptions.

Use HTTPS outside local/test environments. Integration secrets are write-only;
reads expose `configured` flags, never stored values or masked placeholders.
Omitting a secret from a Settings PUT preserves it. Encrypt recoverable
integration secrets at rest using the application's `.env` key (`APP_KEY`);
never expose it through this API. Slack/webhook URLs belong in write-only
`secret`, not public `to`. The SSH probe performs only an unauthenticated host-key handshake. Credentials
are sent exactly once, after fingerprint confirmation, with the installation
request; they remain in memory for that operation and never appear in progress
events. Confirm/pin the host key before authentication; abort on change.

The SSH installer is available only when `SPEEDTEST_PUBLIC_URL` and
`SPEEDTEST_RELEASE_DIR` are configured and the center has a 32-byte `APP_KEY`.
The probe expires after 10 minutes and can be consumed by one install request.
SSH credentials are used only by the in-memory worker; database idempotency
stores an APP_KEY-protected request digest, not a password or private key. The
installer issues a fresh single-use enrollment token for the selected existing
node, transfers it through a temporary FIFO, and removes temporary remote state
after the first heartbeat or rollback. Progress is replayable for 30 days and
uses plain-language messages that omit credentials and remote command output.
See the [SSH installer runbook](ssh-installer.md) for operations, repair and rollback.

## Shared wire rules

- Success JSON: `success`, `code` (actual HTTP status), `message`, `data`, `meta`,
  `errors: null`, optional `trace_id`, defined once by `Success`. Collections put
  arrays directly in `data`. Errors are unwrapped RFC 9457 `Problem` objects with
  `application/problem+json`; use `type`/status, never parse human text.
- Every response includes `X-Request-ID`. A supplied value is reused only when it
  has 8–64 ASCII letters, digits, hyphens or underscores; otherwise the center
  generates one. Problem responses include the same value as `trace_id`.
- `Timestamp` is RFC 3339 UTC with `Z`, including result `ts`, creation times and
  IP `first`/`last`. The UI adapter converts to/from demo epoch milliseconds.
  Time ranges are `[from,to)`, avoiding duplicate boundary samples. Explicit
  `from`/`to` must be paired and cannot accompany `range`; default range is 7d.
- `Mbps`, `Milliseconds`, `Percent` define units. `down`/`up` and expected speeds
  are decimal Mbps; `ping`/`jitter`/`dLat`/`uLat` are ms; `loss` is 0–100 percent;
  duration/uptime are seconds, bytes are bytes, schedule interval/offset minutes,
  geo radius km. Percent of expected can exceed 100. Display units never change
  wire values. Null means unavailable; zero is a real measurement.
- All lists that grow use `page` (1-based) and `size` (default 25, maximum 100),
  with `meta.pagination` (`current_page`, `per_page`, `total`, `last_page`).
  Empty/beyond-last pages return `data: []`; last_page is at least 1. Sorting is
  deterministic with the documented tie-breaker. Aggregates, dashboard snapshots,
  schedules and current-node maps are bounded views rather than history lists.
- Query `nodes` is comma-separated, at most 100 IDs; omission means all, empty
  means none. Unknown/unauthorized IDs fail without leaking foreign resources.
  Analytics requests span at most 366 days and 100 nodes. Series have at most
  300 points per node. Invalid fixed bucket requests return 422 instead of
  silently dropping data. Histogram bins are bounded to 1–100.
- GETs are safe. Responses use `Cache-Control: no-store`; conditional agent
  configuration uses `private, no-cache` and ETag/If-None-Match (304 has no body).
  For editable resources use the GET ETag in required `If-Match`: missing=428,
  stale=412. Settings/schedule updates advance effective node config revisions.
- PUT replaces documented nonsecret fields. Node PATCH uses
  `application/merge-patch+json`; null is rejected for its nonnullable fields.
  `Idempotency-Key` on retryable creates replays the original status/body/Location
  for the same principal/key/body for 24h; a changed body returns 409. Securely
  retain issuance responses for replay, encrypted at rest, then erase them.
  DELETE is idempotent; repeat node deletion returns 404 without new effects.
- 201/202 resource responses include `Location` where addressable; 202 callers
  poll the named command, delivery, installation or job resource. 204/304 have
  no body. CSV/JSON exports, installer script and SSE are documented envelope
  exceptions. SSE schema describes each JSON `data` payload, not framing:
  `id: <id>`, `event: progress`, `data: <InstallEvent JSON>`, blank line.
- Errors: 400 malformed input; 401 auth; 403 authorization/CSRF; 404 hidden/missing
  resource; 409 state/idempotency conflict; 412/428 conditional update; 413 batch
  too large; 405 wrong method with an `Allow` header; 422 validation; 429 rate
  limit with Retry-After; 500 safe server error. Unknown `/api/` paths use the
  same 404 Problem shape. Non-API paths retain static UI file-server behavior.
  Never include tokens, SQL details or stack traces in errors. Access logs record
  the request path without its query string and the remote address.

Agent result batches are at most 100 items/8 MiB, with engine `raw` at most 64 KiB
per item. Validate individual items independently: a valid container returns 207
and one `BatchOutcome` per input index, including invalid items. `accepted` (201)
and `duplicate` (200) can leave the spool; `rejected` (422 or conflicting UUID
409) must be quarantined with a safe diagnostic. Missing/invalid UUID returns
`id: null`. A malformed container gets 400/422 rather than a partial response.
Result UUIDs are globally unique and preallocated by the agent. An identical
replay by the same node is `duplicate` (200) and cannot create another row.
Changed content for that UUID, or a UUID already owned by another node, is a
per-item 409 conflict with a generic diagnostic that reveals no other-node data. Retry transport failures/429/5xx with backoff and jitter;
never discard unacknowledged spool entries. Failed results require an error and
allow null measurements and `ip: null` when the address is unavailable. They also
carry `failureClass` (omitted values are classified from the error text). Never invent an IP or reuse a stale one. IP history hooks skip null-IP results; consumers display "unavailable".
Successful/degraded results require down/up/ping, a non-null IP, a server object with a nonblank name or host, and `error: null`. Numeric `serverId` is optional for every status, positive when present (LibreSpeed automatic selection has no numeric ID). Engines may omit
loss/jitter/loaded latency even on success; preserve those as null. Optional
loadedDown/loadedUp retain low/iqm/high; engine details retain ISP, result ID/URL,
external IP and mismatch warnings, with remaining original fields in raw.
After each measurement, including engine failures, the agent calls `GET /api/agent/ip` over the
same selected interface/source IP. The center-observed `data.ip` is the result's
public IP; preserve the engine-reported address as `engineIp` and add a `warnings`
entry when the two addresses differ.

Agent config advertises the latest release URL/version/sha256 and defaults
autoUpdate to false. The owner enables it per node; verify checksums, replace
atomically and roll back if the first heartbeat fails within two minutes.
Heartbeats optionally report update outcome and version.

Poll commands oldest-first, up to 100 per call. Redeliver until terminal ack;
agents remember command IDs to prevent duplicate execution. Running a manual
test returns a Command to poll; progress acks carry phase/pct/resultId/error.
Terminal acks replay idempotently; conflicting terminal changes return 409.
Each agent gets only its own configuration, effective offset and quiet hours.
The center coordinates offsets; there is no WAN, load-balancer or peer-node model.

## Backlog and UI coverage

| Consumer / backlog | Operations and response data |
| --- | --- |
| Auth, JOOST-12 | `/api/auth/{login,logout,me,password}`; `Session`, `User` |
| Enrollment/control, JOOST-14/17/18/19/22/23 | `/api/agent/{tokens,enroll,results,config,heartbeat,commands,ip}`, command ack; `Enrolled`, `AgentConfig`, `BatchOutcome`, `Command`; node credential rotate/revoke and replacement token |
| Dashboard, JOOST-27/38 | `/api/dashboard`, `/api/series`; `Fleet` current/previous, `NodeCard` summary/sparkline/latestGood/current IP/city, latest results/open alerts/count, `Series.summary` for table mode |
| Nodes and wizard, JOOST-25/41/43/44 | node CRUD/run/commands, installer probes/installations/uninstallations and SSE; `Node`, `Enrollment`, `Install`, `InstallEvent`, pinned fingerprint, `Command` progress |
| Results, JOOST-24/32/38 | results list/detail/filtered CSV and full CSV/JSON exports; `Result` includes every measured field, `Server`, `Geo`, raw JSON and manual flag; search/sort/range/download/status filters |
| Statistics, JOOST-26/39/81 | `/api/stats/{summary,trend,hour-of-day,heatmap,distribution,events}`; `SummaryComparison`, percentiles, engine and ISP filters, bounded event marks; combined total is display-only |
| ISPs, JOOST-74 | `/api/isps`, `/api/isps/compare`, `/api/isps/{id}`, `/api/isps/{id}/trend`, `/api/isps/{id}/plans`; entities from uint32 ASN/organisation; engine-isolated aggregates; optional plans; `isps`/`isp` filters on results, statistics and alert events |
| Monthly report, JOOST-79 | `GET /api/reports/monthly?month=YYYY-MM&format=json|csv|pdf&isp=&engine=`; per ISP and node; UTC month; explicit unknown-ISP rows; current downtime bounded by `generatedAt`; CSV protects formula cells; PDF preserves Unicode and wraps long names |
| Results, JOOST-24/32/38/80 | results list/detail/filtered CSV and full CSV/JSON exports; `Result` includes every measured field, bytes down/up/total, `Server`, `Geo`, raw JSON and manual flag; search/sort/range/download/status filters |
| Statistics, JOOST-26/39 | `/api/stats/{summary,trend,hour-of-day,heatmap,distribution}`; `SummaryComparison`, `MetricSummary`, `Series`, `HourSeries`, `Heatmap` counts, `Distribution` edges/count/percent; combined total is display-only |
| ISPs, JOOST-74 | `/api/isps`, `/api/isps/compare`, `/api/isps/{id}`, `/api/isps/{id}/trend`, `/api/isps/{id}/plans`; entities from uint32 ASN/organisation; engine-isolated aggregates; optional plans; `isps`/`isp` filters on results, statistics, usage and alert events |
| IP & location, JOOST-33/34/35/36/39/42 | `/api/ip/{history,addresses,lookup,map}` (location database updates run on the center's schedule; manual update/refresh/status endpoints are not part of v1); `IPChange`, `IPAddress`, `Geo` (all location/ASN/ISP/accuracy/source fields), `MapLocation`, database timestamp/cache count, maintenance `Job` |
| Schedules, JOOST-28/39 | `/api/schedules`, per-node GET/PUT, rules PUT, stagger POST; `ScheduleInput`, `ScheduleRules`, `Schedules` clashes/upcoming/nextRun/bytesPerDay/bytesPerMonth |
| Alerts, JOOST-29/40 | rules CRUD, events list/detail/ack, snapshot acknowledge-all job; `AlertRule`, `AlertEvent` title/detail/severity/ts/resolvedAt/acked; reopen means acked=false |
| Settings/notifications, JOOST-30/31/40/78 | Settings GET/PUT/key rotation, test-channel POST, delivery logs/status, event catalog and node overrides, template get/update/preview/reset; `Settings`, `NotificationSettings`, `NotificationEvent`, `NotificationTemplate`, `Delivery`, issuance-only `IssuedKey` |
| Settings/notifications, JOOST-30/31/40 | Settings GET/PUT/key rotation, test-channel POST, delivery logs/status; `Settings`, `NotificationSettings`, `Delivery`, issuance-only `IssuedKey` |
| Bandwidth, JOOST-80 | `/api/usage`, `/api/usage/advice`, `/api/usage/budgets`; UTC totals and day/week/month buckets stay separated by node and engine; optional global/node budgets |
| Installer distribution, JOOST-46 | public `GET /install.sh`; checksum-verifying shell script |

The alert engine's rule validation, per-result streak semantics, offline checks,
event filters/acknowledgement and durable JOOST-30 handoff are described in
[`docs/alerts.md`](alerts.md). The alert paths and schemas below are already
part of this OpenAPI contract.

Field names follow `seed.js` and `store.js`: Node `name`, `label`, `provider`,
`plan`, `colorSlot`, `expectedDown`, `expectedUp`, `interval`, `offset`, `threads`,
`server`, `enabled`, `host`, `agent`, `note`, `createdAt`; Result `id`, `nodeId`,
`ts`, `serverId`, `threads`, `ip`, `status`, `error`, `failureClass`, `down`, `up`, `ping`, `jitter`,
`loss`, `dLat`, `uLat`, `bytesDown`, `bytesUp`, `duration`, `manual`. Real IDs are
opaque (result IDs are UUIDs); demo IDs are not wire IDs. `profile`, `ipModel`,
`pref`, `OUTAGES` and RNG state are simulation inputs, never API fields. Demo
load-balancer labels remain plain text; IP changes are counted for every node.
The adapter maps demo `abs`/`pct` chart modes to API `absolute`/`percent`, resolves
`browser` timezone for analytics, and does not read secrets back from Settings.
Reset demo data remains a local preview action, with no destructive API endpoint.

Statistics follow the UI formulas: summaries include nonfailed samples;
trend/hour-of-day/heatmap/distribution include only `ok` samples as `stats.js`
does. Every `MetricSummary` keeps its existing fields and adds nullable `p50`
and `p99`; `p50` equals `median`. Percentiles interpolate at `(n-1)*p`,
standard deviation is population,
and stability is `clamp(100-220*std/mean,0,100)`, null for zero/no mean. Counts
include failures; success rate includes degraded tests and excludes
`failureClass=rate_limited` (shown separately as `rateLimited`). Fleet
percentages are unweighted means of node percentages; fleet latency is median of
ok-only pings. Alert rules with metric `failed` also ignore rate-limited results.
`failureClass` is the fixed vocabulary `rate_limited`, `no_network`,
`server_unreachable`, `engine_missing`, `timeout`, `other`.
Summary previous periods have equal adjacent duration. Missing chart buckets
have null value/count=0. Timezone grouping uses IANA local calendar boundaries,
including DST; heatmap is Monday=0..Sunday=6 × hour 0..23, last 30 days. Combined
series sum the values available from selected nodes in each bucket; they do not
persist topology.

### Statistics endpoints (JOOST-26)

All statistics endpoints require an authenticated UI session and return the standard
success envelope. Omit `nodes` to include every node; `nodes=` selects none.
Explicit `from` and `to` are an inclusive/exclusive RFC 3339 interval, must be
given together, and cannot be combined with `range`. Analytics intervals are
limited to 366 days. `range` accepts `6h`, `24h`, `7d` (default) or `30d` and
ends at the request snapshot.

```text
GET /api/stats/summary?range=30d&nodes=<node-id>
GET /api/stats/trend?from=<RFC3339>&to=<RFC3339>&metric=down&bucket=hour&timezone=Asia/Ho_Chi_Minh
GET /api/stats/hour-of-day?range=7d&metric=ping&timezone=Asia/Ho_Chi_Minh
GET /api/stats/heatmap?node=<node-id>&metric=down&timezone=Asia/Ho_Chi_Minh
GET /api/stats/distribution?range=30d&metric=down&bins=12
GET /api/stats/events?range=7d&nodes=<node-id>&engine=ookla&isp=Viettel
```

All statistics endpoints accept optional `engine` and `isp` filters. An omitted
engine selects each node's primary engine, so different engines are never mixed.
`isp` is one trimmed 1–200 character, case-insensitive exact match; repeated or
whitespace-only values return 422. Effective ISP uses the trimmed result
`engine_isp`, then the cached organisation for that result's IP. A specific ISP
filter excludes results with unknown ISP. Cached geolocation can be refreshed,
so historical ISP attribution may change. `isp` is applied before every
aggregate and comparison, including the heatmap and event marks.

`summary` returns current and immediately previous intervals of equal length.
Its counts include failed tests; `ok` includes both `ok` and `degraded` results.
Success rate is `ok/(count-rateLimited)*100`. Each metric's `n` counts its
non-null measurements from those nonfailed results. Download and upload expected
percentages use that node's configured expected speed. Stability uses download's
population standard deviation. Empty values are null, and a single value has
standard deviation 0.

Trend, hour-of-day, heatmap, and distribution use only status `ok`. Trend uses
calendar `hour` or `day` buckets in the supplied IANA zone (`UTC` by default),
with at most 300 points per node; missing values have `count: 0` and `value:
null`. Spring-forward hours that do not exist are omitted and repeated fall-back
hours are combined under the same local hour. The optional combined trend is
only available for down/up: it sums selected node bucket averages wherever at
least one selected node has a value, matching the preview. A bucket with no
selected node values is null. Its count is the sum of contributing sample
counts, and its summary covers non-null combined bucket totals. Per-node series
summaries use raw source measurements rather than averages of buckets.
Hour-of-day always returns all 24 hours. Heatmap requires one node and returns
168 weekday/hour cells for the rolling 30 days ending at the request snapshot.

### Bandwidth usage and budgets (JOOST-80)

`GET /api/usage` requires an authenticated session. Omit `from` and `to` for
the trailing 30 days ending at the request snapshot; otherwise provide both as
RFC 3339 timestamps, inclusive `from` and exclusive `to`, at most 366 days.
`group` is `day` (default), `week` or `month`; `node`, `engine` and `isp` are optional.
The ISP filter uses each result's recorded ISP assignment, including historical
results, and usage rows remain split by ISP as well as node and engine.
When `engine` is omitted, each node/engine remains its own row and series; the
center never blends different engines into an average. The response also
contains month-to-date, previous calendar month, a month-end projection and
configured budget status. The projection scales observed month-to-date usage
by elapsed UTC calendar time.

```text
GET /api/usage?group=week&from=<RFC3339>&to=<RFC3339>&node=<node-id>&engine=ookla&isp=<isp-id>
GET /api/usage/advice?node=<node-id>&engine=ookla&interval=60
GET /api/usage/budgets
PUT /api/usage/budgets/global
PUT /api/usage/budgets/<node-id>
DELETE /api/usage/budgets/<node-id>
```

Advice estimates daily and 30-day volume from the selected node/engine's
average complete measured byte pairs per test over the prior 30 days and the
requested interval. Missing counters and empty histories are unavailable, never
zero-valued estimates. If `budgetBytes` is omitted, the node budget and global
budget both constrain advice when configured; estimated monthly use by other
enabled engines and nodes is reserved before recommending an interval. The response reports `recommendationStatus` as `feasible`,
`infeasible`, or `unavailable`. A budget that cannot be met even at the
maximum 1440-minute interval has no recommended interval and is marked
`infeasible`; the UI tells the owner to increase the budget or reduce the test
volume. An empty/incomplete history or missing measurements in another enabled
schedule sharing the cap produces `unavailable` with an actionable explanation.

Budgets are optional positive byte limits with `widen` or `pause` action. Read
the collection ETag before writing; every PUT/DELETE requires that value in
`If-Match`, and stale updates return 412. At 80% the center records one warning
event per scope/month. At 100% it records a limit event, applies the configured
schedule action transactionally, writes an audit entry and records a durable
notification extension event. JOOST-78 can consume the named
`PendingNotificationEvents` / `MarkNotificationEventPublished` extension point.
If a widen policy reaches a schedule already at 1440 minutes, the center pauses
that schedule and reports the fallback in the dashboard banner and event queue.
The effective action is reconciled whenever a budget or schedule changes. A
pause takes precedence over interval widening when global and node budgets
overlap; otherwise the largest active widening multiplier applies. Owner
schedule edits become the new restoration baseline, and deleting a budget
restores the baseline when no remaining budget requires an override. The
selected action applies until the next UTC month, when original intervals and
enabled states are restored, including for paused nodes. New nodes default to a
60-minute interval and immediately inherit an active global policy.

The center exposes `speedtest_bandwidth_month_to_date_bytes{node_id,engine}`
and `speedtest_bandwidth_budget_ratio{scope,node_id}` application gauges.
These are scrape-time values from the current UTC month. System alert rules and
notification infrastructure are maintained outside the application.

The same query is available to MCP clients as the read-only `get_usage` tool (marked `x-mcp-tool` in `docs/openapi.yaml`; see the MCP section for the HTTP and stdio servers). It accepts optional `node`, `engine`, `isp`, `from`, `to` and `group` arguments with the same meanings and limits as `GET /api/usage`.

Distribution accepts `bins` from 1 through 100 (default 15); the preview's
current chart uses 12, so pass `bins=12` when comparing its histogram directly.
Bin edges cover the sample minimum through maximum and the last upper edge is
inclusive. Constant data uses a nonzero range centered on the value; no samples
returns `bins: []` and `count: 0`.

`GET /api/stats/events` accepts the same `range` or `from`+`to`, `nodes`,
`engine` and `isp` filters. It returns `data.items` and `data.truncated`; items
are newest-first with ties ordered by type and ID, capped at 500. A true
`truncated` value means more matching events exist. IP changes compare
consecutive known result IPs per node and selected engine, ordered by `(ts,id)`;
null IPs are skipped and the selected engine's last known predecessor before
the requested window is included. This avoids treating another engine's IP
measurement as a transition for the selected engine. ISP changes compare
consecutive known effective ISPs per node and engine, ordered by `(ts,id)`, and
include the last known predecessor before the requested window. IP/ISP marks and
alerts with a `trigger_result_id` use that result's engine and effective ISP.
Unlinked alerts appear only without an ISP filter and are labelled `engine
unknown`; event labels contain plain text. The endpoint reports alert openings,
not inferred threshold anomalies.

The Statistics page's week-over-week comparison requests `summary?range=7d`
independently of the page range. It compares each selected node's selected
metric mean over `[now-7d,now)` with `[now-14d,now-7d)`. The percent change is
`(current-previous)/previous*100`; it is unavailable if either mean is null or
the previous mean is zero.

Settings defaults match the preview: dark, 7d, auto units, browser timezone,
24-hour clock, one decimal, 365-day retention (0=forever), GeoLite2 + ip-api fallback,
weekly updates, Ookla/auto server/4 threads, stagger on/maxConcurrent=1, empty
quiet hours, email enabled to `ops@example.com`, other channels off/empty.
Public center URL comes from deployment `.env`, not the demo URL. `POST /api/nodes`
does not accept an interval; its response and the new node's Ookla schedule use
interval=60/offset=0, enabled, threads=4, server=auto. Newly enabled engine
schedules also default to 60 when interval is omitted or zero. Legacy schedule
PUT fields remain required. The 60-minute migration changes only the schedules
column default, never existing rows; rollback restores that default to 30.
Schedule PUT clamps offset after interval changes; effective quiet timezone is explicit. Usage uses
up to 200 recent nonfailed results × tests/day, month=30 days; paused/no history=0.
The UI should display null measurements/location as unavailable, not zero.

## Reproduce contract verification

Run from the ticket worktree on the native Node runtime. Tools are temporary
and are not application dependencies. The first command installs a pinned
linter, YAML parser and JSON Schema validator under ignored `.tmp/` only:

```bash
npm_config_cache="$PWD/.tmp/npm-cache" npm install --prefix .tmp/tools --no-audit --no-fund @redocly/cli@2.72.1 yaml@2.8.3 ajv@8.17.1 ajv-formats@3.0.1
.tmp/tools/node_modules/.bin/redocly lint docs/openapi.yaml --extends recommended-strict
```

Run the following schema and UI fixture checks without any database or service.
It exercises actual deterministic demo data, null failures, empty states and
range boundaries; these are contract tests, not evidence of endpoint execution.

```bash
node <<'JS'
const fs = require('node:fs'), vm = require('node:vm'), assert = require('node:assert/strict');
const YAML = require('./.tmp/tools/node_modules/yaml');
const Ajv = require('./.tmp/tools/node_modules/ajv/dist/2020').default;
const formats = require('./.tmp/tools/node_modules/ajv-formats');
const api = YAML.parse(fs.readFileSync('docs/openapi.yaml', 'utf8'));
const ajv = new Ajv({ strict: false, allErrors: true });
formats(ajv); ajv.addSchema(api, 'contract');
const schemas = api.components.schemas;
const compiled = {};
const validate = name => compiled[name] ||= ajv.compile({ $ref: `contract#/components/schemas/${name}` });
for (const name of Object.keys(schemas)) validate(name);
let checks = 0;
function check(name, value, expected = true) {
  const v = validate(name);
  assert.equal(v(value), expected, `${name}: ${JSON.stringify(v.errors)}`); checks++;
}
const ctx = { window: {}, console, localStorage: { getItem: () => null, setItem() {} } };
vm.createContext(ctx);
for (const file of ['seed.js', 'store.js'])
  vm.runInContext(fs.readFileSync(`ui/assets/js/${file}`, 'utf8'), ctx);
const ST = ctx.window.ST, nodes = ST.seedNodes();
const plain = x => JSON.parse(JSON.stringify(x));
const select = (x, name) => Object.fromEntries(Object.keys(schemas[name].properties)
  .filter(k => k in x).map(k => [k, x[k]]));
for (const n of nodes) {
  check('NodeInput', plain(select(n, 'NodeInput')));
  check('ScheduleInput', plain(select(n, 'ScheduleInput')));
}
for (const server of ST.servers) check('Server', plain(server));
for (const rule of ST.rules()) check('AlertRule', plain(rule));
let failed = 0, good = 0;
for (const source of ST.state.results) {
  const r = plain(source);
  r.id = '00000000-0000-4000-8000-000000000001';
  r.ts = new Date(r.ts).toISOString(); r.manual = !!r.manual; r.error ??= null;
  r.server = plain(ST.server(r.serverId));
  check('ResultInput', select(r, 'ResultInput'));
  r.raw = {}; r.geo = plain(ST.geo(r.ip));
  check('Result', r); check('Geo', r.geo);
  r.status === 'failed' ? failed++ : good++;
}
assert(failed > 0 && good > 0);
const sample = plain(ST.state.results.find(r => r.status === 'ok'));
sample.id = '00000000-0000-4000-8000-000000000001';
sample.ts = new Date(sample.ts).toISOString(); sample.error = null; sample.manual = false;
sample.server = plain(ST.server(sample.serverId));
const input = select(sample, 'ResultInput');
check('ResultInput', { ...input, loss: 101 }, false);
check('ResultInput', { ...input, down: -1 }, false);
check('ResultInput', { ...input, down: null }, false);
check('ResultInput', { ...input, loss: null, jitter: null, dLat: null, uLat: null });
check('ResultInput', { ...input, ts: '2026-11-03T07:00:00+07:00' }, false);
check('ResultInput', { ...input, status: 'failed', error: '' }, false);
check('ResultInput', { ...input, status: 'failed', error: 'speedtest: no_route', ip: null });
check('ResultInput', { ...input, ip: null }, false);
check('ResultInput', { ...input, serverId: null, server: { name: 'LibreSpeed', host: 'speed.example' } });
check('ResultInput', { ...input, serverId: null, server: {} }, false);
check('ResultInput', { ...input, server: { name: '   ' } }, false);
check('ResultInput', { ...input, server: { name: 'valid', id: 0 } }, false);
check('ResultBatch', { results: [] }, false);
check('ResultBatch', { results: Array(101).fill(input) }, false);
check('ScheduleInput', { interval: 4, offset: 0, enabled: true }, false);
check('ScheduleInput', { interval: 1440, offset: 1439, enabled: true });
check('NodeInput', { name: 'test', expectedDown: 0, expectedUp: 1 }, false);
check('Pagination', { current_page: 1, per_page: 25, total: 0, last_page: 1 });
check('IP', '2001:db8::1'); check('IP', '203.0.113.1'); check('IP', 'localhost', false);
check('IP', '999.0.0.1', false);
const settings = select(plain(ST.settings()), 'Settings');
settings.geoKeyConfigured = false; settings.enrollKeyConfigured = true;
for (const channel of Object.keys(settings.notify)) settings.notify[channel].configured = false;
check('Settings', settings);
check('Settings', { ...settings, geoKey: 'must-never-be-returned' }, false);
const operations = Object.values(api.paths).flatMap(p => Object.values(p));
assert.equal(new Set(operations.map(o => o.operationId)).size, operations.length);
for (const response of Object.values(api.components.responses)) {
  const media = response.content['application/problem+json'];
  check('Problem', media.example);
}
const paths = [
  '/api/agent/enroll', '/api/agent/results', '/api/agent/config', '/api/agent/heartbeat',
  '/api/agent/commands', '/api/agent/commands/{id}/ack', '/api/agent/ip',
  '/api/nodes', '/api/results', '/api/isps', '/api/isps/compare', '/api/stats/summary', '/api/stats/trend',
  '/api/stats/hour-of-day', '/api/stats/heatmap', '/api/stats/distribution',
  '/api/dashboard', '/api/series', '/api/schedules/{node}', '/api/alerts/rules',
  '/api/alerts/events', '/api/notifications/test/{channel}', '/api/notifications/events',
  '/api/notifications/events/{key}', '/api/notifications/events/{key}/nodes/{id}',
  '/api/notifications/templates/{key}/{channel}', '/api/notifications/templates/{key}/{channel}/previews',
  '/api/notifications/templates/{key}/{channel}/resets', '/api/settings',
  '/api/ip/history', '/api/ip/addresses', '/api/ip/lookup', '/api/ip/map',
  '/api/auth/login', '/api/auth/logout', '/api/auth/me', '/api/auth/password',
  '/api/auth/2fa', '/api/auth/2fa/setup', '/api/auth/2fa/enable', '/api/auth/2fa/disable'
];
for (const path of paths) assert(api.paths[path], path);
for (const o of operations) {
  assert(o.security && Object.keys(o.responses).some(s => /^2\d\d$/.test(s)));
  for (const response of Object.values(o.responses))
    for (const media of Object.values(response.content || {}))
      if (media.schema && media.example !== undefined) {
        // Resolve local references through the registered contract.
        const resolved = JSON.parse(JSON.stringify(media.schema).replaceAll('#/components/', 'contract#/components/'));
        assert(ajv.compile(resolved)(media.example)); checks++;
      }
}
console.log(`PASS: ${operations.length} operations; ${Object.keys(schemas).length} schemas compiled; ${checks} checks; ${good} nonfailed/${failed} failed UI results`);
JS
```

The schema and fixture checks above validate contract examples against
deterministic UI data; they do not exercise running handlers, PostgreSQL,
migrations, browser behavior or a real agent. Implementation work must run the
unit and integration suites for each changed boundary, and UI-flow changes must
run browser E2E. Staging and real-agent soak are separate owner-authorized
verification. Follow the workspace Sonar/Gitleaks gates before any push.

Compatibility: `/api` is the initial v1 surface fixed by the backlog; do not add
an incompatible prefix for these consumers. Optional response fields/endpoints
may be added. Renames, removals, type/meaning changes, tighter accepted inputs or
changed status semantics require a coordinated migration and a new major prefix
(e.g. `/api/v2`), with a documented consumer transition/deprecation window.

## Agent ingestion implementation and operations

The center wires `internal/center/agentapi` behind the node credential middleware.
Every operation checks bearer revocation and the per-node request limit. HTTP
methods follow the contract (wrong methods return 405 with `Allow`). Results
commit in independent transactions, so a later database/transport error may
follow earlier commits; replay the whole unacknowledged batch safely.
An identical payload means the same stored values, including all nullable
metrics, engine details and JSON objects (object key order is irrelevant).
UUID letter case, UTC timestamp precision and IP spelling normalize through
PostgreSQL; omission of optional raw/warnings means `{}`/`[]`. Unknown keys,
case-mismatched property names, null nonnullable properties and invalid items
are rejected independently. No raw engine JSON or bearer value is logged.

Configuration joins the authenticated node and its schedule. Shared public
settings use individual `settings.key` rows: `engine` (default `ookla`),
`quietStart`/`quietEnd` (default empty), `timezone` (default `UTC`), and
`agentRelease` (default null, otherwise the contract's AgentRelease object).
Settings writers must validate these values against the contract. No peer-node
configuration is returned. The strong ETag hashes the complete effective
configuration, including shared settings and release metadata; weak validators,
validator lists and `*` work for `If-None-Match`. `revision` combines node and
schedule revisions. Update node/schedule revisions in their owning mutation
transactions; shared settings changes also change the ETag without relying on
those revisions. No new environment variables or migrations are required.

Heartbeat records the center receive time and agent version; uptime, lastTest
and optional updateResult are validated protocol observations, not additional
persistent node fields. Polling returns up to 100 oldest pending/running commands
in `(created_at,id)` order without claiming them. Agents deduplicate execution by
command ID. Acknowledgements lock the owned command, preserve omitted progress,
refuse decreasing progress or changes after a terminal state, and verify that a
referenced result belongs to the same node. Terminal retry preserves `ackedAt`.

`agentapi.ResultHook.AfterResultStored` runs only after a new result is inserted,
inside that result's transaction. The center wires `ipwatch.Watcher` to maintain
IP history and per-node/address counters atomically. Timelines use stored
`(ts,result UUID)` measurement order and include failed results with a known IP.
Failed results with a NULL IP are stored but skipped by history, counters,
geolocation and the change-signal outbox. Late observations
repair adjacent transitions but emit no alerts; only a new newest result with a
different IP emits a durable `ip_changed` signal for JOOST-29. Duplicate UUIDs
never run the hook. Hook errors roll back the result and all derived state.
See [IP history and the alert handoff](ip-history.md) for storage, geolocation,
late-arrival, consumer and rollback contracts. `/api/agent/ip` uses the socket
peer, or the existing trusted proxy policy
configured by `SPEEDTEST_TRUSTED_PROXY_CIDRS`; it performs no hostname lookup.

Verification (native Go; database suites must run serially on a verified worktree
DB, never `speedtest`):

```bash
python3 scripts/geo-test-fixtures.py
go test -race ./...
go test -race -p 1 -tags integration ./cmd/center ./internal/center/db/... ./internal/center/geo ./internal/center/nodes ./internal/center/server
# Dedicated paced HTTP + authentication + storage CPU measurement:
go test -p 1 -tags integration ./internal/center/server -run '^TestAgentIngestionLoad1000PerMinuteIntegration$' -count=1 -v
```

The load test commits 1,000 distinct full-field results over one minute and
builds/runs the production center executable and reports its process CPU seconds
/ elapsed seconds as a percentage of one CPU core (including startup/shutdown).
It includes HTTP handling, credential authentication and database client work;
CPU of the separate shared PostgreSQL service and load generator is excluded.
The test driver may use race/coverage instrumentation; the measured production
executable uses the normal build. Target: at most 5%.
Use structured access logs (path, request ID, status, duration) with `/readyz` to
distinguish authentication/rate limits (401/429), rejected input (item 422/409),
and storage failures (500). An item failure contains only a safe generic problem.

## Results CSV export

`GET /api/results/export.csv` takes the same filter and sort parameters as `GET /api/results` (`nodes`, `range` or `from`+`to`, `status`, `q`, `minDown`, `maxDown`, `sort`, `dir`) and no pagination. It streams every matching row in the requested order (default `ts` descending) as `text/csv; charset=utf-8` with `Content-Disposition: attachment; filename="speedtest-results-YYYY-MM-DD.csv"`. With neither `range` nor `from`/`to` it covers the trailing 7 days, as the list does.

Columns, in order: `time` (RFC 3339 UTC), `node` (name and label), `status`, `download_mbps`, `upload_mbps`, `ping_ms`, `jitter_ms`, `packet_loss_pct`, `bytes_down`, `bytes_up`, `bytes_total`, `loaded_latency_down_ms`, `loaded_latency_up_ms`, `server` (`name - city`), `public_ip`, `error`, `engine`. Byte values are exact integers; missing measurements are empty. Numbers are plain decimals and empty when the engine did not report them. Cells follow RFC 4180 quoting; a text cell that starts with `=`, `+`, `-`, `@`, tab or carriage return is prefixed with `'` so spreadsheets do not run it as a formula. A failure after the first row aborts the connection so a truncated download is never mistaken for a complete one.

## Schedules

Slots are Unix-epoch minutes: a node tests at `offset + k*interval`. `PUT /api/schedules/{node}` clamps the offset to `interval-1` instead of rejecting it, so changing an interval never needs a second request. `nextRun` and `upcoming` (up to 10) skip the quiet window of the rules and are empty while the node or its schedule is disabled. `bytesPerDay` is the average of `bytesDown + bytesUp` of the 200 most recent non-failed results times the runs in the next 24 hours outside quiet hours; `bytesPerMonth` is 30 times that, and both are zero without history or while paused.

A **clash** is a pair of enabled nodes whose tests can start less than 5 minutes apart at any time (found from the gcd of the two intervals, so mixed intervals are covered). With `maxConcurrent` above 1 a pair is reported only when more nodes overlap one of them than the limit allows. The coordination rules are stored in the settings rows `stagger`, `maxConcurrent`, `quietStart`, `quietEnd` and `timezone`. Saving rules, or `POST /api/schedules/stagger`, bumps the schedule revision of the affected nodes, which is part of the agent configuration revision, so agents pick the change up through the config ETag. Staggering spaces nodes of the same interval evenly in node-id order (`i * interval / count`).

## Full export and retention

`GET /api/export/results.csv` and `GET /api/export/results.json` stream every stored result, oldest first (`ts`, then `id`), whatever its age; query parameters are ignored. The CSV has the columns of the filtered export. The JSON is a bare array of `Result` objects (no envelope) written one element at a time, so memory use does not grow with the number of results. A failure after the first byte aborts the connection, so a truncated download cannot look complete.

A daily job (03:15 UTC) deletes results older than the `retentionDays` setting (default 365; `0` keeps everything) with `DELETE ... WHERE id IN (SELECT id ... LIMIT n)` batches of 5,000 and a short pause between full batches, so ingestion is never blocked. Each run logs `rows_deleted`, `batches`, `retention_days` and `duration_ms`.

## Dashboard and series

`GET /api/dashboard?range=6h|24h|7d|30d` (default 7d) returns the whole dashboard in one call: `current` and `previous` fleet figures (the previous period is the same length, directly before), a card per node (node, summary for the window, 48-point 24 hour download sparkline), the 9 newest results, the 6 newest unacknowledged alert events and the unacknowledged count. Fleet figures follow the preview: `down`/`up` are the unweighted mean of each node's percentage of its expected speed (nodes without data are left out), `ping` is the median of every `ok` test's latency, `success` is non-failed eligible tests divided by all tests except `rate_limited` results, and `rateLimited` is reported separately. `ips` counts address changes (the first address a node ever reports is not a change). An empty fleet returns empty arrays, zero counts and null aggregates.

`GET /api/series` returns comparison series with equally spaced buckets (`nodes`, `range` or `from`+`to`, `metric` down|up|ping|jitter|loss, `mode` absolute|percent, `maxPoints` 1–300, default 300; `timezone` is accepted for symmetry and does not change equal-width buckets). Each point is the mean of the non-failed samples in its bucket plus their count; a bucket without samples is a `null` gap. In percent mode each sample is divided by its own node's expected speed before it is averaged. The summary of a series is computed from the original samples of the window, not from the bucket means. Five 7-day series stay below 100 KB.

## IP and location views

All four routes need a session and only read. `GET /api/ip/history?node=` lists address changes newest first (the first address a node ever reports is not a change), with the location of the old and new address (`fromGeo`, `toGeo`, null when unknown). `GET /api/ip/addresses?node=` lists the distinct addresses each node has used, most recently seen first, with first/last seen, number of tests and the average download/upload of the non-failed tests (null when there were none). `GET /api/ip/map?node=` gives the most recently observed address of each node with its location, leaving out nodes that never reported one. Locations come from the cache of `ip_addresses`; an address whose lookup has not happened or found nothing has `geo: null`, never invented coordinates. Lists are paginated with `page` and `size`.

`GET /api/ip/lookup?ip=` accepts only an IPv4 or IPv6 literal (host names, ports, prefixes and zones answer 422 before anything is looked up) and runs the geolocation chain: cache, offline GeoLite2, then the configured fallback providers; a found location is cached. An address no source knows returns `data: null` with status 200.

## Settings

`GET /api/settings` (session) returns the owner's preferences with `ETag`; `PUT /api/settings` (session, CSRF, origin, `If-Match`) replaces them and answers 422 with field-level `errors` for invalid values, 428 without `If-Match` and 412 when the tag is stale. Before anything is saved the ETag is `"0"`, so the first save always moves it. Defaults match the UI preview (dark theme, 7d range, auto units, 1 decimal, 365 days retention, GeoLite2 with ip-api fallback, Ookla, 4 threads, concurrency 1, stagger on).

Secrets (the location-service key, email/Telegram/Slack credentials and the webhook URL) are write-only: they are encrypted with AES-GCM using `APP_KEY` from `.env` before they are stored and are never returned; the read model only says which are configured. Omitting a secret on PUT keeps the stored one.

Retention, default engine, quiet hours, the stagger flag and the concurrency limit are stored in their own `settings` rows (`retentionDays`, `engine`, `quietStart`, `quietEnd`, `stagger`, `maxConcurrent`), which are the rows the retention job, the agent configuration and the schedule rules read; the settings API and `PUT /api/schedules/rules` therefore always agree. Changing quiet hours, stagger, concurrency or the engine moves every schedule revision so agents refetch their configuration. A display time zone that is an IANA name is also stored as the quiet-hours zone; `browser` is a UI preference and never overwrites it.

`POST /api/settings/enrolment-key` (session, CSRF, origin) generates a new enrolment key, stores only its SHA-256 and returns the key once as `enrollToken`; later reads only report `enrollKeyConfigured`.

## Notification delivery (JOOST-30)

The test-channel POST and delivery list/detail GETs are implemented, with session
protection and CSRF/Origin checks for POST. Tests return 202 with a delivery
Location, or 422 for an unconfigured/disabled channel. Delivery lists accept
page/size and optional event UUID, newest first. No delivery response exposes a
destination, payload, URL or credential. Notification pages are limited to
1,000,000. See [notification delivery](notifications.md) for transport Settings,
retry/expiry behavior, the exactly-once transaction boundary and verification.

## Notification catalog (JOOST-78)

`GET /api/notifications/events` lists code defaults merged with owner settings.
`GET|PATCH /api/notifications/events/{key}` reads or updates an event, and
`GET|PUT /api/notifications/events/{key}/nodes/{id}` reads or updates enablement
for node-scoped events. Templates use `GET|PUT /api/notifications/templates/{key}/{channel}`;
preview and reset use `POST /previews` and `POST /resets` on that template path.
Writes require the owner session, CSRF token and same-origin request. See
[the notification catalog](notifications.md) for placeholders, formatting and defaults.

### Published agent releases

Agent identifiers accept `speedtest-agent/<version> (<goos>/<goarch>)`; legacy
identifiers default to linux/amd64. Owner `node.agent` omits the platform suffix.
`node.updateVersion` is the strictly newer advertised SemVer version, or null for
current/newer, development/prerelease or unsupported agents. With no explicit
`settings.agentRelease`, agent config `release` uses the node-platform entry in
the cached release manifest for older stable agents only, preventing automatic
downgrades. See [the manifest contract](release.md#release-advertisement-manifest).

## Independent engines (JOOST-71)

Nodes default to primary engine `ookla`. Heartbeats may report `engines` (a unique
array of `ookla` and `librespeed`, including an empty array); omitted capabilities
preserve the legacy capability record. Node views expose `engines` and
`primaryEngine`. A schedule for an unreported engine is rejected with 422.

`PUT /api/schedules/{node}` accepts `{ "engines": [ { "engine": "ookla",
"interval": 60, "offset": 0, "enabled": true }, { "engine": "librespeed",
"interval": 30, "offset": 5, "enabled": true } ] }` under the existing session,
CSRF, origin and If-Match requirements. Keep the Ookla entry (it may be disabled)
for rollout compatibility. Omitted intervals for new engine schedules default to 60 minutes. GET includes
an `engines` list of schedule views and the primary schedule compatibility fields.
The collection contains one view per node/engine. The legacy single-schedule PUT
continues to edit only Ookla. Agent config adds `schedules`; `schedule` and `engine`
remain the primary schedule and identity for older agents.

Results contain `engine`; old agents omitting it are recorded as Ookla. UUID
replay comparison includes engine identity. Results, filtered CSV, dashboard,
series and all statistics endpoints accept `?engine=ookla|librespeed`. Omission
selects each node's primary engine before aggregation; there is no all-engine
average. Full exports retain all measurements and their engine identity. CSV adds
`engine` as its final column. Alert rules optionally specify `engine`; omission
selects the node's primary engine, so another engine cannot advance or reset its
measurement streak.

## ISP plans and page filters

An assigned ISP plan supplies the node's expected download and upload speeds
when those values are omitted. The center records default provenance separately
for each speed. Editing a plan updates only values still using that plan default;
an owner-entered value remains an override even when it equals the advertised
speed. Existing assignments migrate as explicit values because their original
provenance cannot be inferred from numeric equality. Clear and reassign a plan
to opt an existing node into its advertised defaults. Clearing a plan keeps the
current expected speeds as node settings.

ISP trends use hourly buckets for windows up to 24 hours and daily buckets for
longer windows. Each point is timestamped at the average time of its samples, so
all points remain inside a rolling window, including across UTC midnight. The
Results and Alerts ISP selectors filter their own page data; they do not narrow
the shared snapshot used by dashboard, node, map or export views.
