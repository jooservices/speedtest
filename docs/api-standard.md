# REST API standard

This page states the rules the command-center API follows. It is the reference for integrators and for anyone judging a
change to the API. The operations themselves are in [openapi.yaml](openapi.yaml); the wire details and the feature
notes are in [api.md](api.md). The rules here describe what the spec and the server already do; they change no
runtime behaviour.

## Versioning and alias policy

- The canonical surface is `/api/v1`. Every operation of [openapi.yaml](openapi.yaml) under `/api` is served under
  `/api/v1` with the same suffix, for example `/api/nodes/{id}` is `/api/v1/nodes/{id}`. Integrators and agents should
  use `/api/v1`.
- Each `/api` operation in the spec is marked `deprecated: true` and names its canonical form in
  `x-canonical-path`. The `/api` paths remain as deprecated aliases and keep working.
- A deprecated alias answers with the same status, body and headers as its `/api/v1` form, except that only the alias
  carries `Deprecation` and `Sunset`. The parity test in `internal/center/server/api_versioning_test.go` checks this for
  every operation and reads each canonical path from `x-canonical-path`.
- A breaking change needs a new major prefix, `/api/v2`, and a coordinated migration; the spec says so in its
  `info.description`.
- The alias headers are the constants in `internal/center/server/api_versioning.go`. `Deprecation` is the RFC 9745
  value `@1794873600` (2026-11-17 00:00 UTC). `Sunset` is the RFC 9110 HTTP-date after which the `/api` alias stops
  being served.
- **The Sunset date is a placeholder.** The value `Wed, 30 Jun 2027 00:00:00 GMT` in `api_versioning.go` is not a
  decision. The owner sets the real date; changing it is a one-line edit of that constant. Until then, integrators must
  not treat that date as a commitment.
- `/install.sh` and the download paths are not under `/api`. They have no `/api/v1` form and are outside this policy.

## Resource paths

- A path names a resource in the plural: `/api/nodes`, `/api/results`, `/api/alerts/rules`. A single resource is
  `{id}` under its collection, for example `/api/nodes/{id}`.
- Sub-resources nest under their parent: `/api/nodes/{id}/commands/{commandId}`.
- Path segments are lowercase, and words are joined with hyphens: `/api/nodes/{id}/server-selection`.
- A verb segment appears only for an action that is not a plain create, read, update or delete, for example
  `/api/nodes/{id}/run`, `/api/alerts/events/{id}/ack` or `/api/notifications/test/{channel}`.
