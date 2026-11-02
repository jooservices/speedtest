# Offline geolocation and database updates

`internal/center/geo` implements `Geo.Lookup(ctx, ip)` with MaxMind City and ASN
MMDBs using `oschwald/geoip2-golang` v1.13.0, as required by JOOST-33. The
package is a library; the center scaffold has no server lifecycle yet. Its future
lifecycle owner opens the reader, starts `Run` with a cancellable context, cancels
and joins it on shutdown, then closes the reader. No fallback, persistence,
Settings UI or HTTP endpoint is added here.

```go
reader, err := geo.Open(geo.ConfigFromEnv(), slog.Default())
if err != nil {
    return err
}
// Call reader.Lookup(ctx, ip) from the center's geolocation consumer.
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
# Follow the workspace pre-test database guard or an explicitly authorized
# exception for this entirely database-free suite.
go test -race -coverprofile=.tmp/geo.cover ./internal/center/geo
go tool cover -func=.tmp/geo.cover
go test ./...
go vet ./...
go build ./...
```

`make test` and CI prepare the fixtures before testing. Tests use only local
files and loopback `httptest` servers, with fake credentials. They make no
licensed MaxMind download, database connection, or browser request. The change
touches no UI flow, so browser/layout/theme checks are not applicable. Real
licensed downloads and staging lifecycle integration require the center runtime
and owner-authorized staging deployment; this library does not deploy itself.

References: [MaxMind updates](https://dev.maxmind.com/geoip/updating-databases/),
[download API](https://github.com/maxmind/openapi/blob/main/bundled/downloads.yaml),
[reader](https://github.com/oschwald/geoip2-golang),
[test fixtures and licences](https://github.com/maxmind/MaxMind-DB/tree/d1551916cff0712d12ff602c309234471b0402a2/test-data).
