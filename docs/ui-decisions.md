# UI decisions and open questions

## Decisions
- **Statistics analytics (JOOST-81).** Extend the existing statistics pipeline and add a bounded events endpoint; keep the rolling 30-day, Monday-first 168-cell heatmap. Show p50/p95/p99 with the selected metric's unit, compare the selected metric over consecutive seven-day periods, and add weekday/hour congestion plus IP/ISP/alert marks. The exact ISP filter uses trimmed `engine_isp`, then the cached IP organisation; unknown attribution is explained in the filter help and excluded from specific ISP matches. Event marks and their list disclose the 500-item cap. Demo mode follows the same formulas and filters as the center.
- **Plain HTML/CSS/JS, no build step, no framework.** The preview opens from a static server with `?demo=1`; without it, the store reads the same-origin command-center API. Charts are a small SVG kit in `ui/assets/js/charts.js` (line/area, bars, heatmap, sparkline, map) so the preview has no external requests.
- **Dark theme by default**, switchable to light; the choice is remembered. Chart colours come from a validated categorical palette; each node keeps its colour for its whole life.
- **Nodes are independent.** No WAN or load-balancer concept in the data model. Expected speed is the only thing a node needs to be judged against, so "% of expected" is the fair way to compare nodes of different capacity (offered on the dashboard chart).
- **Sample data** is deterministic and generated in the browser (`seed.js`): 5 nodes from the owner's real setup (Viettel Fast 50 at 300 Mbps, Viettel Pro 600 at 1000, VNPT Home 5 Super at 1000, VNPT Home 2 at 1000, and one combined node at 3300), 30 days at 30-minute intervals, outages, one degraded week, changing public IPs. Sample IPs use reserved documentation ranges only.
- **Rate-limited tests are shown separately from success rates.** The dashboard and Statistics table show the excluded count next to the rate, and node detail explains how many rate-limited tests were excluded. When every test in a period is rate-limited, the rate is empty while the count remains visible. Demo and API-backed views use the same wording.
- **Locations** come from a sample lookup keyed by IP, standing in for GeoLite2.
- **Server selection** in live node settings is per engine. Ookla lists may show the distance supplied by Ookla; LibreSpeed lists are alphabetical and have no distance column. Demo mode keeps its local preview control.
- **The map is illustrative.** The real build should use a map library with tiles.
- Interface text is English (the product is generic); the owner reviews in Vietnamese.

## Production review improvements (JOOST-82)

- The Nodes table shows download and upload averages as a percentage of each node's expected speed. These are calculated independently for that node.
- Dashboard KPI deltas compare the selected period with the equally long period immediately before it. Live mode reads both periods from the center's dashboard snapshot because the results list contains only the selected period. IP changes use lower-is-better direction, with the history link kept beside the comparison. While the comparison loads the cards say so; an unavailable comparison can be retried by refreshing. When the earlier period has no results, the card says so instead of showing an unexplained dash; a zero baseline is labelled as new activity.
- Failed result rows keep the existing full error in the result details and show a short, whitespace-normalized reason chip in result tables. The chip is capped at 32 characters.
- Trend charts with fewer than three valid measurements explain that more data is needed for the selected metric. Dashboard notices count only visible series; Statistics notices count only selected nodes, using the selected engine's measurements. Node charts mark expected download and upload speeds separately, including when there are no valid points; the dashboard's normalized chart marks 100% as expected.
- The JOOST-71 engine selector filters dashboard, results and statistics views; different engines remain separate series and are never combined into one average.
- On phones the top bar keeps the menu, time range and Run action on one row. Active-node status, theme and sign-out actions live in the menu.
- Enter activates a focused row or node card. Enter on a nested Run button opens the test dialog without also activating its parent.
- Interactive tables keep native table semantics and expose the primary row action as a descriptively named link or button in a data cell. The node list links to node details; result rows open their details, and nested Run controls remain separate.
- Dashboard comparison responses update only the five KPI values and deltas in place. Navigation invalidates pending comparisons, so a delayed response cannot rebuild a different page or move dashboard focus.
- Statistics sparse warnings count valid plotted hourly or daily buckets in each displayed trend series. Each warning also reports its raw sample count, in demo and API modes.
- The UI coverage report publishes function counts alongside line coverage and requires every function in the reviewed dashboard, Statistics and shared UI files to run.
- The E2E accessibility scenario runs axe against the WCAG 2.0, 2.1 and 2.2 A/AA tags on every page and verifies visible keyboard focus, row/card activation and nested Run behavior.

## API foundation (JOOST-37)

- `api.js` provides same-origin JSON requests, cookie sessions, in-memory CSRF, cancellation, ETag/Idempotency-Key headers and opt-in in-memory GET caching (maximum 5 seconds, 100 entries). Auth responses are never cached; writes and 401 clear the cache.
- `store.js` keeps synchronous read/query functions over an asynchronously loaded snapshot. `ST.load()` loads the snapshot; `ST.remote` exposes loading/error/ready and `ST.cancelLoad()` cancels it. Remote mutations return promises and never write demo storage.
- The app shell supplies loading/error/retry on every route. The `#/signin` route is a session-required handoff; the complete sign-in page belongs to JOOST-40. Page-specific backend analytics and workflows remain in JOOST-38/39/40/41. Real-backend browser E2E belongs to JOOST-48 (Claude decision, JOOST-37 comment 24579).
- Contract-shaped fake HTTP responses are validated against the exact OpenAPI operation schemas. Demo sample data, persistence and visual components remain available only with `?demo=1`.

## Open questions for review
1. Vietnamese language option for the UI?
2. Is a single shared login enough for v1, or are separate users and roles needed?
3. Which notification channels matter first (email, Telegram, Slack, webhook)?
4. Should a combined node's result be compared against the sum of the other nodes automatically (currently optional through "Add combined total" in Statistics)?
5. Data retention default (currently one year).
