#!/usr/bin/env python3
"""Five deliberate breakages the test suite must catch. Each is applied, the named
package tests run (integration tag, serial), the result is reported, and the file is
restored with git. Needs a configured worktree .env. Usage: scripts/mutation-check.py"""
import subprocess, sys, pathlib

ROOT = pathlib.Path(__file__).resolve().parent.parent
MUTATIONS = [
    ("ingestion drops the jitter value", "internal/center/agentapi/postgres.go", "r.Ping, r.Jitter,", "r.Ping, nil,", "./internal/center/agentapi/... ./internal/center/server/..."),
    ("alert streak off by one", "internal/center/alerts/model.go", "opened := !active && count >= threshold", "opened := !active && count > threshold", "./internal/center/alerts/..."),
    ("stats average replaced by maximum", "internal/center/stats/stats.go", '"avg(" + column + ") FILTER', '"max(" + column + ") FILTER', "./internal/center/stats/..."),
    ("retention keeps one day too many", "internal/center/jobs/retention.go", "AddDate(0, 0, -days)", "AddDate(0, 0, -days-1)", "./internal/center/jobs/..."),
    ("owner same-origin check removed", "internal/center/server/auth.go", "if !s.sameOrigin(r) {\n\t\t\twriteForbidden(w, r)\n\t\t\treturn\n\t\t}\n\t\t_, session, ok := s.validSessionAndCSRF(w, r)", "if false && !s.sameOrigin(r) {\n\t\t\twriteForbidden(w, r)\n\t\t\treturn\n\t\t}\n\t\t_, session, ok := s.validSessionAndCSRF(w, r)", "./internal/center/server/..."),
]

def run(pkg):
    return subprocess.run(["go", "test", "-count=1", "-p", "1", "-tags", "integration", *pkg.split()], cwd=ROOT, capture_output=True, text=True).returncode

def main():
    caught = 0
    for name, rel, old, new, pkg in MUTATIONS:
        path = ROOT / rel
        original = path.read_text()
        if old not in original:
            print(f"SKIP  {name}: pattern not found in {rel} (update this script)"); continue
        try:
            path.write_text(original.replace(old, new, 1))
            built = subprocess.run(["go", "build", "./..."], cwd=ROOT, capture_output=True).returncode == 0
            if not built:
                print(f"SKIP  {name}: mutant does not compile (adjust the mutation)"); continue
            status = run(pkg)
        finally:
            path.write_text(original)
        if status != 0:
            caught += 1; print(f"CAUGHT   {name}")
        else:
            print(f"SURVIVED {name}  <-- the suite does not notice this breakage")
    subprocess.run(["git", "diff", "--quiet", "--", *[m[1] for m in MUTATIONS]], cwd=ROOT, check=True)
    print(f"{caught}/{len(MUTATIONS)} mutations caught")
    return 0 if caught == len(MUTATIONS) else 1

sys.exit(main())
