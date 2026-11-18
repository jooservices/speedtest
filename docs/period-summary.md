# Owner-local period summaries

`internal/center/report` builds daily and weekly summaries from an owner's local
calendar date and IANA time zone. `report.NewPeriod` validates the period kind,
the `YYYY-MM-DD` date and the time zone. A weekly period must start on Monday;
its local date is the Monday that starts the week.

Each period reports inclusive `from` and exclusive `to` UTC envelope bounds.
For ordinary calendar periods, the envelope is one half-open interval. A day
can span 23 or 25 hours and a week can span 167 or 169 hours when daylight
saving time changes. A skipped daily date is invalid; a skipped end date
resolves to the first instant belonging to the next existing date.

A rollback across local midnight can make one local date occupy disjoint UTC
intervals. For example, in `America/St_Johns` on 2010-11-07 the clock advanced
through the first minute of November 7, rolled back to November 6, then entered
November 7 again. In that case `from` and `to` describe the outer envelope, not
membership of every instant inside it. Summary queries use the union of exact
half-open UTC intervals, so the repeated segment belongs to November 6 and the
early and later segments belong to November 7. The same interval rule applies
to weekly periods. Results exactly at an interval start are included; results
exactly at an interval end are excluded.

`Store.Summarize` returns the period kind, local start date, time zone, UTC
envelope bounds and a `nodes` array. It includes one entry for each node with results in
the period, using only that node's `primary_engine`; results from other engines
are excluded. Each node entry reports its ID, name, primary engine, success and
failure counts, and average download and upload speeds in Mbps. `ok` and
`degraded` results count as successes; `failed` results count as failures. The
speed averages use successful results only and are `null` when the node has no
successful results.

When no node has results in the period, `nodes` is an empty JSON array, not
`null`. Nodes are ordered by case-insensitive name and then ID. The summary is
per-node; it never combines measurements from different nodes.
