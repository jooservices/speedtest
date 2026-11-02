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
