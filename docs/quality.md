# Quality tooling

## Go function complexity

Two passes check function size. Both read the repository configuration files.

1. **Repository gate (`.golangci.yml`).** `funlen` and `gocyclo` are enabled for every package. `gocyclo` has `min-complexity: 30`, so no function outside the exclusion may exceed complexity 30. `funlen` has the limits of 286 lines and 193 statements.
2. **New-code gate (`.golangci-new-code.yml`, `make lint-new-code BASE=<ref>`).** Runs `gocyclo` at `min-complexity: 10` with `--new-from-merge-base`. `gocyclo` reports a function at its declaration line, and the merge-base filter keeps only findings on lines changed since `BASE`. So this pass checks a function only when its declaration line is in the diff: a new function, or an existing one whose signature changed. An existing function whose body changed but whose signature did not is not checked here; it is covered by the repository gate at 30 only, so keep changed bodies within 10 in review. Lowering the new-code threshold is a separate change.

### Exclusion

Integration test files are excluded from `gocyclo` only. The rule in `.golangci.yml` matches any file named `integration_test.go` or `*_integration_test.go`, and `funlen` still applies to them. Their long end-to-end flows are the reason for the exclusion; do not use the exclusion for any other file.

### Baseline

The baseline was measured with Go 1.27.3 and golangci-lint v2.15.0. The gated `gocyclo` maximum was re-measured on 2026-11-17 after the JOOST-105 to JOOST-107 refactors, with the issue caps disabled so no finding was hidden.

| Check | `go test` build | Integration build | Configured limit |
| --- | ---: | ---: | ---: |
| `gocyclo` complexity, all gated files | 29 | 29 | 30 |
| `gocyclo` complexity, integration test files (excluded) | not gated | 119 | not gated |
| `funlen` lines | 177 | 281 | 286 lines |
| `funlen` statements | 79 | 192 | 193 statements |

The gated `gocyclo` maxima are `TestTwoFactorHTTPLifecycle`, `TestFromEngineOK` and `TestBatchOutcomes` (29 each). The excluded integration maxima are `TestOwnerStatisticsHTTPFlowIntegration` (119) and `TestAgentIngestionProtocolIntegration` (83). The integration `funlen` maximum is `TestNotificationCenterEndToEndIntegration` (281 lines and 192 statements, reported by `funlen` at limits of 280 lines and 191 statements). Each `funlen` limit is set above its measured maximum.

### Lowering the ratchet

Lower `gocyclo` `min-complexity` in `.golangci.yml` in small steps, and only after refactoring every function above the new value that the exclusion does not cover. For each step:

1. Run `golangci-lint run --config <temporary config with min-complexity set to the new value>` and fix or split each reported function in the same change.
2. Set `min-complexity` in `.golangci.yml` to the new value.
3. Run `golangci-lint run` and `golangci-lint run --build-tags integration`; both must exit zero.
4. Update the baseline table above with the new measured maxima.

Run `golangci-lint run` and `golangci-lint run --build-tags integration` locally before pushing. `make lint` runs the repository gate and then the new-code gate against `BASE` (default `origin/develop`).

CI (`.github/workflows/lint.yml`) runs the repository gate with `--enable=funlen --enable=gocyclo` on pushes to `develop` and `master` and on every pull request. It runs the new-code gate only for pull requests (`if: github.event_name == 'pull_request'`), so the new-code gate is not run by CI on pushes to `develop` or `master`. CI does not run the integration build.

## Installer under systemd

`make test-install-systemd` builds an Ubuntu 24.04 image with systemd as PID 1, serves fake release files inside the container, and runs the normal root install path. It checks that `speedtest-agent.service` is installed, enabled, and active, then runs `internal/center/installer/uninstall_remote.sh` and checks the service and installed files are removed. This test is separate from `make ci` because it needs Docker. It prints a clear `SKIP` and exits successfully only when the Docker CLI is missing. If the Docker daemon denies the current group, the script retries through `sg docker -c`.

## UI JavaScript types

`jsconfig.json` checks only `ui/assets/js/` using TypeScript's JavaScript checker. `allowJs`, `checkJs`, and `noEmit` keep the UI in plain JavaScript with no build step; use JSDoc annotations or narrow, reasoned `@ts-expect-error` comments for typing issues. Run `npm run typecheck` or `make typecheck`.

## Unit-test timing

`make test-timing` first runs the Python unit tests for the budget checker, then runs `go test -json ./...` without integration tags. `docs/test-budget.json` sets a 60-second default per package and a 20-second budget for `internal/center/auth`, based on the 8.74-second maximum observed in the 2026-11-12 baseline run. The script reports packages over budget and exits nonzero; update an override only after measuring a real run.

## Flaky-test record

Known timing-sensitive tests and their mitigations are tracked in [flaky-tests.md](flaky-tests.md). Add an entry when an intermittent failure is understood; do not weaken a test threshold to hide machine-load noise.
