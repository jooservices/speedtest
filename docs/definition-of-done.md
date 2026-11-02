# Definition of Done

Owner decision, 2026-11-02. A ticket is **Done only when the work is proven to work in production conditions**. **Any single open issue means the ticket does not pass**, including a UI or UX issue. There is no "good enough" and no "fix later": a defect found at any stage sends the ticket back to its developer.

The reviewer (Claude) checks every item below with evidence. A developer's own statement is not evidence.

## 1. Correctness
- [ ] Every acceptance criterion in the ticket is met and demonstrated (command output, screenshot, or test name), not just claimed.
- [ ] Error paths, empty states, boundary values and concurrency behave correctly, not only the happy path.
- [ ] No unrelated change in the diff; no TODO, debug output or commented-out code left behind.

## 2. Tests (all must pass, none skipped to get green)
- [ ] Unit tests for the new logic; changed packages keep at least 80% line coverage.
- [ ] Integration tests for every boundary touched (database, HTTP API, agent protocol).
- [ ] End-to-end (browser) tests for every user flow the change touches, running through `scripts/browser/e2e`, three consecutive green runs.
- [ ] Regression test for every bug fixed.
- [ ] The whole suite is green in CI, not only the new tests.

## 3. UI and UX (any defect fails the ticket)
- [ ] Looks right in dark and light themes at 390, 768 and 1440 px wide; nothing overflows, overlaps or is cut off.
- [ ] Loading, empty, error and success states all exist and read plainly; messages say what happened and what to do.
- [ ] Keyboard-only use works with a visible focus; labels, roles and contrast meet WCAG 2.2 AA.
- [ ] Charts and tables show correct numbers, units and time zone, and their hover/click behaviour works.
- [ ] No console errors or warnings; no layout shift or flicker on load.
- [ ] Reviewed by Claude from real screenshots of the running application.

## 4. Production readiness
- [ ] Runs on staging (WSL) against the shared services with real agents, no errors or warnings in the logs.
- [ ] Migrations apply and roll back on a copy of realistic data; the rollback steps are written down.
- [ ] Configuration only through `.env` with a documented default; nothing hard-coded to Docker or a machine.
- [ ] Security: authentication and authorisation on every endpoint, input validated, secrets never logged or stored in clear, dependencies free of known vulnerabilities.
- [ ] Performance targets in the ticket are measured and met; no unbounded query, memory growth or log volume.
- [ ] Health checks, logs and alerts allow an operator to tell when it is broken.
- [ ] Documentation and runbook updated.

## 5. Quality gates before every push
- [ ] `scripts/scan/main.py sonar <path>` returns `"qualityGate": "OK"` with zero new issues.
- [ ] `scripts/scan/main.py gitleaks <path>` returns `"pass": true`.
- [ ] Formatting, vet and lint are clean.

## 6. Process
- Maximum two review loops. If the ticket is still not passing after loop 2, Claude takes ownership, fixes it, and records why loop 2 failed.
- A defect found after a ticket is Done reopens it, and a Bug linked to it blocks the next release.
- Release `1.0.0` happens only when every ticket is Done under this definition and no Bug is open. The owner decides when to deploy to staging and when to release.
