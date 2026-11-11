# Monthly ISP and plan report (JOOST-79)

For one UTC month, one row per ISP and node:

| Column | Meaning |
|---|---|
| Plan, advertised down/up | The assigned plan only when its ISP matches the row; blank when none or the ISP changed |
| Tests, failures | Tests of the month in the node's engine, and how many failed |
| Avg / p95 down, avg / p95 up | Over tests that did not fail |
| Delivered % | Average download as a percentage of the advertised download; blank without a plan |
| Worst hours (UTC) | The three hours of the day with the lowest average download for this ISP and node |
| Downtime minutes | From each failed test to the next test of the node, or the report boundary for an open interval, at most 24 hours per failure |
| IP changes | Actual address transitions for this ISP and node; an initial observation is not a change, and a transition belongs to its destination ISP |

An unlinked result inherits the ISP from the latest IP-history event at the measurement time, then from an earlier ISP-linked result only when no IP-history event exists. If the latest event has no ISP, or neither source identifies an ISP, the report includes an `Unknown ISP` row with `ispId: null`, ASN `0`, and no plan. IP-only transitions also create a row, even when the node has no test in that ISP during the month. A transition with no destination ISP belongs to `Unknown ISP`, never to its previous provider.

Engines are never blended: without `engine` each node uses its primary engine (JOOST-71).

## Where
- UI: ISPs page, "Monthly report": month picker, preview table, Download CSV and Download PDF.
- API: `GET /api/reports/monthly?month=2026-10&format=json|csv|pdf&isp=<id>&engine=ookla` (signed-in owner; default month is the previous one; a future month is refused with 422).
- CSV is RFC 4180; a text cell that starts with `=`, `+`, `-` or `@` gets a leading `'` so a spreadsheet cannot run it.
- The PDF uses an embedded Noto Sans font, keeps Unicode names intact, wraps long values, and paginates the A4 landscape table.

## Scheduled e-mail
Set `SPEEDTEST_MONTHLY_REPORT_EMAIL=true` (default off) and configure the e-mail channel in Settings. On the first three days of each month (from 01:00 UTC on day 1) the centre sends a plain-text summary of the previous month with a sign-in link to the PDF. A 15-minute lease is renewed while a report is built and sent. Unsent months are kept oldest-first; an expired interrupted claim can be recovered after the send window, while ordinary failed attempts retry after five minutes only while the window remains open. A failure to persist the final sent state leaves the lease to expire, so an uncertain provider response can result in a later duplicate delivery. Sending does not use the alert queue, so quiet hours do not delay it.
