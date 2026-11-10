# Geolocation lookup, fallback and database updates

`internal/center/geo` combines local City and ASN MMDB readers, an optional
IPinfo Lite MMDB, the `ip_addresses` PostgreSQL cache, and configured online
providers. The center owns the reader, durable quota store and background
workers; it cancels and joins their goroutines before closing the readers.

```go
reader, err := geo.Open(geo.ConfigFromEnv(), slog.Default())
if err != nil {
    return err
}
providerConfig, err := geo.ProviderConfigFromEnv()
if err != nil {
    return err
}
providers, err := geo.NewFallbackProviders(providerConfig)
if err != nil {
    return err
}
resolver := geo.NewResolver(geo.NewPostgresCache(pool), reader, providers...)
// The center also composes the PostgreSQL quota store and resumable backfill.
// Run blocks until cancellation; the center starts and joins these loops:
// reader.Run(ctx, geo.EnvironmentSettings{})
// reader.RunIPInfo(ctx)
// backfill.Loop(ctx)
// Cancel and join that goroutine before reader.Close().
```

Lookup reads local memory only; IP addresses never leave the process. The result
contains English city/region, ISO country code, latitude/longitude, IANA time
zone, accuracy radius in kilometres, ASN and organisation. City and ASN coverage
can differ: fields absent in one edition remain zero/empty; both databases must
be available, and an IP absent from both returns `ErrNotFound`. Invalid addresses
return `ErrInvalidIP`, canceled contexts return their context error. Missing or
invalid databases return `ErrNotFound` and one warning per reader, not per IP.
Concurrent lookups, replacements and repeated closes are safe.

## Fallback providers and cache

The resolver checks `ip_addresses` first, then offline MMDB data, then enabled
providers in this order: ip-api, IPinfo Lite, ipwho.is, freeipapi, GeoJS,
ipapi.co, ipgeolocation.io and RIPEstat. A cached entry below quality score 85
can be enriched. Existing fields are replaced only by a higher-ranked source;
missing fields are filled and field-level provenance is retained. Concurrent
requests for the same IP in one center process share one lookup. Invalid
addresses never reach PostgreSQL or an online provider. A cache read/write
failure is returned to the caller so a successful upstream result is not
reported complete without its durable cache entry. Provider failures do not
stop later providers in the chain; caller cancellation does.

The providers use a timeout that bounds both rate-limit waiting and HTTP I/O,
plus independent circuit breakers.
After the configured number of transport, HTTP, timeout, or malformed-response
failures, a provider is skipped until its reset period expires; one half-open
request checks recovery. A provider's `not found` response is treated as a
healthy answer and does not open the breaker. Error messages do not include
request URLs, response bodies, or tokens.

