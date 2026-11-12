#!/usr/bin/env python3
"""Run unit tests and enforce the per-package timing budget."""

from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[1]
DEFAULT_BUDGET = ROOT / "docs" / "test-budget.json"


def load_budget(path: Path) -> tuple[float, dict[str, float]]:
    data = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(data, dict):
        raise ValueError("budget must be a JSON object")

    default = data.get("default_seconds")
    if isinstance(default, bool) or not isinstance(default, (int, float)) or default <= 0:
        raise ValueError("default_seconds must be a positive number")

    overrides = data.get("overrides", {})
    if not isinstance(overrides, dict):
        raise ValueError("overrides must be a JSON object")
    for package, seconds in overrides.items():
        if not isinstance(package, str) or not package:
            raise ValueError("override package names must be non-empty strings")
        if isinstance(seconds, bool) or not isinstance(seconds, (int, float)) or seconds <= 0:
            raise ValueError(f"override for {package} must be a positive number")
    return float(default), {key: float(value) for key, value in overrides.items()}


def parse_test_output(output: str) -> tuple[dict[str, float], list[str]]:
    timings: dict[str, float] = {}
    failures: set[str] = set()
    for line_number, line in enumerate(output.splitlines(), start=1):
        try:
            event: dict[str, Any] = json.loads(line)
        except json.JSONDecodeError as error:
            raise ValueError(f"go test emitted invalid JSON on line {line_number}") from error

        action = event.get("Action")
        package = event.get("Package")
        if action not in {"pass", "fail"} or not package or event.get("Test") is not None:
            continue
        elapsed = event.get("Elapsed")
        if isinstance(elapsed, bool) or not isinstance(elapsed, (int, float)):
            raise ValueError(f"go test omitted package elapsed time for {package}")
        timings[package] = float(elapsed)
        if action == "fail":
            failures.add(package)
    return timings, sorted(failures)


def find_over_budget(
    timings: dict[str, float], default: float, overrides: dict[str, float]
) -> list[tuple[str, float, float]]:
    return [
        (package, elapsed, overrides.get(package, default))
        for package, elapsed in sorted(timings.items())
        if elapsed > overrides.get(package, default)
    ]


def main() -> int:
    try:
        default, overrides = load_budget(DEFAULT_BUDGET)
    except (OSError, json.JSONDecodeError, ValueError) as error:
        print(f"FAIL: could not load test budget: {error}", file=sys.stderr)
        return 1

    result = subprocess.run(
        ["go", "test", "-json", "./..."],
        cwd=ROOT,
        capture_output=True,
        text=True,
        check=False,
    )
    try:
        timings, failures = parse_test_output(result.stdout)
    except ValueError as error:
        print(f"FAIL: could not read Go test timings: {error}", file=sys.stderr)
        return 1

    if result.returncode:
        packages = ", ".join(failures) or "unknown package"
        print(f"FAIL: go test -json ./... exited {result.returncode}: {packages}", file=sys.stderr)
        return result.returncode
    if not timings:
        print("FAIL: go test -json ./... returned no package timings", file=sys.stderr)
        return 1

    violations = find_over_budget(timings, default, overrides)
    if violations:
        print("FAIL: packages exceeded their test timing budgets:", file=sys.stderr)
        for package, elapsed, budget in violations:
            print(f"  {package}: {elapsed:.2f}s > {budget:.2f}s", file=sys.stderr)
        return 1

    print(f"PASS: test timing budgets for {len(timings)} packages (default {default:g}s)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
