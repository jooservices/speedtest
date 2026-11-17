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
- Deviations from these rules are not catalogued here yet. Finding and fixing them is out of scope for JOOST-109.
