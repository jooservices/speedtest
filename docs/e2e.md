# Browser end-to-end tests

`make e2e` (`node tests/e2e/main.mjs`) runs real browser scenarios against a **real command center**, **real agents** and a seeded
PostgreSQL database. Nothing in the data path is faked except the speed test itself (`internal/agent/testdata/fake-speedtest`).

## What it needs

- A worktree `.env` pointing at a disposable `speedtest_test` or `speedtest_wt<id>` database that carries the Workspaces ownership
  marker. The harness refuses any other database name and refuses a database without the marker, then drops and recreates its schema.
- `psql`, Go and Node 22 with `npm ci` done.
- A Chromium: set `PLAYWRIGHT_CHROMIUM_EXECUTABLE` to a local binary, or `PW_TEST_CONNECT_WS_ENDPOINT` to a remote browser server.
  The browser never needs to reach anything but the center started on a loopback port.

For a remote browser that must reach the center, set `E2E_HTTP_PORT` to an available port (1024–65535) and create an SSH reverse
forward for that port from the browser host to local `127.0.0.1:<port>`. The center then binds that port on local loopback and the
browser can keep using the harness origin `http://127.0.0.1:<port>`. This variable is only a test harness override; without it the
harness continues to select a random loopback port. For `make ui-coverage`, also set `UI_TEST_PORT` to that forwarded port for its
standalone smoke and wizard browser tests; the unit coverage runner overrides it to port 0 so parallel fixture servers stay isolated.

## What a run does

1. builds the center, the agent and the fake speed test into `.tmp/e2e/bin`;
2. resets the database, migrates, seeds the default owner and `tests/e2e/seed.sql` (five nodes, a week of results, IP changes, alert rules and events);
3. starts the center with `-ui-dir ui`, so the UI under test is the one in the working tree;
4. runs the scenarios of `tests/e2e/scenarios/index.mjs` in order (the shared center is the state they build on), then checks every JSON
   response the browser received against `docs/openapi.yaml` and writes browser JavaScript coverage.

## How the tests are organised: steps, suites, scenarios

| Layer | Where | What it is | Example |
|---|---|---|---|
| **Step** | `tests/e2e/steps/core.mjs` | one small user action or check | `press('Save changes')`, `fill('Node name', 'VM3')`, `toast('Saved')`, `tab('Access')`, `db(sql)` |
| **Suite** | `tests/e2e/suites/<area>.mjs` | a reusable group of steps that carries out one piece of a feature | `nodes.detailAndSettings`, `alerts.ruleLifecycle`, `settings.preferences` |
| **Scenario** | `tests/e2e/scenarios/index.mjs` | a numbered test case: an ID, the area, a title, what it proves, and the suite it runs | `SC-NODE-03 node settings validate, save and refuse a duplicate name` |

A scenario is what appears in a report and what a requirement points at; a suite is where the clicking lives; a step is the only
place that knows how a button or field is found. When the UI changes, fix a step; when a flow changes, fix a suite; when a business rule
changes, add or edit a scenario. IDs are `SC-<AREA>-<number>` and never reused. The full list is generated into
[e2e-scenarios.md](e2e-scenarios.md) (`node scripts/e2e-scenarios.mjs --write`; the script also fails on duplicate IDs or a scenario without
`covers`).

Rules for new scenarios: select by role, label or text, never by position or CSS where a role exists; no fixed sleeps (use `settled()`,
a `waitFor`, or `eventually()` for a value stored a moment after a click); check what the center stored with `db()` or `api()`, not only
what the page shows; put the change back (or order the scenario last) when it alters shared state.

## Scenarios

44 scenarios across auth, dashboard, nodes, results, statistics, IP, ISPs, schedules, alerts, settings and quality (layout, accessibility,
theme): see [e2e-scenarios.md](e2e-scenarios.md).

The SSH install wizard (host-key confirmation, live progress, failure) is covered against the contract fixture by
`npm run test:wizard`; the SSH, sudo, scripts and PostgreSQL side is covered by the installer integration tests
(see [ssh-installer.md](ssh-installer.md)).

Set `E2E_ONLY=SC-NODE-03,stats` (scenario IDs, areas or words of a title, comma separated) to run some scenarios (sign-in always runs first) and `E2E_TRACE=1` to print every write the browser makes.
Failure screenshots land in `.tmp/e2e/shots`.

The test process, center and agents run natively in this worktree. When using the Workspaces remote browser, tunnel the fixed
local port to vm-tools and provide the remote Playwright server through `PW_TEST_CONNECT_WS_ENDPOINT`; the vm-tools browser
reaches the center through the reverse tunnel at `E2E_BROWSER_ORIGIN`.

## Coverage of the UI code

`make ui-coverage` runs the E2E suite, the demo/API smoke run, the wizard test and the Node unit tests, each recording V8 line coverage of
`ui/assets/js` (the page is reloaded often, so coverage is taken before every navigation and united), and prints one line per file with
`node scripts/ui-coverage.mjs`. The gate is **85% of lines per file**; `--detail <file>` lists the lines nothing ran. Go packages are
measured with `go test -coverprofile` (see docs/development.md).

## Independent engines (JOOST-71)

`SC-ENG-01` enables LibreSpeed on an installed node, checks the new 60-minute
schedule default, changes its interval independently, verifies persistence after
reload, then disables it. `SC-ENG-02` selects LibreSpeed and returns to the primary
engine on dashboard, results and statistics; responses are checked against OpenAPI.
The native harness is approved for this ticket: `node tests/e2e/main.mjs`, without
Docker or the shared browser launcher. Run database suites and browser harnesses
serially because both reset the same verified worktree database.
