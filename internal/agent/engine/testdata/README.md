# Fixture provenance

`legacy.json` preserves the measurements and absence of packetLoss/loaded latency
from a real Ookla CLI invocation published by its operator:
https://karneliuk.com/2021/01/tools-3-checking-your-connectivity-bandwidth-with-speedtest/amp/
(timestamp 2020-12-28T15:21:52Z). Identifying ISP, IP, server and result references
are replaced with documentation-only values; unrelated interface details omitted.
No real measurement is run by these tests.

Modern loaded-latency fixtures are emitted by JOOST-20's shared fake (see
`docs/testing-harness.md`). Failure objects in tests are representative synthetic
Ookla-style log objects, not claimed to be captured live output.

`librespeed.json` preserves a real `librespeed-cli --json` report published in
[LibreSpeed issue #26](https://github.com/librespeed/speedtest-cli/issues/26).
The client IP, hostname, ISP, city, region, country, coordinates, server name
and URL were replaced with documentation-only values; measurement values and
the CLI's JSON field layout are retained. The CLI emits reports as an array in
newer versions and as an object in older versions; parser tests cover both.
