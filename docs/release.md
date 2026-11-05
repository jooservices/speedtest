# Releasing and installing

## Build a release

Requires Go 1.27+, Git, Python 3 and `sha256sum` (the existing development tools).

```bash
make release VERSION=1.2.3          # VERSION uses the exact tag, otherwise 0.0.0-dev+<sha>
```

Writes to `dist/` (git-ignored): static binaries (`CGO_ENABLED=0`, stamped with the
version) `speedtest-agent-linux-amd64`, `speedtest-agent-linux-arm64`,
`speedtest-center-linux-amd64`, `speedtest-migrate-linux-amd64`, the agent unit
`speedtest-agent.service`, `SHA256SUMS` and `release.json`. Verify with
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

## Release advertisement manifest

`make release` writes `dist/release.json` from the same version stamp used in the
binaries and the entries in `SHA256SUMS`:

```json
{"version":"1.2.3","commit":"<git sha>","builtAt":"2026-11-05T00:00:00Z","files":{"speedtest-agent-linux-amd64":"<64 hex sha256>","speedtest-agent-linux-arm64":"<64 hex sha256>"}}
```

The actual inventory also includes the center, migrator and service unit. Exact
version tags strip the optional `v` prefix; untagged builds use
`0.0.0-dev+<short sha>`. `VERSION=x.y.z` overrides the stamp. Publish the whole
directory atomically so binaries, checksums and manifest always agree.

The center reads `SPEEDTEST_RELEASE_DIR/release.json` at most once per minute,
including failed reads. It validates SemVer, nonempty commit, RFC3339 build time,
64-digit hexadecimal hashes, fixed artifact names and existing regular files.
Missing or invalid manifests produce a warning once per refresh and no update
indicator; they never make node/configuration responses fail. Validation checks
the inventory; the updater still verifies the downloaded bytes against its hash.

Agents identify themselves as `speedtest-agent/<version> (<goos>/<goarch>)` in
enrollment and control. The existing node agent column retains this string; owner
responses omit the platform suffix. Old identifiers default to linux/amd64.
Unsupported platforms are never offered an artifact.

When no explicit non-null `settings.agentRelease` exists, `/api/agent/config`
returns the matching `speedtest-agent-<os>-<arch>` entry as `release` (version,
configured public download URL, sha256) only for older stable agents, preventing
automatic downgrades or replacement of development builds. Its existing content
ETag changes with the advertisement. Owner node views use the same release source and set
`updateVersion` only for a strictly newer SemVer version. Equal/newer agents and
development/prerelease agent builds have a null indicator. Explicit stored
`settings.agentRelease` remains authoritative for both views. The signed update
flow is unchanged. `release.json` itself is not publicly served.

Verification: `TestPublishedReleaseFlow` is a browser-free integration test of
publication, per-platform heartbeat storage, owner list/detail, agent config,
real artifact downloads, explicit overrides and invalid-manifest fallback.
Browser scenario `SC-NODE-08` verifies that a published release enables the Update
action and queues the advertised version through the real center.
