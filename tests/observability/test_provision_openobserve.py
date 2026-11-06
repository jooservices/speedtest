import importlib.util
import json
import pathlib
import unittest

SPEC = importlib.util.spec_from_file_location("provision", pathlib.Path(__file__).resolve().parents[2] / "deploy/observability/provision_openobserve.py")
provision = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(provision)


class DashboardTests(unittest.TestCase):
    def test_every_panel_has_a_unique_id_a_query_and_a_place(self):
        panels = provision.dashboard()["tabs"][0]["panels"]
        ids = [p["id"] for p in panels]
        self.assertEqual(len(ids), len(set(ids)))
        self.assertEqual(len(panels), 12)
        for p in panels:
            self.assertTrue(p["queries"][0]["query"])
            self.assertIn(p["queryType"], ("promql", "sql"))
            self.assertLessEqual(p["layout"]["x"] + p["layout"]["w"], 48, "panels stay inside the 48-column grid")

    def test_sql_panels_name_their_axis_columns(self):
        for p in provision.dashboard()["tabs"][0]["panels"]:
            if p["queryType"] == "sql":
                fields = p["queries"][0]["fields"]
                self.assertTrue(fields["x"] and fields["y"], p["title"])

    def test_dashboard_is_json(self):
        json.dumps(provision.dashboard())


class AlertTests(unittest.TestCase):
    def test_names_are_unique_and_every_alert_has_a_runbook_entry(self):
        names = [a["name"] for a in provision.ALERTS]
        self.assertEqual(len(names), len(set(names)))
        runbook = (pathlib.Path(__file__).resolve().parents[2] / "docs/operations.md").read_text()
        for a in provision.ALERTS:
            anchor = a["description"].split("docs/operations.md#")[1].split()[0].rstrip(".")
            self.assertIn(f"### {anchor}", runbook, f"{a['name']} points at a missing runbook entry")

    def test_promql_and_sql_bodies(self):
        promql = next(a for a in provision.ALERTS if a["kind"] == "promql")
        body = provision.alert_body(promql, ["speedtest-telegram"])
        self.assertEqual(body["query_condition"]["type"], "promql")
        self.assertEqual(body["query_condition"]["promql_condition"]["operator"], promql["operator"])
        self.assertEqual(body["destinations"], ["speedtest-telegram"])
        sql = next(a for a in provision.ALERTS if a["kind"] == "sql")
        body = provision.alert_body(sql, ["d"])
        self.assertEqual(body["query_condition"]["type"], "sql")
        self.assertEqual(body["stream_type"], "logs")
        self.assertEqual(body["trigger_condition"]["threshold"], int(sql["threshold"]))

    def test_the_metrics_the_alerts_read_are_documented(self):
        doc = (pathlib.Path(__file__).resolve().parents[2] / "docs/observability.md").read_text()
        for a in provision.ALERTS:
            self.assertIn(a["name"], doc)


class DryRunTests(unittest.TestCase):
    def test_dry_run_needs_no_credentials_and_writes_nothing(self):
        import io, contextlib, sys
        out = io.StringIO()
        argv, sys.argv = sys.argv, ["provision"]
        try:
            with contextlib.redirect_stdout(out):
                self.assertEqual(provision.main(), 0)
        finally:
            sys.argv = argv
        self.assertIn("dry run", out.getvalue())
        self.assertIn("alert: speedtest-center-down", out.getvalue())


if __name__ == "__main__":
    unittest.main()
