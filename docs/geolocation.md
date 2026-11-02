# Geolocation lookup, fallback and database updates

`internal/center/geo` implements `Geo.Lookup(ctx, ip)` with MaxMind City and ASN
MMDBs using `oschwald/geoip2-golang` v1.13.0, as required by JOOST-33. `Resolver`
adds the ordered `ip_addresses` cache, offline reader, and online fallback chain
behind the same `Geo` interface. The package is a library; the center scaffold
has no server lifecycle yet. Its lifecycle owner supplies the shared PostgreSQL
pool and closes the reader after cancelling and joining its updater.

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
// Call resolver.Lookup(ctx, ip) from the center's geolocation consumer.
// Run blocks until cancellation; start it in the owner's lifecycle goroutine:
// reader.Run(ctx, geo.EnvironmentSettings{})
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

The resolver checks `ip_addresses` first, then GeoLite2, then enabled providers
in this order: ip-api.com and IPinfo Lite. Successful results are stored in the
existing `ip_addresses.geo` JSONB, `source`, and `looked_up_at` columns. Cache
entries are write-once: later provider responses cannot replace a completed
lookup. Concurrent requests for the same IP in one center process share one
lookup. Invalid addresses never reach PostgreSQL or an online provider. A cache
read/write failure is returned to the caller so a successful upstream result is
not reported complete without its durable cache entry. Provider failures do not
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

IPinfo Lite is optional and requires `SPEEDTEST_GEO_IPINFO_TOKEN`. The free Lite
API is authenticated and currently has no request cap, but returns country and
basic ASN only (no city or coordinates). IPinfo requires attribution when Lite
data is shown publicly. See [IPinfo Lite API](https://ipinfo.io/developers/lite-api)
and its [free-plan details](https://support.ipinfo.io/hc/en-us/articles/30792535492626-What-Is-the-Usage-Limit-for-the-Free-Plan).
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
updater, restore previously backed-up City/ASN MMDBs to configured paths, then
reopen the reader. This ticket has no SQL migration.

## Verification

Fixtures are public test-only data from MaxMind's `MaxMind-DB` repository,
pinned to commit `d1551916cff0712d12ff602c309234471b0402a2`. They are downloaded
only into ignored `.tmp/` and SHA-256 verified; no MMDB is committed. Tests fail
with an actionable message if the fixtures are missing. The fetching script is
idempotent and works offline after the verified files are cached.

```sh
python3 scripts/geo-test-fixtures.py
# Unit coverage uses local MMDB fixtures and loopback HTTP test servers.
go test -race -coverprofile=.tmp/geo.cover ./internal/center/geo
go tool cover -func=.tmp/geo.cover
go test ./...
go vet ./...
go build ./...
```

`make test` and CI prepare the fixtures before testing. Unit tests use local
files and loopback `httptest` servers, with fake credentials. They make no
licensed MaxMind download, database connection, or browser request. The
database integration test is separate (`go test -tags integration
./internal/center/geo`) and requires a migrated `speedtest_wt34` or
`speedtest_test` database; it verifies the project ownership marker and rolls
back its fixture transaction. The change touches no UI flow, so browser,
layout, and theme checks are not applicable. Real provider calls and staging
lifecycle integration require owner-authorized staging deployment; this library
does not deploy itself.

References: [MaxMind updates](https://dev.maxmind.com/geoip/updating-databases/),
[download API](https://github.com/maxmind/openapi/blob/main/bundled/downloads.yaml),
[reader](https://github.com/oschwald/geoip2-golang),
[test fixtures and licences](https://github.com/maxmind/MaxMind-DB/tree/d1551916cff0712d12ff602c309234471b0402a2/test-data).
