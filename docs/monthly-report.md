# Monthly ISP and plan report (JOOST-79)

For one UTC month, one row per ISP and node:

| Column | Meaning |
|---|---|
| Plan, advertised down/up | The plan assigned to the node (ISP management, JOOST-74); blank when none |
| Tests, failures | Tests of the month in the node's engine, and how many failed |
| Avg / p95 down, avg / p95 up | Over tests that did not fail |
| Delivered % | Average download as a percentage of the advertised download; blank without a plan |
| Worst hours (UTC) | The three hours of the day with the lowest average download |
| Downtime minutes | From each failed test to the next test of the node (or the end of the month), at most 24 hours per failure |
| IP changes | Address changes recorded for the node in the month |

Engines are never blended: without `engine` each node uses its primary engine (JOOST-71).

## Where
- UI: ISPs page, "Monthly report": month picker, preview table, Download CSV and Download PDF.
- API: `GET /api/reports/monthly?month=2026-10&format=json|csv|pdf&isp=<id>&engine=ookla` (signed-in owner; default month is the previous one; a future month is refused with 422).
- CSV is RFC 4180; a text cell that starts with `=`, `+`, `-` or `@` gets a leading `'` so a spreadsheet cannot run it.
- The PDF is generated in the centre without a library: A4 landscape, ASCII only (other characters show as `?`).

## Scheduled e-mail
Set `SPEEDTEST_MONTHLY_REPORT_EMAIL=true` (default off) and configure the e-mail channel in Settings. On the first three days of each month (from 01:00 UTC on day 1) the centre sends a plain-text summary of the previous month with a sign-in link to the PDF. The month is claimed in `monthly_report_runs`, so a restart or a second centre never sends it twice; a failed send releases the claim and is retried hourly until the window closes. Sending does not use the alert queue, so quiet hours do not delay it.
