import importlib.util
import json
import unittest
from pathlib import Path
from tempfile import TemporaryDirectory


SCRIPT = Path(__file__).resolve().parents[2] / "scripts" / "test-timing.py"
SPEC = importlib.util.spec_from_file_location("test_timing", SCRIPT)
assert SPEC and SPEC.loader
test_timing = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(test_timing)


class ParseTestOutputTests(unittest.TestCase):
    def test_package_timings_ignore_individual_test_events(self):
        output = "\n".join(
            [
                json.dumps({"Action": "pass", "Package": "example/auth", "Test": "TestLogin", "Elapsed": 0.3}),
                json.dumps({"Action": "pass", "Package": "example/auth", "Elapsed": 8.74}),
                json.dumps({"Action": "fail", "Package": "example/api", "Elapsed": 1.25}),
            ]
        )

        timings, failures = test_timing.parse_test_output(output)

        self.assertEqual(timings, {"example/auth": 8.74, "example/api": 1.25})
        self.assertEqual(failures, ["example/api"])

    def test_invalid_json_is_rejected(self):
        with self.assertRaisesRegex(ValueError, "line 1"):
            test_timing.parse_test_output("not-json")


class BudgetTests(unittest.TestCase):
    def test_override_and_default_budgets_are_applied(self):
        violations = test_timing.find_over_budget(
            {"example/auth": 20.01, "example/quick": 60, "example/slow": 60.01},
            60,
            {"example/auth": 20},
        )

        self.assertEqual(
            violations,
            [("example/auth", 20.01, 20), ("example/slow", 60.01, 60)],
        )

    def test_budget_file_is_validated(self):
        with TemporaryDirectory() as directory:
            path = Path(directory) / "budget.json"
            path.write_text('{"default_seconds": 60, "overrides": {"example/auth": 20}}', encoding="utf-8")

            self.assertEqual(
                test_timing.load_budget(path),
                (60.0, {"example/auth": 20.0}),
            )

            path.write_text('{"default_seconds": 0}', encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "positive number"):
                test_timing.load_budget(path)


if __name__ == "__main__":
    unittest.main()