- Paths are written without a trailing slash. A few owner routes also answer the slash form; they are listed in
  [api.md](api.md#api-versioning-joost-108), and other slash forms get no alias headers.
- A path with an empty segment (`//`) is never served as an operation.

## Methods

- `GET` is safe and has no side effects. Responses use `Cache-Control: no-store`, except conditional agent
  configuration, which uses `private, no-cache`.
- `POST` creates a resource or runs an action. A create answers `201` with a `Location` (see below); an action that
  runs in the background answers `202` and names the resource to poll.
- `PUT` replaces the documented fields of a resource.
- `PATCH` changes only the fields it sends. It uses `application/merge-patch+json`, and a `null` is rejected for a
  field that cannot be null.
- `DELETE` is idempotent: a repeated delete has no further effect. For a node, the repeat answers `404`.
- A wrong method answers `405` with an `Allow` header.

## Status codes

| Code | Meaning |
| --- | --- |
| 200, 201, 202 | Success. `201` and `202` include `Location` where the resource is addressable. |
| 204, 304 | No body. `304` answers a conditional `GET` whose `If-None-Match` matches the ETag. |
| 400 | Malformed input. |
| 401 | Authentication required or failed. |
| 403 | Authorization failed, or a CSRF or origin check failed. |
| 404 | Resource hidden or missing. Unknown `/api/` paths use the same 404 problem shape. |
| 405 | Wrong method; the response carries `Allow`. |
| 409 | State conflict, or an `Idempotency-Key` reused with a different body. |
| 412 | `If-Match` is stale. |
| 413 | Batch too large. |
| 422 | Validation failed; field-level `errors` are listed. |
| 428 | Required `If-Match` is missing. |
| 429 | Rate limited; the response carries `Retry-After`. |
| 500 | Safe server error. |

## Conditional requests and concurrency

- Editable resources return an `ETag` on `GET`. An update of such a resource sends that value in `If-Match`.
- A missing `If-Match` answers `428`. A stale one answers `412`. Both use problem types such as
  `/problems/preconditionrequired` and `/problems/stale`.
- Agent configuration is the exception for reads: it returns an `ETag` and accepts `If-None-Match`, answering `304`
  with no body when the tag still matches.
- A retryable create may send `Idempotency-Key`. The original status, body and `Location` are replayed for the same
  principal, key and body for 24 hours. A different body with the same key answers `409`.

## Errors

- Errors are RFC 9457 problem objects with the media type `application/problem+json`.
- `type` is a stable category URI of the form `/problems/<category>`, for example `/problems/validation` or
  `/problems/two-factor-required`. Clients branch on `type` and the status, never on the human `title` or `detail`.
- Every response carries `X-Request-ID`. A problem response repeats that value as `trace_id`.
- Success bodies use the `Success` envelope: `success`, `code`, `message`, `data`, `meta` and `errors: null`.

## Location and addressable resources

- A `201` or `202` response to a create or an action names the new resource in `Location`.
- A `202` caller polls the named command, delivery, installation or job resource until it is done.

## Timestamps, units and lists

- Timestamps are RFC 3339 in UTC with `Z`.
- Units are fixed on the wire: `Mbps` for bandwidth, `Milliseconds` for latency and jitter, and `Percent` for percentages.
  Display units never change wire values. `null` means unavailable; `0` is a real measurement.
- A list that grows is paginated with `page` (1-based) and `size` (default 25, maximum 100), and reports
  `meta.pagination`.

## Where the details live

- [api.md](api.md) holds the wire rules in full, the feature notes and the API versioning notes.
- [openapi.yaml](openapi.yaml) is the operation contract. Its `x-canonical-path` and `deprecated` fields carry the
  versioning policy above.
- Deviations from these rules are catalogued with a decision in [Deviations and decisions](#deviations-and-decisions).

## Deviations and decisions

`node scripts/api-audit.mjs` applies the rules above to [openapi.yaml](openapi.yaml) (the operations under `/api`;
`/install.sh` is outside this policy). Run `node scripts/api-audit.mjs --list` to see the findings, and
`node scripts/api-audit.mjs --check` to verify the table. The check exits 1 when a finding has no row, when a row names
no current finding, or when an exception operation has no `x-rest-exception` that starts with the finding key.

The rules the audit applies, and the finding key each one writes:

| Rule | Key | Applies to |
| --- | --- | --- |
| Segments are lowercase letters, digits and hyphens | `path-charset <path>` | every path segment |
| No empty segment and no trailing slash | `path-shape <path>` | every path |
| A segment before an `{id}` and a collection `GET` name the plural | `plural <path>` | every path |
| A collection `GET` accepts `page` and `size` | `list-page GET <path>` | a `GET` on a plural path |
| A `GET` takes no request body | `get-body GET <path>` | every `GET` |
| A `PATCH` accepts `application/merge-patch+json` | `patch-media PATCH <path>` | every `PATCH` |
| A `PUT` or `PATCH` on an item sends `If-Match` (answers 428) | `if-match <METHOD> <path>` | every `PUT` and `PATCH` on a path with `{param}` |
| An action (`ack`, `run`, `enroll`, `login`, `logout`, `stagger`, `heartbeat`, `resets`, `previews`, `test`) is a `POST`; before an `{id}` it is not a collection | `action-method <METHOD> <path>` | the last segment |
| Each status code is in the standard's table | `status <METHOD> <path> <code>` | every response |
| A `201` or `202` names the resource in `Location` | `location <METHOD> <path> <code>` | every `201` and `202` |
| An error is `application/problem+json` | `problem <METHOD> <path> <code>` | every 4xx and 5xx |
| Each `/api` operation is deprecated and names `/api/v1` plus the same suffix | `version <METHOD> <path>` | every `/api` operation |

Each finding has one row below with a decision:

- `fix`: part 2 changes the operation to follow the rule. The `/api` alias policy above is unchanged.
- `alias`: part 2 adds the compliant form and keeps the current one as a deprecated alias, so clients do not break.
- `exception`: the operation keeps its form. Each operation it covers carries `x-rest-exception` in
  [openapi.yaml](openapi.yaml), a string or list of strings that starts with the finding key and a colon.

| Finding | Decision | Reason |
| --- | --- | --- |
| `plural /api/auth/me` | exception | Singleton: the caller's own session identity. |
| `plural /api/auth/2fa` | exception | Singleton: the caller's own two-factor status. |
| `location POST /api/auth/2fa/setup 201` | exception | Creates a pending secret for the caller; no other resource is addressable. |
| `status POST /api/auth/2fa/setup 503` | fix | The 503 response points at the ServerError component (a 500); part 2 gives it the right code. |
| `location POST /api/agent/enroll 201` | fix | The enrolled node is addressable as `/api/v1/nodes/{id}`; name it in `Location`. |
| `list-page GET /api/nodes/{id}/servers` | fix | The discovered server list grows with discovery; add `page` and `size`. |
| `location POST /api/nodes/{id}/credential 201` | fix | The credential is addressable as `/api/v1/nodes/{id}/credential`; name it in `Location`. |
| `status POST /api/agent/results 207` | exception | Batch ingest returns per-item outcomes; 207 is not in the standard's table. |
| `plural /api/agent/config` | exception | Singleton: the caller's own effective configuration. |
| `list-page GET /api/agent/commands` | exception | Returns at most 100 pending commands by design; the agent polls its own queue. |
| `plural /api/agent/ip` | exception | Singleton: the caller's observed public IP. |
| `path-charset /api/results/export.csv` | exception | Download; the extension names the file format for the client. |
| `plural /api/results/export.csv` | exception | Download of a file, not a collection. |
| `path-charset /api/export/results.csv` | exception | Full export download; the extension names the file format. |
| `plural /api/export/results.csv` | exception | Download of a file, not a collection. |
| `path-charset /api/export/results.json` | exception | Full export download; the extension names the file format. |
| `plural /api/export/results.json` | exception | Download of a file, not a collection. |
| `plural /api/reports/monthly` | exception | A report for one UTC month, not a collection. |
| `list-page GET /api/isps` | fix | The ISP list grows as new ISPs are seen; add `page` and `size`. |
| `plural /api/isps/compare` | exception | Computed comparison of selected ISPs in one period. |
| `plural /api/isps/{id}/trend` | exception | Computed trend for one ISP. |
| `list-page GET /api/isps/{id}/plans` | exception | Plans are entered by the owner per ISP; the list is not paged in v1. |
| `location POST /api/isps/{id}/plans 201` | fix | The plan is addressable as `/api/v1/isps/{id}/plans/{planId}`; name it in `Location`. |
| `if-match PUT /api/isps/{id}/plans/{planId}` | fix | Plans are edited without a version; add an ETag on read and `If-Match` with 412 and 428. |
| `plural /api/stats/summary` | exception | Computed statistics view, not a collection. |
| `plural /api/stats/trend` | exception | Computed statistics view, not a collection. |
| `plural /api/stats/hour-of-day` | exception | Computed statistics view, not a collection. |
| `plural /api/stats/heatmap` | exception | Computed statistics view, not a collection. |
| `plural /api/stats/distribution` | exception | Computed statistics view, not a collection. |
| `plural /api/usage` | exception | Computed traffic view by node, engine and period. |
| `plural /api/usage/advice` | exception | Estimate computed from the previous 30 days of results. |
| `list-page GET /api/usage/budgets` | exception | One budget per scope; the list is not paged in v1. |
| `status GET /api/usage/budgets 503` | exception | The budget store is temporarily unavailable; no standard code covers that. |
| `list-page GET /api/stats/events` | exception | Returns the newest 500 marks, a bounded list. |
| `plural /api/dashboard` | exception | Single snapshot of the fleet, not a collection. |
| `list-page GET /api/series` | exception | Bounded series of at most `maxPoints` per node. |
| `list-page GET /api/schedules` | fix | Per-node schedules grow with the fleet; add `page` and `size`. |
| `plural /api/audit` | alias | The owner change log is a collection; add `/api/v1/audit-entries` and keep `/api/audit` as an alias. |
| `action-method PUT /api/alerts/events/{id}/ack` | alias | Clients send PUT today; add `POST /api/v1/alerts/events/{id}/ack` and keep the PUT. |
| `list-page GET /api/notifications/events` | exception | The event catalog is bounded by the events the product defines. |
| `patch-media PATCH /api/notifications/events/{key}` | fix | Accept `application/merge-patch+json`, as `PATCH /api/settings` does. |
| `if-match PATCH /api/notifications/events/{key}` | fix | The event setting is editable; accept `If-Match` with 412 and 428. |
| `if-match PUT /api/notifications/events/{key}/nodes/{id}` | fix | The per-node setting is editable; accept `If-Match` with 412 and 428. |
| `if-match PUT /api/notifications/templates/{key}/{channel}` | fix | The template is editable; accept `If-Match` with 412 and 428. |
| `list-page GET /api/settings` | exception | Singleton: the preferences object, not a collection. |
| `location POST /api/settings/enrolment-key 201` | exception | Rotates the shared bootstrap key (singleton); no other resource is addressable. |
| `plural /api/ip/history` | fix | `history` names an event log; rename to `/api/v1/ip/changes`. |
| `plural /api/ip/lookup` | exception | Read of the location of one literal IP, not a collection. |
| `list-page GET /api/ip/providers` | exception | The configured providers, a bounded set. |
| `plural /api/ip/map` | exception | Computed map of the current location per node. |
| `location POST /api/installer/probes 201` | fix | The probe is stored; name its resource in `Location`, or document why there is none. |
| `list-page GET /api/installations/{id}/events` | exception | Server-sent event stream of one installation, not a paged list. |
