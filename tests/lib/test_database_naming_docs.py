import re
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
GUARD = ROOT / "internal" / "center" / "db" / "test_database.go"
# Every document that states the test database naming rule.
DOCS = ["README.md", "docs/api.md", "docs/architecture.md", "docs/geolocation.md",
        "docs/e2e.md", "docs/development.md", "migrations/README.md"]


def guard_branches():
    pattern = re.search(r"regexp\.MustCompile\(`([^`]+)`\)", GUARD.read_text(encoding="utf-8")).group(1)
    body = re.fullmatch(r"\^\((.+)\)\$", pattern).group(1)
    return pattern, body.split("|")


class DatabaseNamingDocsTests(unittest.TestCase):
    def test_guard_accepts_ticket_tree_and_refuses_live_name(self):
        pattern, _ = guard_branches()
        guard = re.compile(pattern)

        def accepted(name):
            # Mirrors CheckTestDatabase: the shared test database is a special case next to the pattern.
            return name == "speedtest_test" or guard.fullmatch(name) is not None

        for name in ["speedtest_test", "wt_joost_94"]:
            self.assertTrue(accepted(name), name)
        for name in ["speedtest", "wt_a"]:
            self.assertFalse(accepted(name), name)

    def test_migrations_readme_states_both_guard_branches(self):
        _, branches = guard_branches()
        text = (ROOT / "migrations" / "README.md").read_text(encoding="utf-8")
        for branch in branches:
            self.assertIn(f"^{branch}$", text)

    def test_every_doc_that_names_the_worktree_database_also_names_the_ticket_form(self):
        for doc in DOCS:
            text = (ROOT / doc).read_text(encoding="utf-8")
            if "speedtest_wt" in text:
                self.assertIn("wt_<ticket>", text, doc)


if __name__ == "__main__":
    unittest.main()
