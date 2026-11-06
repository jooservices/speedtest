#!/usr/bin/env python3
"""Create the Speedtest dashboard, alert destinations and alert rules in OpenObserve (idempotent).

  provision_openobserve.py                dry run: print what would be created
  provision_openobserve.py --apply        create or update everything

Settings (environment, or --env-file with KEY=VALUE lines such as deploy/observability/vector.env):
  OPENOBSERVE_URL        default http://127.0.0.1:15080
  OPENOBSERVE_ORG        default "default"
  OPENOBSERVE_USER / OPENOBSERVE_PASSWORD   required with --apply (never printed)
  SPEEDTEST_ALERT_WEBHOOK_URL               optional generic webhook destination (Slack-compatible JSON)
  TELEGRAM_BOT_TOKEN / TELEGRAM_CHAT_ID     optional; when both are set a Telegram destination is created and
                                            every alert uses it, otherwise alerts are created without a destination
                                            (visible in OpenObserve, not delivered) and the script says so.
"""
from __future__ import annotations

import argparse
import base64
import json
import os
import sys
import urllib.error
import urllib.request
from pathlib import Path

DASHBOARD_TITLE = "Speedtest overview"
FOLDER = "default"


def read_env_file(path: str | None) -> None:
    if not path:
        return
    p = Path(path)
    if p.is_symlink() or not p.is_file() or p.stat().st_mode & 0o077:
        sys.exit(f"Refusing {path}: it must be a regular file readable only by its owner.")
    for line in p.read_text().splitlines():
        line = line.strip()
        if line and not line.startswith("#") and "=" in line:
            key, value = line.split("=", 1)
            os.environ.setdefault(key.strip(), value.strip().strip('"').strip("'"))


