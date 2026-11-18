# Owner-local period summaries

`internal/center/report` builds daily and weekly summaries from an owner's local
calendar date and IANA time zone. `report.NewPeriod` validates the period kind,
the `YYYY-MM-DD` date and the time zone. A weekly period must start on Monday;
its local date is the Monday that starts the week.

Each period has inclusive `from` and exclusive `to` bounds in UTC. The bounds
come from the first instant belonging to each local calendar date (midnight
when it exists). A skipped start date is invalid; a skipped end date resolves
to the first instant of the next existing date. A day can span 23 or 25 hours
and a week can span 167 or 169 hours when daylight saving time changes. Queries
use the half-open interval `[from, to)`, which includes a result exactly at
`from` and excludes one exactly at `to`.

`Store.Summarize` returns the period kind, local start date, time zone, UTC
bounds and a `nodes` array. It includes one entry for each node with results in
the period, using only that node's `primary_engine`; results from other engines
are excluded. Each node entry reports its ID, name, primary engine, success and
failure counts, and average download and upload speeds in Mbps. `ok` and
`degraded` results count as successes; `failed` results count as failures. The
speed averages use successful results only and are `null` when the node has no
successful results.

When no node has results in the period, `nodes` is an empty JSON array, not
`null`. Nodes are ordered by case-insensitive name and then ID. The summary is
per-node; it never combines measurements from different nodes.
