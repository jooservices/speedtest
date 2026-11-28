# Browser end-to-end tests

Run the Workspace gates from an isolated worktree with `/home/joos/Workspaces/scripts/dev/check run e2e --all` or
`/home/joos/Workspaces/scripts/dev/check run ui-coverage --all`. The project profile declares the application port for both stages;
the Workspace runner uses the machine's browser setting and `AIFLOW_E2E_HOST` to select the browser host and set up the reverse tunnel.
On dev-workspaces this puts Playwright on vm-tools. Check the worktree database guard before either gate.
Direct `make e2e` and `make ui-coverage` invoke the harness without that routing layer and can launch Chromium locally.

`make e2e` (`node tests/e2e/main.mjs`) runs real browser scenarios against a **real command center**, **real agents** and a seeded
PostgreSQL database. Nothing in the data path is faked except the speed test itself (`internal/agent/testdata/fake-speedtest`).

## What it needs

- A worktree `.env` pointing at a disposable database through `SPEEDTEST_DB_*`, with `APP_ENV` set to `local` or `test`.
  The harness refuses any other `APP_ENV`, then drops and recreates the schema of that database.
- `psql`, Go and Node 22 with `npm ci` done.
- A Chromium: set `PLAYWRIGHT_CHROMIUM_EXECUTABLE` to a local binary, or `PW_TEST_CONNECT_WS_ENDPOINT` to a remote browser server.
  The browser never needs to reach anything but the center started on a loopback port.

For a remote browser that must reach the center, the Workspace gate selects `E2E_HTTP_PORT` (1024–65535) and creates an SSH reverse
forward for that port from the browser host to local `127.0.0.1:<port>`. The center then binds that port on local loopback and the
browser uses the harness origin `http://127.0.0.1:<port>`. Without the Workspace gate, the harness selects a random loopback port.
The UI coverage target reuses the forwarded port for its sequential smoke, wizard and browser unit tests; local runs use random fixture ports.

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

Scenarios cover auth, dashboard, nodes, results, statistics, IP, schedules, alerts, settings and quality, including axe WCAG checks,
keyboard focus, layout, themes and UI improvements: see [e2e-scenarios.md](e2e-scenarios.md).

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