The free ip-api JSON endpoint is HTTP-only, allows 45 requests per minute per
client IP, and requires clients to stop when `X-Rl` reaches zero until `X-Ttl`
expires. The built-in limiter spaces requests at no more than 45/minute and
honours both headers. Its free endpoint is not licensed for commercial use;
operators must use it only where its terms permit. [ip-api JSON API and limits](https://ip-api.com/docs/api%3Ajson)
and its [free-use terms](https://ip-api.com/docs/legal) are authoritative.

IPinfo Lite is opt-in for online lookups and requires both
`SPEEDTEST_GEO_IPINFO_ENABLED=true` and `SPEEDTEST_GEO_IPINFO_TOKEN`. Its
authenticated Lite API has no published request cap and returns country and
basic ASN only (no city or coordinates). With a token, the center also refreshes
the offline IPinfo Lite MMDB daily; this can fill country/ASN gaps without
sending individual IP lookups over the network. Database downloads are
HTTPS-only outside loopback, validated as IPinfo Lite MMDBs and atomically
installed; a bad download retains the previous file and reader. IPinfo requires
attribution when its data is shown publicly. See [IPinfo Lite API](https://ipinfo.io/developers/lite-api),
[database downloads](https://ipinfo.io/developers/data-download), and its
[free-plan details](https://support.ipinfo.io/hc/en-us/articles/30792535492626-What-Is-the-Usage-Limit-for-the-Free-Plan).
Set `SPEEDTEST_GEO_IPINFO_REQUESTS_PER_MINUTE` to add local pacing if needed;
zero follows the provider's published unlimited request policy.

All provider controls and defaults are documented in `.env.example`. The
IPinfo token has no default and must remain private. Disabled providers are not
constructed or called. Only successful lookups are cached, so a transient
network or database failure can be retried on a later request.

All configuration is documented in `.env.example`. Paths default to
`data/geoip/GeoLite2-{City,ASN}.mmdb`, relative to the center working directory.
The process must have write permission to their directories. Files are 0600 and
new directories 0700; downloaded databases and secrets are ignored by Git.
Environment variables must be exported by the caller (`.env` is configuration
input for the deployment environment, not parsed by this package).

`UpdaterSettings` has `LicenseKey(context.Context) (string, error)` and
`AutoUpdate(context.Context) (bool, error)`. The environment adapter reads the
current values on every check. JOOST-31 can provide stored Settings without
changing this package. `Run` checks on startup and every seven days; disabling
auto-update prevents all downloads but keeps existing lookups working. A missing
key produces a single warning per reader and skips downloads. Set a key and
leave auto-update enabled to recover on the next check, or call `Update` once.

The download origin is configurable. HTTPS is mandatory except loopback HTTP
for tests. With `SPEEDTEST_GEOLITE2_ACCOUNT_ID`, the updater uses MaxMind's
current Basic Auth download API. Without it, it uses the backwards-compatible
`app/geoip_download` licence-key API. Redirects must use HTTPS; authentication
headers are removed on redirects to signed storage URLs. Errors and logs omit
request URLs, keys, response bodies and raw Settings errors. Do not attach an
HTTP transport that logs URLs or credentials.

Each edition is refreshed independently. The updater verifies the compressed
archive SHA-256, the gzip trailer, the tar entry and the MMDB edition before
writing a private temporary file on the target filesystem, syncing, closing and
renaming it over the target. Only then is the in-memory reader replaced under
the lookup lock. On any failure before rename, that edition's old file and
reader survive. One edition can succeed while the other fails; the returned
error identifies the failed edition. This is atomic per file, not a two-file
transaction. A power failure has the filesystem's rename durability semantics;
the in-process swap is safe for concurrent requests. Downloads are limited to
128 MiB compressed / 256 MiB decompressed, checksums to 4 KiB, HTTP requests to
two minutes, redirects to ten. Updates are serialized per Reader. Use one reader
and one updater lifecycle per configured pair of paths.

Operators see a successful update info event per edition and a failure error
from `Run`; no IP or key is logged. On failure, check connectivity, credentials,
path permissions and capacity, then call `Update` to retry. Archive and checksum
can refer to different releases during a vendor release: the checksum mismatch
safely retains that edition's old file; retry later. For rollback, stop/join the
updater, restore previously backed-up MMDBs to configured paths, then reopen the
reader. Migration `00016_geo_provider_usage.sql` adds durable provider budget
and resumable backfill state; its Down section removes both tables.

## Verification

Fixtures are public test-only data from MaxMind's `MaxMind-DB` repository,
pinned to commit `d1551916cff0712d12ff602c309234471b0402a2`. They are downloaded
only into ignored `.tmp/` and SHA-256 verified; no MMDB is committed. Tests fail
with an actionable message if the fixtures are missing. The fetching script is
idempotent and works offline after the verified files are cached.

```sh
python3 scripts/geo-test-fixtures.py
# Unit and race tests use local MMDB fixtures and loopback HTTP test servers.
go test -race ./internal/center/geo/... ./internal/center/ipwatch ./internal/center/ipapi
# Database tests use only this worktree's configured speedtest_wt<ID> database.
go test -p 1 -tags integration ./internal/center/geo/... -run 'Integration$' -count=1
E2E_ONLY=SC-SET-02 node tests/e2e/main.mjs
E2E_ONLY=SC-IP-01 node tests/e2e/main.mjs
```

Unit tests use local files and loopback `httptest` servers with fake credentials;
they make no licensed database download or real provider call. Integration
tests verify the project ownership marker, reset the worktree schema, apply the
current migrations and clean their fixture rows. Do not run database suites
concurrently against the same worktree database. The two focused E2E scenarios
cover provider budget visibility and quality/source details in IP views. The
full repository CI gate remains responsible for the complete browser and
integration suites. Live provider calls require deployment-specific credentials
and network access.

References: [MaxMind updates](https://dev.maxmind.com/geoip/updating-databases/),
[download API](https://github.com/maxmind/openapi/blob/main/bundled/downloads.yaml),
[reader](https://github.com/oschwald/geoip2-golang),
[test fixtures and licences](https://github.com/maxmind/MaxMind-DB/tree/d1551916cff0712d12ff602c309234471b0402a2/test-data).

## More providers, consensus and DB-IP Lite (JOOST-75)

Resolver order stays: durable cache, offline MMDB, then enabled online providers.
The default provider switches are documented in `.env.example`: ip-api and
online IPinfo Lite are off; ipwho.is, freeipapi and RIPEstat are on; GeoJS,
ipapi.co and ipgeolocation.io are off. ipgeolocation.io needs
`SPEEDTEST_GEO_IPGEOLOCATION_KEY` and HTTPS; the key travels only in the request
to that service and appears in no error or log line. Every provider checks that
the answer is for the requested IP. Low-value owner lookups use at most the
first configured fallback. Node public-IP lookups use all enabled providers
when consensus is enabled.

**Consensus** (`SPEEDTEST_GEO_CONSENSUS=true`, needs at least two providers): all enabled providers are asked, the country
most of them report wins (a tie goes to the earlier provider), and empty fields in that answer are filled from providers
that agree. The stored source records the contributing providers, for example `consensus:ipwho+freeipapi`. When providers
disagree, a warning lists the competing countries (never the IP). Location source and quality are shown in IP views.

**DB-IP Lite**: when no MaxMind licence key is set the center keeps `GeoLite2-City.mmdb` and `GeoLite2-ASN.mmdb` current
from the free DB-IP Lite files (same format, CC BY 4.0). It checks every 24 hours and replaces a file only when it is
missing or older than 25 days, tries the previous month when the current month is not published yet, verifies the
database before the atomic swap, and keeps the old file and reader when anything fails.
It is on by default in production (`APP_ENV=production`) and off elsewhere; `SPEEDTEST_GEO_DBIP_AUTO_UPDATE=true|false` overrides; with a MaxMind key the GeoLite2 updater runs instead. Wherever the
data is shown, the attribution **IP Geolocation by DB-IP** (https://db-ip.com) applies; this page and the application
documentation carry it. Provider settings remain environment-controlled; the owner Settings page reports each provider's
on/off state, whether a key is configured, and today's usage without returning key values.

## Quota-aware quality and backfill (JOOST-88)

The stored quality score is 0–100, combines location completeness, source and age, and uses 85 as the enrichment
threshold. It is recomputed from stored fields and lookup age on reads. Merge fills missing fields; it replaces a populated
field only when the incoming source ranks higher, and stores provenance for each field. Offline data is checked before
online requests. Node-address lookups may use consensus; interactive owner lookups are limited to one configured
provider to conserve free quotas. A lookup that adds no field value or better provenance does not renew the cached
observation time. Consensus votes only on known countries; a countryless RIPEstat ASN can fill an ASN gap while each
field keeps its actual contributing provider.

`geo_provider_usage` stores per-provider UTC-day request totals, current-minute counts and provider cooldowns in
PostgreSQL. Reservations are atomic across center processes and survive restarts. A daily limit of zero means no local
daily cap. Background work can use at most 80% of a configured daily quota, leaving the rest for interactive lookups.
The broker observes ip-api `X-Rl`, `X-Ttl` and HTTP 429 responses and blocks calls until the provider window expires.
The ip-api free endpoint defaults off; enabling it logs a warning that request IPs travel over plaintext HTTP and that
the free service is for non-commercial use. Single lookups are paced to 45/minute; batch requests are paced to 15/minute
and contain at most 100 IPs per request. The batch window has its own durable reservation key; batch calls atomically
reserve both that window and the shared ip-api request budget, and batch `X-Rl`/`X-Ttl` or 429 responses persist its
cooldown across restarts and center replicas.

At the configured UTC hour (02:00 by default), the resumable backfill selects at most 100 weak or aged addresses per
night, in bounded batches of up to 100. It includes unlooked placeholders, visits node addresses before lookup-only
addresses, and uses ip-api batching when enabled and otherwise the budgeted fallback chain. If ingestion finds every
provider out of budget, it commits the measurement and leaves an unlooked address for this scheduled retry path.
`geo_backfill_state` stores the node-priority/IP cursor across UTC days and budget stops; after the keyspace is fully
visited, the next pass starts from the highest-priority address again. Daily limits and batch size/pause are configurable
in `.env.example`.

`python3 scripts/geo-test-fixtures.py` verifies pinned public MMDB fixtures, including IPinfo's vendor sample database.
IPinfo Lite stores ASN values as strings such as `AS174`; the reader validates and converts that value while keeping
GeoLite fields available if the supplemental IPinfo record cannot be decoded.

`GET /api/ip/providers` is session-protected and reports provider state, key-configured state, daily limit and today's
request count. The Settings location panel never exposes a credential. Stored `ip_addresses.geo` uses `quality_score`
and `field_sources`; API responses use `qualityScore` and `fieldSources` so operators can see aggregate quality and
per-field sources.

The center exports bounded-label `speedtest_geo_lookups_total{provider,outcome}` counters and includes
`geo_backfill` in worker run/success metrics. Provider names and outcomes come from fixed allowlists; IP addresses and
keys are never metric labels.

## Free provider facts (read from the providers' own pages on 2026-11-10)

Limits change; check the page before relying on a number.

| Provider | Free limit | Key | HTTPS | Commercial use | Gives | Notes |
| --- | --- | --- | --- | --- | --- | --- |
| DB-IP Lite (offline MMDB) | none, monthly files | no | download | yes, CC BY 4.0 with attribution | country, city, coordinates, ASN | the base of the offline database |
| IPinfo Lite | no request limit | token | yes | yes, CC BY-SA 4.0 with attribution | country and ASN only; free MMDB/CSV download too | best source for ASN and country gaps |
| ip-api.com | 45 requests/minute; batch of 100 IPs per request, 15 batch requests/minute | no | **no (HTTP only)** | **no (non-commercial)** | city, region, coordinates, timezone, ISP, ASN | over the limit: HTTP 429, repeated: 1-hour ban; `X-Rl`/`X-Ttl` headers; IPs travel unencrypted |
| ipgeolocation.io | 1,000 credits/day, no per-minute limit | free key | yes | yes (within the limit) | country, state, city, coordinates, ASN, timezone | resets 00:00 UTC; calls from a server only |
| ipwho.is | 1,000 requests/day | no | yes | yes | country, region, city, coordinates, ASN, ISP, timezone | no bulk on the free plan |
| freeipapi.com | 60 requests/minute | no | yes | yes | country, region, city, ASN, timezone | European servers; no bulk on free |
| GeoJS | not published on the docs page | no | yes | see its terms | country, city, coordinates, ASN, organisation | accepts several IPs in one call |
| ipapi.co | 1,000 requests/day on a trial | trial | yes | **trial is "not meant for production"** | city, coordinates, ASN | keep off in production |
| RIPEstat network-info | no key; fair use | no | yes | public RIPE data | prefix and ASN only | possible ASN fallback |

Because of these limits the providers are used sparingly: the offline database answers first, and a free provider is asked
only when the offline answer is missing or weak, in the order and with the budgets of the quota-aware policy (JOOST-88).
