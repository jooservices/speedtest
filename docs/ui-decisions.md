# UI decisions and open questions

## Decisions
- **Plain HTML/CSS/JS, no build step, no framework.** The preview opens from a static server with `?demo=1`; without it, the store reads the same-origin command-center API. Charts are a small SVG kit in `ui/assets/js/charts.js` (line/area, bars, heatmap, sparkline, map) so the preview has no external requests.
- **Dark theme by default**, switchable to light; the choice is remembered. Chart colours come from a validated categorical palette; each node keeps its colour for its whole life.
- **Nodes are independent.** No WAN or load-balancer concept in the data model. Expected speed is the only thing a node needs to be judged against, so "% of expected" is the fair way to compare nodes of different capacity (offered on the dashboard chart).
- **Sample data** is deterministic and generated in the browser (`seed.js`): 5 nodes from the owner's real setup (Viettel Fast 50 at 300 Mbps, Viettel Pro 600 at 1000, VNPT Home 5 Super at 1000, VNPT Home 2 at 1000, and one combined node at 3300), 30 days at 30-minute intervals, outages, one degraded week, changing public IPs. Sample IPs use reserved documentation ranges only.
- **Rate-limited tests are shown separately from success rates.** The dashboard and Statistics table show the excluded count next to the rate, and node detail explains how many rate-limited tests were excluded. When every test in a period is rate-limited, the rate is empty while the count remains visible. Demo and API-backed views use the same wording.
- **Locations** come from a sample lookup keyed by IP, standing in for GeoLite2.
- **The map is illustrative.** The real build should use a map library with tiles.
- Interface text is English (the product is generic); the owner reviews in Vietnamese.

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
