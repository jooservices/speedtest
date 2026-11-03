# Releasing and installing

## Build a release

```bash
make release VERSION=1.2.3          # VERSION defaults to `git describe`
```

Writes to `dist/` (git-ignored): static binaries (`CGO_ENABLED=0`, stamped with the
version) `speedtest-agent-linux-amd64`, `speedtest-agent-linux-arm64`,
`speedtest-center-linux-amd64`, `speedtest-migrate-linux-amd64`, the agent unit
`speedtest-agent.service` and `SHA256SUMS`. Verify with
`cd dist && sha256sum -c SHA256SUMS`. `speedtest-agent-linux-amd64 version` prints the
stamped version.

## Serve the installer from the command center

Set both variables in the center's `.env` (see `.env.example`):

```bash
SPEEDTEST_PUBLIC_URL=https://speedtest.example.com   # the address agents use
SPEEDTEST_RELEASE_DIR=/opt/speedtest/dist            # the directory made by make release
APP_KEY=...                                           # private base64 encoding of 32 random bytes
```

The center then serves `GET /install.sh` and `GET /download/<artifact>` (only the agent
binaries, the unit and `SHA256SUMS`; nothing else in the directory is reachable). The
public URL is written into the script from configuration, never from the request, so a
forged `Host` header cannot redirect a download. Without these variables both routes are
absent.

The owner-authenticated SSH installer also requires a valid `APP_KEY`, since its
idempotency record covers an in-memory SSH credential request without storing an
offline-guessable credential digest.

## Install an agent

On the node, as root (the dashboard shows the one-time token when you add a node):

```bash
curl -fsSL https://speedtest.example.com/install.sh | sudo sh -s -- --token TOKEN --name vm1
```

The script detects the architecture (x86_64 or aarch64), downloads the agent and
`SHA256SUMS`, **refuses to install on a checksum mismatch**, creates the `speedtest-agent`
system user, enrols the node with the token, installs the systemd unit and starts it. The
token is single use and short lived; this public bootstrap script accepts it as a
command-line argument. The owner-authenticated SSH installer passes its fresh
enrollment token to the agent through standard input instead.

## Tests

`make test-install` runs ShellCheck and `scripts/test-install.sh`, which installs into a
temporary root from a fake download server and covers a normal install on both
architectures, a tampered download, an unsupported architecture, bad input and the
non-root refusal. The Go side is covered by `internal/center/install`.

## Release notes and changelog

Every user-visible change adds a line under `## [Unreleased]` in `CHANGELOG.md`
(Added, Changed, Fixed, Removed, Security). To release: pick the version (SemVer),
rename `Unreleased` to `## [x.y.z] - YYYY-MM-DD`, add a fresh `Unreleased`, run
`make ci && make release VERSION=x.y.z`, tag `vx.y.z` and publish the notes below.
The owner decides when to release; this repository never tags or publishes by itself.

```markdown
## x.y.z - YYYY-MM-DD

Highlights: one or two sentences for a non-technical reader.

### Added / Changed / Fixed / Security
- ...

### Upgrade notes
- Migrations to run, configuration to change, agents to update.

### Checksums
Attach `SHA256SUMS`.
```

## Self-update

Installed agents can update themselves from an advertised release; see [agent-update.md](agent-update.md).