class Client:
    def __init__(self, url: str, org: str, user: str, password: str):
        self.url, self.org = url.rstrip("/"), org
        self.auth = "Basic " + base64.b64encode(f"{user}:{password}".encode()).decode()

    def call(self, method: str, path: str, body=None):
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(self.url + path, data=data, method=method,
                                     headers={"Authorization": self.auth, "Accept": "application/json", "Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=20) as res:
                raw = res.read()
        except urllib.error.HTTPError as error:
            detail = error.read().decode(errors="replace")[:300]
            raise SystemExit(f"OpenObserve {method} {path} failed: HTTP {error.code} {detail}") from None
        except OSError as error:
            raise SystemExit(f"OpenObserve is not reachable at {self.url}: {error}") from None
        return json.loads(raw) if raw else None


def ensure(c: "Client", kind: str, name: str, body: dict) -> None:
    """Create a template or destination, or update it when it already exists."""
    base = f"/api/{c.org}/alerts/{kind}"
    existing = c.call("GET", base) or []
    names = {item.get("name") for item in (existing if isinstance(existing, list) else existing.get("list", []))}
    c.call("PUT" if name in names else "POST", f"{base}/{name}" if name in names else base, body)


# ---------------------------------------------------------------- dashboard

def panel(n: int, title: str, kind: str, queries: list[tuple[str, str]], x: int, y: int, unit: str | None = None, description: str = "", columns: tuple[str, list[str]] | None = None) -> dict:
    """queries: (promql or sql, stream). SQL starts with SELECT."""
    built = []
    for text, stream in queries:
        is_sql = text.lstrip().upper().startswith("SELECT")
        # A custom SQL panel names the column for the horizontal axis and the columns to plot.
        axis = [{"label": columns[0], "alias": columns[0], "column": columns[0], "color": None, "isDerived": False, "havingConditions": [], "treatAsNonTimestamp": False, "aggregationFunction": None}] if columns and is_sql else []
        values = [{"label": c, "alias": c, "column": c, "color": None, "isDerived": False, "havingConditions": [], "aggregationFunction": None} for c in (columns[1] if columns and is_sql else [])]
        built.append({
            "query": text, "customQuery": True,
            "fields": {"stream": stream, "stream_type": "logs" if is_sql else "metrics", "x": axis, "y": values, "z": [], "filter": {"filterType": "group", "logicalOperator": "AND", "conditions": []}},
            "config": {"promql_legend": "", "layer_type": "scatter", "weight_fixed": 1, "limit": 0, "min": 0, "max": 100},
        })
    return {
        "id": f"Panel_ID{n}", "type": kind, "title": title, "description": description,
        "config": {"show_legends": True, "legends_position": "bottom", "unit": unit, "decimals": 2, "axis_border_show": False, "no_value_replacement": ""},
        "queryType": "sql" if built and built[0]["fields"]["stream_type"] == "logs" else "promql",
        "queries": built, "layout": {"x": x * 2, "y": y, "w": 24, "h": 7, "i": n}, "htmlContent": "", "markdownContent": "",
    }


def dashboard() -> dict:
    panels = [
        panel(1, "Requests per second by status class", "line", [("sum by (status_class) (rate(http_requests_total[5m]))", "http_requests_total")], 0, 0, "short",
              "Traffic and errors seen by the center."),
        panel(2, "Request latency p95 (seconds)", "line", [("histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))", "http_request_duration_seconds_bucket")], 12, 0, "seconds"),
        panel(3, "Nodes offline", "line", [("speedtest_nodes_offline", "speedtest_nodes_offline")], 0, 7, "short", "Enabled nodes without a heartbeat for 5 minutes."),
        panel(4, "Age of the newest result (seconds)", "line", [("speedtest_last_result_age_seconds", "speedtest_last_result_age_seconds")], 12, 7, "seconds", "Rises when no node reports."),
        panel(5, "Notification backlog", "line", [
            ("speedtest_notification_deliveries_pending", "speedtest_notification_deliveries_pending"),
            ("speedtest_notification_deliveries_failed", "speedtest_notification_deliveries_failed"),
            ("speedtest_notification_outbox_pending", "speedtest_notification_outbox_pending")], 0, 14, "short"),
        panel(6, "Oldest pending notification (seconds)", "line", [("speedtest_notification_oldest_pending_age_seconds", "speedtest_notification_oldest_pending_age_seconds")], 12, 14, "seconds"),
        panel(7, "Database pool", "line", [("speedtest_db_pool_acquired_conns", "speedtest_db_pool_acquired_conns"), ("speedtest_db_pool_max_conns", "speedtest_db_pool_max_conns")], 0, 21, "short"),
        panel(8, "Seconds since each background job last succeeded", "line", [("time() - speedtest_job_last_success_timestamp_seconds", "speedtest_job_last_success_timestamp_seconds")], 12, 21, "seconds"),
        panel(9, "Errors in the logs", "bar", [("SELECT histogram(_timestamp, '5 minutes') AS ts, count(*) AS errors FROM \"speedtest_logs\" WHERE level = 'ERROR' GROUP BY ts ORDER BY ts", "speedtest_logs")], 0, 28, "short", columns=("ts", ["errors"])),
        panel(10, "Uptime probe: /readyz status", "line", [("SELECT histogram(_timestamp, '5 minutes') AS ts, max(readyz) AS readyz FROM \"speedtest_logs\" WHERE component = 'probe' GROUP BY ts ORDER BY ts", "speedtest_logs")], 12, 28, "short",
              "200 is healthy, 0 is no answer.", columns=("ts", ["readyz"])),
        panel(11, "Disk free (%) and certificate days left", "line", [("SELECT histogram(_timestamp, '5 minutes') AS ts, min(disk_free_pct) AS disk_free_pct, min(cert_days) AS cert_days FROM \"speedtest_logs\" WHERE component = 'probe' GROUP BY ts ORDER BY ts", "speedtest_logs")], 0, 35, "short", columns=("ts", ["disk_free_pct", "cert_days"])),
        panel(12, "Results accepted, duplicate and rejected per minute", "line", [("sum by (status) (rate(speedtest_results_ingested_total[5m])) * 60", "speedtest_results_ingested_total")], 12, 35, "short"),
    ]
    return {"version": 5, "title": DASHBOARD_TITLE, "description": "Health of the Speedtest center: traffic, errors, latency, stale nodes, notifications, database pool and jobs.",
            "role": "", "owner": "", "tabs": [{"tabId": "default", "name": "Overview", "panels": panels}],
            "variables": {"list": [], "showDynamicFilters": False}, "defaultDatetimeDuration": {"type": "relative", "relativeTimePeriod": "6h", "startTime": None, "endTime": None}}


# ---------------------------------------------------------------- alerts

LOG_STREAM = "speedtest_logs"


def promql_alert(name: str, metric: str, query: str, operator: str, threshold: float, minutes: int, description: str, silence: int = 30) -> dict:
    return {"name": name, "stream": metric, "stream_type": "metrics", "kind": "promql", "promql": query, "operator": operator, "threshold": threshold,
            "period": minutes, "description": description, "silence": silence}


def sql_alert(name: str, sql: str, threshold: int, minutes: int, description: str, silence: int = 30) -> dict:
    return {"name": name, "stream": LOG_STREAM, "stream_type": "logs", "kind": "sql", "sql": sql, "operator": ">=", "threshold": threshold,
            "period": minutes, "description": description, "silence": silence}


ALERTS = [
    sql_alert("speedtest-center-down", f"SELECT count(*) AS n FROM \"{LOG_STREAM}\" WHERE component = 'probe' AND readyz = 0", 2, 3, "The uptime probe gets no answer from the center twice in 3 minutes: the center is down or unreachable. Runbook: docs/operations.md#center-down", 10),
    sql_alert("speedtest-readyz-failing", f"SELECT count(*) AS n FROM \"{LOG_STREAM}\" WHERE component = 'probe' AND readyz > 0 AND readyz <> 200", 2, 5, "The center answers but /readyz is not 200 (usually the database): twice in 5 minutes. Runbook: docs/operations.md#readyz-failing"),
    promql_alert("speedtest-5xx-rate", "http_requests_total", "sum(rate(http_requests_total{status_class=\"5xx\"}[5m]))", ">", 0.05, 5, "The center answers server errors. Runbook: docs/operations.md#server-errors"),
    promql_alert("speedtest-latency-p95", "http_request_duration_seconds_bucket", "histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))", ">", 1.0, 10, "p95 request latency above one second. Runbook: docs/operations.md#slow-requests"),
    promql_alert("speedtest-no-results", "speedtest_last_result_age_seconds", "speedtest_last_result_age_seconds", ">", 7200, 5, "No result arrived for two hours (twice the default interval). Runbook: docs/operations.md#no-results"),
    promql_alert("speedtest-nodes-offline", "speedtest_nodes_offline", "speedtest_nodes_offline", ">=", 1, 10, "One or more nodes stopped reporting. Runbook: docs/operations.md#nodes-offline"),
    promql_alert("speedtest-notifications-failing", "speedtest_notification_deliveries_failed", "increase(speedtest_notification_deliveries_failed[15m])", ">", 0, 5, "Notification deliveries failed. Runbook: docs/operations.md#notifications-stuck"),
    promql_alert("speedtest-notifications-stuck", "speedtest_notification_oldest_pending_age_seconds", "speedtest_notification_oldest_pending_age_seconds", ">", 600, 5, "A notification has waited more than ten minutes. Runbook: docs/operations.md#notifications-stuck"),
    promql_alert("speedtest-db-pool-exhausted", "speedtest_db_pool_acquired_conns", "speedtest_db_pool_acquired_conns / speedtest_db_pool_max_conns", ">=", 0.9, 5, "The database pool is almost fully used. Runbook: docs/operations.md#database-pool"),
    promql_alert("speedtest-job-stale", "speedtest_job_last_success_timestamp_seconds", "time() - speedtest_job_last_success_timestamp_seconds{job!=\"retention\"}", ">", 900, 5, "A background loop has not succeeded for 15 minutes. Runbook: docs/operations.md#background-jobs"),
    sql_alert("speedtest-disk-low", f"SELECT count(*) AS n FROM \"{LOG_STREAM}\" WHERE component = 'probe' AND disk_free_pct >= 0 AND disk_free_pct < 15", 1, 10, "Less than 15% disk space free. Runbook: docs/operations.md#disk-space"),
    sql_alert("speedtest-certificate-expiring", f"SELECT count(*) AS n FROM \"{LOG_STREAM}\" WHERE component = 'probe' AND cert_days >= 0 AND cert_days < 14", 1, 60, "The TLS certificate expires in less than 14 days. Runbook: docs/operations.md#certificate-expiry", 720),
    sql_alert("speedtest-error-burst", f"SELECT count(*) AS n FROM \"{LOG_STREAM}\" WHERE level = 'ERROR'", 20, 5, "Twenty or more error log lines in five minutes. Runbook: docs/operations.md#server-errors"),
]


def alert_body(a: dict, destinations: list[str]) -> dict:
    cond = {"type": a["kind"], "conditions": None, "search_event_type": "ui", "multi_time_range": []}
    if a["kind"] == "sql":
        cond["sql"] = a["sql"]
    else:
        cond["promql"] = a["promql"]
        cond["promql_condition"] = {"column": "value", "operator": a["operator"], "value": a["threshold"]}
    return {"name": a["name"], "stream_type": a["stream_type"], "stream_name": a["stream"], "is_real_time": False, "enabled": True,
            "description": a["description"], "destinations": destinations, "context_attributes": {}, "row_template": "", "query_condition": cond,
            "trigger_condition": {"period": a["period"], "operator": a["operator"], "threshold": int(a["threshold"]) if a["kind"] == "sql" else 1,
                                  "frequency": 1, "cron": "", "frequency_type": "minutes", "silence": a["silence"]}}


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--apply", action="store_true")
    ap.add_argument("--env-file")
    args = ap.parse_args()
    read_env_file(args.env_file)
    url = os.environ.get("OPENOBSERVE_URL", "http://127.0.0.1:15080")
    org = os.environ.get("OPENOBSERVE_ORG", "default")
    webhook = os.environ.get("SPEEDTEST_ALERT_WEBHOOK_URL", "")
    bot, chat = os.environ.get("TELEGRAM_BOT_TOKEN", ""), os.environ.get("TELEGRAM_CHAT_ID", "")
    if not args.apply:
        print(f"dry run against {url} org {org}")
        print(f"dashboard: {DASHBOARD_TITLE} ({len(dashboard()['tabs'][0]['panels'])} panels)")
        print(f"destinations: webhook={'yes' if webhook else 'no'} telegram={'yes' if bot and chat else 'no (token not supplied)'}")
        for a in ALERTS:
            print(f"alert: {a['name']} [{a['kind']}] period {a['period']} min")
        return 0
    user, password = os.environ.get("OPENOBSERVE_USER", ""), os.environ.get("OPENOBSERVE_PASSWORD", "")
    if not user or not password:
        sys.exit("OPENOBSERVE_USER and OPENOBSERVE_PASSWORD are required with --apply (they are never printed).")
    c = Client(url, org, user, password)

    # Dashboard: replace the one with our title.
    existing = c.call("GET", f"/api/{org}/dashboards") or {}
    for d in existing.get("dashboards", []):
        did = d.get("dashboard_id") or d.get("dashboardId") or (d.get("v5") or {}).get("dashboardId")
        title = d.get("title") or (d.get("v5") or {}).get("title")
        if title == DASHBOARD_TITLE and did:
            c.call("DELETE", f"/api/{org}/dashboards/{did}?folder={FOLDER}")
    c.call("POST", f"/api/{org}/dashboards?folder={FOLDER}", dashboard())
    print("dashboard created")

    # Destinations. Telegram or a webhook when supplied; otherwise a placeholder that cannot deliver.
    destinations: list[str] = []

    def destination(name: str, url: str, text: str) -> None:
        ensure(c, "templates", name, {"name": name, "body": text, "type": "http", "title": ""})
        ensure(c, "destinations", name, {"name": name, "url": url, "method": "post", "skip_tls_verify": False, "template": name, "headers": {}, "type": "http"})
        destinations.append(name)

    if webhook:
        destination("speedtest-webhook", webhook, json.dumps({"text": "Speedtest alert {alert_name}: {alert_description}"}))
    if bot and chat:
        destination("speedtest-telegram", f"https://api.telegram.org/bot{bot}/sendMessage",
                    json.dumps({"chat_id": chat, "text": "Speedtest alert {alert_name}\n{alert_description}"}))
    if not destinations:
        # OpenObserve refuses an alert without a destination. Until a real one is supplied the alerts point at a
        # destination that cannot deliver: they evaluate and show their state in OpenObserve, nothing reaches a person.
        # OpenObserve's SSRF guard rejects private and unresolvable hosts, so the placeholder is a public address that
        # ignores the call; the body carries no alert text.
        destination("speedtest-unrouted", "https://example.com/", json.dumps({"alert": "unrouted"}))
        print("WARNING: no real destination configured (set TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID, or SPEEDTEST_ALERT_WEBHOOK_URL, and run again): alerts are evaluated but delivered nowhere")

    # Alerts.
    current = {a["name"]: a for a in (c.call("GET", f"/api/v2/{org}/alerts") or {}).get("list", [])}
    streams = {t: {x["name"] for x in (c.call("GET", f"/api/{org}/streams?type={t}") or {}).get("list", [])} for t in ("logs", "metrics")}
    pending = []
    for a in ALERTS:
        if a["stream"] not in streams[a["stream_type"]]:
            # OpenObserve only accepts an alert on a stream that exists; it appears with the first data point.
            pending.append(a["name"])
            continue
        body = alert_body(a, destinations)
        if a["name"] in current:
            c.call("PUT", f"/api/v2/{org}/alerts/{current[a['name']]['alert_id']}", body)
        else:
            c.call("POST", f"/api/v2/{org}/alerts", body)
        print("alert", a["name"])
    if pending:
        print("waiting for data before these alerts can be created (run this script again later):", ", ".join(pending))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
