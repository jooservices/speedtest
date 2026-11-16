import json
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
PROFILE = ROOT / ".ai" / "project.json"
WORKFLOWS = ROOT / ".github" / "workflows"
# The static and unit stages of the project profile that pull requests must run.
STATIC_AND_UNIT = {"vet", "lint", "typecheck", "ui-syntax", "shell", "secrets",
                   "unit-go", "unit-ui", "python", "scripts", "test-matrix"}
# lint runs through golangci/golangci-lint-action, which is the CI form of `golangci-lint run`.
RUN_THROUGH_ACTION = {"lint"}


def workflow_text():
    """The text of every workflow: the CI is split by purpose into several files, and a stage may run in any of them."""
    return "\n".join(path.read_text(encoding="utf-8") for path in sorted(WORKFLOWS.glob("*.yml")))


def workflow_commands(text):
    commands = set()
    for line in text.splitlines():
        stripped = line.strip()
        if stripped.startswith("run: "):
            stripped = stripped[len("run: "):]
        commands.add(stripped)
    return commands


class CiStageTests(unittest.TestCase):
    def test_every_static_and_unit_stage_command_is_a_ci_step(self):
        stages = json.loads(PROFILE.read_text(encoding="utf-8"))["stages"]
        commands = workflow_commands(workflow_text())
        missing = [f'{stage["name"]}: {stage["cmd"]}' for stage in stages
                   if stage["name"] in STATIC_AND_UNIT - RUN_THROUGH_ACTION and stage["cmd"] not in commands]
        self.assertEqual(missing, [])

    def test_lint_stage_runs_through_the_golangci_action(self):
        self.assertIn("golangci/golangci-lint-action", workflow_text())


if __name__ == "__main__":
    unittest.main()
