# Remote SSH installer

The owner API installs or repairs one independent node. The wizard UI belongs to
JOOST-41. Configure `SPEEDTEST_PUBLIC_URL`, `SPEEDTEST_RELEASE_DIR` and the private
32-byte base64 `APP_KEY` in the center's `.env` (defaults in `.env.example`).
Build the release from this revision: the agent must support `enroll --token-stdin
--repair`, and the release directory must contain both Linux architectures,
`speedtest-agent.service` and `SHA256SUMS`.

## Owner flow

1. POST `/api/installer/probes` with `{host, port}` and `Idempotency-Key`.
   This sends no SSH credential. Display the returned SHA256 fingerprint for
   explicit confirmation; probes expire after ten minutes.
2. POST `/api/nodes/{id}/installations` with the confirmed `probeId`,
   `fingerprint` and `ssh` credentials, plus a new `Idempotency-Key`.
   Host and port must match the probe; it is consumed by one request.
   A changed SSH host key aborts before authentication.
3. The 202 response supplies `Location: /api/installations/{id}`.
   GET that resource or stream `/api/installations/{id}/events`.
   Reconnect SSE with `Last-Event-ID` to replay subsequent progress; an expired
   cursor returns 409. Each successful JSON response includes `meta: {}`.
   Every owner operation requires a session; writes also require CSRF and Origin.

Passwords, unencrypted private keys and a separate optional sudo password are
accepted only after confirmation. The account must support noninteractive sudo
validation and root shell execution. Passwordless sudo is supported. Encrypted
private keys and interactive authentication prompts are not supported.

## Installation and repair

The fixed script supports systemd Linux on amd64 and arm64. It downloads the
binary, unit and checksum manifest from the configured center, verifies both
downloaded artifacts, prepares the dedicated account and private credential
directory, and starts the service. A fresh heartbeat must follow the restart
before the job succeeds.

A one-time enrollment token is issued for the selected existing node and sent
through an SSH side channel into a FIFO, then to the agent's standard input.
`agent enroll --repair` preserves an existing credential when authenticated
configuration retrieval succeeds. Missing/rejected credentials are enrolled
again. A transient center failure aborts rather than replacing a healthy
credential. Enrollment and SSH secrets never appear in remote command arguments,
persisted jobs/events or diagnostics; idempotency stores a keyed request digest.

Four workers and a bounded 16-job memory queue limit concurrent work.
Only one job may be active for a node. Repeating the same owner/key/request
replays the job; changing its node or body returns 409. A new confirmed probe
and request key permit another upgrade or repair.

## Failure and recovery

The installer backs up an existing binary, service and configuration, including
the previous enabled/active service state. Failure restores them; a fresh failed
installation removes the new account and files. Once a heartbeat is confirmed,
temporary backup/configuration/FIFO state is removed. If SSH disconnects, rollback
tries a new connection against the same pinned fingerprint. An unreachable
machine cannot be cleaned remotely: restore SSH access and retry; do not assume
that a disconnected host is clean.

SSH credentials exist only in the bounded worker memory and cannot survive a
center restart. Previously pending/running jobs become failed on startup; the
owner must submit credentials again. Job history is retained for thirty days,
with pruning on subsequent installation requests. Unused enrollment tokens are
revoked after the attempt.

## Removing the agent (uninstall)

`POST /api/nodes/{id}/uninstallations` takes the same body as an installation (a freshly confirmed probe plus SSH
credentials, used once and never stored) and an `Idempotency-Key`; it answers `202` with `Location: /api/installations/{id}`.
The job connects with the same pinned host key and sudo check, then runs a fixed script that stops the service and
deletes only the agent's own files: `/etc/systemd/system/speedtest-agent.service`, `/usr/local/bin/speedtest-agent` (and
`.prev`), `/etc/speedtest-agent`, `/var/lib/speedtest-agent`, `/var/lib/speedtest-agent-update` and the
`speedtest-agent` account. Progress arrives as the same SSE events (stop, files, account, removed). The job never
deletes the node: the dashboard deletes it only after the removal succeeded, or when the owner chooses to remove it from
the dashboard anyway. An idempotency key used for an installation never replays an uninstallation.

When SSH is not possible, run this on the machine instead (as root):

```sh
sudo speedtest-agent uninstall --all
```

`agent uninstall` alone keeps the old behaviour (credential and configuration only); `--all` also stops the service and
removes the binary, state directories and account, `--keep-user` keeps the account. Both are idempotent and restricted to
the paths above; `--root <dir>` runs against a scratch directory for tests.

## Verification

Run from a configured worktree using its disposable database:

```sh
make ci
go test -race ./...
go test -p 1 -parallel 1 -tags integration -race ./... -count=1
go test -p 1 -parallel 1 -tags integration -race ./internal/center/installer \
  -count=1 -coverprofile=.tmp/installer.cover
go tool cover -func=.tmp/installer.cover
go test -p 1 -parallel 1 -tags integration -race ./internal/center/server \
  -run TestInstallerHTTPSSHEnrollmentEndToEndIntegration -count=3 -v
govulncheck ./...
```

Database fixtures call `db.CheckDestructiveAllowed` (`APP_ENV` must be `local` or `test`) before destructive setup. Never run these suites
concurrently on the same database.

`TestRemoteScriptLifecycle` executes the real shell scripts on a disposable
filesystem with fake download/system tools: amd64, arm64, checksum rejection,
unsupported architecture, commit cleanup and restoration of an existing install.
`TestSudoCommandKeepsPayloadSeparate` covers password, cached, NOPASSWD and denied
sudo without exposing the password as shell input.

`TestInstallerHTTPSSHEnrollmentEndToEndIntegration` exercises the backend flow
through HTTP, SSH, PostgreSQL, real token exchange, authenticated heartbeat,
SSE and final status. Browser wizard coverage remains with JOOST-41/JOOST-48;
this ticket changes no browser code.
