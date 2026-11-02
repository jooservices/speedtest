# Speedtest API contract

[openapi.yaml](openapi.yaml) is the OpenAPI 3.1 source of truth for JOOST-9.
It defines the product API endpoints; the static `ui/` preview still runs on
local demo state. The command center also exposes the operational `/healthz`
and `/readyz` checks outside that product API contract. When database settings
are configured, readiness pings PostgreSQL and reports `ok` or `unavailable`;
without database settings it reports `not configured`.
See the [OpenAPI specification](https://spec.openapis.org/oas/v3.1.0) and
[Redocly lint command](https://redocly.com/docs/cli/commands/lint).

## Authentication and authorization

UI requests use the opaque `speedtest_session` cookie, with `HttpOnly`,
`SameSite=Lax`, `Path=/`, and `Secure` over HTTPS. The session is stored server-side;
passwords use argon2id. Every authenticated UI session has the single owner's
permissions; this contract does not introduce roles or SSO.

1. `GET /api/auth/me` without a cookie establishes an anonymous session and returns
   `user: null`, `csrfToken`, `expiresAt`, and `Set-Cookie`.
2. Send that cookie and `X-CSRF-Token` with `POST /api/auth/login`. Login failures
   are generic; rate limits apply per IP and account. Success rotates the session
   and CSRF token and returns the authenticated `Session`.
3. Every UI write, including login/logout, requires the session-bound CSRF header
   and Origin validation. An expired/revoked cookie returns 401, including on
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
Deduplication is by node and preallocated result UUID; an identical replay cannot
create another result. Retry transport failures/429/5xx with backoff and jitter;
never discard unacknowledged spool entries. Failed results require an error and
allow null measurements; ok/degraded require down/up/ping/serverId and `error: null`. Engines may omit
loss/jitter/loaded latency even on success; preserve those as null. Optional
loadedDown/loadedUp retain low/iqm/high; engine details retain ISP, result ID/URL,
external IP and mismatch warnings, with remaining original fields in raw.
After each successful measurement, the agent calls `GET /api/agent/ip` over the
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
| Statistics, JOOST-26/39 | `/api/stats/{summary,trend,hour-of-day,heatmap,distribution}`; `SummaryComparison`, `MetricSummary`, `Series`, `HourSeries`, `Heatmap` counts, `Distribution` edges/count/percent; combined total is display-only |
| IP & location, JOOST-33/34/35/36/39/42 | `/api/ip/{history,addresses,lookup,map,service,database-updates,refreshes}`; `IPChange`, `IPAddress`, `Geo` (all location/ASN/ISP/accuracy/source fields), `MapLocation`, database timestamp/cache count, maintenance `Job` |
| Schedules, JOOST-28/39 | `/api/schedules`, per-node GET/PUT, rules PUT, stagger POST; `ScheduleInput`, `ScheduleRules`, `Schedules` clashes/upcoming/nextRun/bytesPerDay/bytesPerMonth |
| Alerts, JOOST-29/40 | rules CRUD, events list/detail/ack, snapshot acknowledge-all job; `AlertRule`, `AlertEvent` title/detail/severity/ts/resolvedAt/acked; reopen means acked=false |
| Settings/notifications, JOOST-30/31/40 | Settings GET/PUT/key rotation, test-channel POST, delivery logs/status; `Settings`, `NotificationSettings`, `Delivery`, issuance-only `IssuedKey` |
| Installer distribution, JOOST-46 | public `GET /install.sh`; checksum-verifying shell script |

Field names follow `seed.js` and `store.js`: Node `name`, `label`, `provider`,
`plan`, `colorSlot`, `expectedDown`, `expectedUp`, `interval`, `offset`, `threads`,
`server`, `enabled`, `host`, `agent`, `note`, `createdAt`; Result `id`, `nodeId`,
`ts`, `serverId`, `threads`, `ip`, `status`, `error`, `down`, `up`, `ping`, `jitter`,
`loss`, `dLat`, `uLat`, `bytesDown`, `bytesUp`, `duration`, `manual`. Real IDs are
opaque (result IDs are UUIDs); demo IDs are not wire IDs. `profile`, `ipModel`,
`pref`, `OUTAGES` and RNG state are simulation inputs, never API fields. Demo
load-balancer labels remain plain text; IP changes are counted for every node.
The adapter maps demo `abs`/`pct` chart modes to API `absolute`/`percent`, resolves
`browser` timezone for analytics, and does not read secrets back from Settings.
Reset demo data remains a local preview action, with no destructive API endpoint.

Statistics follow the UI formulas: summaries include nonfailed samples;
trend/hour-of-day/heatmap/distribution include only `ok` samples as `stats.js`
does. Percentiles interpolate at `(n-1)*p`, standard deviation is population,
and stability is `clamp(100-220*std/mean,0,100)`, null for zero/no mean. Counts
include failures; success rate includes degraded tests. Fleet percentages are
unweighted means of node percentages; fleet latency is median of ok-only pings.
Summary previous periods have equal adjacent duration. Missing chart buckets
have null value/count=0. Timezone grouping uses IANA local calendar boundaries,
including DST; heatmap is Monday=0..Sunday=6 × hour 0..23, last 30 days. Combined
series require all selected nodes to have values; they do not persist topology.

Settings defaults match the preview: dark, 7d, auto units, browser timezone,
24-hour clock, one decimal, 365-day retention (0=forever), GeoLite2 + ip-api fallback,
weekly updates, Ookla/auto server/4 threads, stagger on/maxConcurrent=1, empty
quiet hours, email enabled to `ops@example.com`, other channels off/empty.
Public center URL comes from deployment `.env`, not the demo URL. New nodes
start at interval=30/offset=0, enabled, threads=4, server=auto. Schedule PUT clamps
offset after interval changes; effective quiet timezone is explicit. Usage uses
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
  check('ResultInput', select(r, 'ResultInput'));
  r.raw = {}; r.server = plain(ST.server(r.serverId)); r.geo = plain(ST.geo(r.ip));
  check('Result', r); check('Geo', r.geo);
  r.status === 'failed' ? failed++ : good++;
}
assert(failed > 0 && good > 0);
const sample = plain(ST.state.results.find(r => r.status === 'ok'));
sample.id = '00000000-0000-4000-8000-000000000001';
sample.ts = new Date(sample.ts).toISOString(); sample.error = null; sample.manual = false;
const input = select(sample, 'ResultInput');
check('ResultInput', { ...input, loss: 101 }, false);
check('ResultInput', { ...input, down: -1 }, false);
check('ResultInput', { ...input, down: null }, false);
check('ResultInput', { ...input, loss: null, jitter: null, dLat: null, uLat: null });
check('ResultInput', { ...input, ts: '2026-11-03T07:00:00+07:00' }, false);
check('ResultInput', { ...input, status: 'failed', error: '' }, false);
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
  '/api/nodes', '/api/results', '/api/stats/summary', '/api/stats/trend',
  '/api/stats/hour-of-day', '/api/stats/heatmap', '/api/stats/distribution',
  '/api/dashboard', '/api/series', '/api/schedules/{node}', '/api/alerts/rules',
  '/api/alerts/events', '/api/notifications/test/{channel}', '/api/settings',
  '/api/ip/history', '/api/ip/addresses', '/api/ip/lookup', '/api/ip/map',
  '/api/auth/login', '/api/auth/logout', '/api/auth/me', '/api/auth/password'
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

Contract-only changes have no changed Go package, SQL boundary, running endpoint
or browser flow: Go coverage/vet, database integration, three browser runs,
staging/agent soak and migrations are inapplicable here. Claude reviews this
contract and UI coverage; implementation tickets must prove runtime behavior.
No push is requested, so pre-push Sonar/Gitleaks gates are not invoked here.

Compatibility: `/api` is the initial v1 surface fixed by the backlog; do not add
an incompatible prefix for these consumers. Optional response fields/endpoints
may be added. Renames, removals, type/meaning changes, tighter accepted inputs or
changed status semantics require a coordinated migration and a new major prefix
(e.g. `/api/v2`), with a documented consumer transition/deprecation window.
