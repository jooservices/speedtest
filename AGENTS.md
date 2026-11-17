# Speedtest: AI developer guidance

Apply the Workspaces root `AGENTS.md` and `JOOservices/AGENTS.md` first. This file adds project rules.

## What this is
Agents on nodes run scheduled speed tests and report to a command center that stores results, tracks each node's public IP and location, and serves a web UI. Read `README.md` and `docs/ui-decisions.md`. The approved UI preview is in `ui/`; keep it working in demo mode.

## Rules
- **Done means Done:** follow [`docs/definition-of-done.md`](docs/definition-of-done.md) exactly. Any open issue, including a UI or UX issue, means the ticket is not finished. Attach evidence for each item in your completion comment.
- Stack: Go 1.27 (native), PostgreSQL (shared Workspaces service), plain HTML/CSS/JS for the UI with no build step. Add a dependency only when justified in the ticket comment.
- Nodes are independent. A node knows its own configuration only; never add a WAN or load-balancer concept to the data model.
- Configuration comes from `.env` with documented defaults in `.env.example`. Never hard-code a service address and never print or commit a secret.
- Databases: use only the databases provisioned for this project; run `scripts/docker/shared check-project` before any migrate, seed or test run. Tests use the disposable database that `SPEEDTEST_DB_*` names in your worktree `.env`, never the live one; code reads only `APP_ENV` and the connection variables and knows nothing about the workspace.
- Work in a ticket worktree from the latest `develop`. Run the Sonar and Gitleaks scans before every push.
- Jira content and code comments are English. Record progress and verification in Jira comments, not in the description.
- Do not define or activate sub-agents.
