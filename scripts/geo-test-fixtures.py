#!/usr/bin/env python3
"""Fetch pinned public MaxMind test fixtures; never track database files."""
import hashlib
from pathlib import Path
from urllib.request import urlopen

ROOT = Path(__file__).resolve().parents[1]
COMMIT = "d1551916cff0712d12ff602c309234471b0402a2"
FIXTURES = {
    "GeoIP2-City-Test.mmdb": "1394af009121820d43b620fea19fc4b6f09141698091c00f70f225a982389035",
    "GeoLite2-ASN-Test.mmdb": "a0f9e8614ad582ba1af3eeb18ead666f73ad6e4cbd5280f80d5d4d95bbe6c3f1",
}
IPINFO_COMMIT = "3cf4e76d882af7b3beca24817841ad0a0a4fa1e9"
IPINFO_FIXTURE = {
    "IPinfo-Lite-Test.mmdb": "4f8a5824a59952a97684b256aeac632ac9fd2b1ecab15fedfe60fbf492e2fbb7",
}
for name, digest in FIXTURES.items():
    target = ROOT / ".tmp" / name
    data = target.read_bytes() if target.exists() else b""
    if hashlib.sha256(data).hexdigest() != digest:
        with urlopen(f"https://raw.githubusercontent.com/maxmind/MaxMind-DB/{COMMIT}/test-data/{name}", timeout=60) as response:
            data = response.read(1 << 20)
        if hashlib.sha256(data).hexdigest() != digest:
            raise SystemExit(f"Checksum mismatch: {name}")
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(data)
    print(f"Verified {name} SHA-256 {digest}")
for name, digest in IPINFO_FIXTURE.items():
    target = ROOT / ".tmp" / name
    data = target.read_bytes() if target.exists() else b""
    if hashlib.sha256(data).hexdigest() != digest:
        url = f"https://raw.githubusercontent.com/ipinfo/sample-database/{IPINFO_COMMIT}/IPinfo%20Lite/ipinfo_lite_sample.mmdb"
        with urlopen(url, timeout=60) as response:
            data = response.read(1 << 20)
        if hashlib.sha256(data).hexdigest() != digest:
            raise SystemExit(f"Checksum mismatch: {name}")
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(data)
    print(f"Verified {name} SHA-256 {digest}")
