# Browser end-to-end tests

`make e2e` (`node tests/e2e/main.mjs`) runs real browser scenarios against a **real command center**, **real agents** and a seeded
PostgreSQL database. Nothing in the data path is faked except the speed test itself (`internal/agent/testdata/fake-speedtest`).

## What it needs

- A worktree `.env` pointing at a disposable `speedtest_test` or `speedtest_wt<id>` database that carries the Workspaces ownership
  marker. The harness refuses any other database name and refuses a database without the marker, then drops and recreates its schema.
- `psql`, Go and Node 22 with `npm ci` done.
- A Chromium: set `PLAYWRIGHT_CHROMIUM_EXECUTABLE` to a local binary, or `PW_TEST_CONNECT_WS_ENDPOINT` to a remote browser server.
  The browser never needs to reach anything but the center started on a random loopback port.

## What a run does

1. builds the center, the agent and the fake speed test into `.tmp/e2e/bin`;
2. resets the database, migrates, seeds the default owner and `tests/e2e/seed.sql` (five nodes, a week of results, IP changes, alert rules and events);
3. starts the center with `-ui-dir ui`, so the UI under test is the one in the working tree;
4. runs the scenarios in `tests/e2e/main.mjs`, built from the reusable steps in `tests/e2e/steps.mjs`.

## Scenarios

| Scenario | Covers |
| --- | --- |
| signing in needs the right password | sign-in page, plain-language errors, hidden menu while signed out |
| the dashboard shows the seeded fleet | fleet tiles, node cards, IP change count |
| every page draws real data without console errors | all 8 pages, node detail tabs |
| every page holds its layout | 390 / 768 / 1440 px, dark and light, no overflow |
| results can be filtered, opened and exported | status filter, search, minimum download, drawer, CSV download |
| a node is added with the install command and reports | wizard, one-time key, real agent enrolment, check-in, **Run test** with live progress, stored manual result, key works once |
| schedules are saved on the center | interval, stagger, quiet hours persisted in PostgreSQL |
| alert rules ... | create, validate, edit, switch off, delete, acknowledge, acknowledge all |
| settings are saved ... | preference save, write-only secrets stored encrypted, enrolment key shown once |
| a test notification reaches the configured channel | webhook delivery to a local receiver, unconfigured channel refused |
| signing out ends the session | sign-out, protected route redirect, API 401 |

The SSH install wizard (host-key confirmation, live progress, failure) is covered against the contract fixture by
`npm run test:wizard`; the SSH, sudo, scripts and PostgreSQL side is covered by the installer integration tests
(see [ssh-installer.md](ssh-installer.md)).

Set `E2E_ONLY=<part of a scenario name>` to run one scenario (sign-in always runs first) and `E2E_TRACE=1` to print every write the browser makes.
Failure screenshots land in `.tmp/e2e/shots`.
