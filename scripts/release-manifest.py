#!/usr/bin/env python3
"""Write the release contract from the exact build stamp and checksum inventory."""
import json
import pathlib
import sys


def write_manifest(directory, version, commit, built_at):
    directory = pathlib.Path(directory)
    files = {}
    for line in (directory / "SHA256SUMS").read_text().splitlines():
        checksum, name = line.split()
        files[name] = checksum
    manifest = dict(version=version, commit=commit, builtAt=built_at, files=files)
    (directory / "release.json").write_text(json.dumps(manifest, indent=2) + "\n")


if __name__ == "__main__":
    write_manifest(*sys.argv[1:])
