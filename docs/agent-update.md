# Agent self-update

The agent can replace its own binary when the owner allows it. The design keeps the long-running agent
unprivileged: it never writes its own executable. A short privileged step that systemd runs before each start does
the installation, so a compromised agent user cannot plant code that runs as root.

## Flow

1. The center advertises the latest release in the agent configuration (`release`: `version`, `url`, `sha256`;
   settings row `agentRelease`) and the owner switches `autoUpdate` on for the node. An owner can also send the
   `update_agent` command with a version, which works with `autoUpdate` off.
2. The agent sees a valid release that differs from its own version and has not failed recently, leaves
   `request.json` (just the wanted version) in its inbox and stops cleanly three seconds later, after the command
   acknowledgement has been sent. systemd restarts the service (`Restart=always`).
3. `ExecStartPre=-+/usr/local/bin/speedtest-agent update-apply` runs as root. It **does not trust the request**: it
   fetches the release from the center itself with the node credential, requires the same version, downloads it over
   HTTPS (200 MiB limit), verifies the SHA-256 and that the file is an ELF executable for this CPU, then
   hard-links the current binary to `speedtest-agent.prev` and renames the new file over the binary (atomic).
4. The new version starts. After its first successful heartbeat it writes `confirmed` to the inbox. At the next
   start the privileged step sees it, deletes `speedtest-agent.prev` and records `succeeded`.
5. If no heartbeat succeeds within **2 minutes** the new agent exits with an error (a crash or hang is caught by the same
   deadline at the next start). The privileged step then renames `.prev` back over the binary, records
   `rolled_back` and remembers the version so it is never installed again.

A failed attempt (download error, checksum mismatch, wrong architecture, unwritable directory) leaves the installed
binary untouched, records `failed`, and the agent does not retry that version for 6 hours.

## Files

| File | Owner | Purpose |
| --- | --- | --- |
| `/usr/local/bin/speedtest-agent`, `.prev` | root | installed and previous binary |
| `/var/lib/speedtest-agent-update/state.json`, `result.json` | root, world-readable | update in progress, versions never to retry, latest result |
| `/var/lib/speedtest-agent/inbox/request.json`, `confirmed` | agent user | the only things the agent can say to the privileged step; read without following symlinks |

Paths can be overridden with `SPEEDTEST_AGENT_BINARY`, `SPEEDTEST_AGENT_UPDATE_DIR` and `SPEEDTEST_AGENT_INBOX_DIR`.

## Reporting

Every heartbeat carries `agent` (`speedtest-agent/<version>`) and, after an attempt, `updateResult`
(`version`, `status` = `succeeded` | `failed` | `rolled_back`, `ts`, `error`). Errors never contain the release URL or any credential.

## Operating notes

- `systemctl status speedtest-agent` and `journalctl -u speedtest-agent` show the `update-apply` output.
- To cancel a queued update, delete `/var/lib/speedtest-agent/inbox/request.json` before the restart.
- To allow a version again after a rollback, remove it from `failed` in `state.json`.
- The release URL must be HTTPS. A release served only over HTTP (for example a plain-HTTP staging center) cannot be installed by self-update.
