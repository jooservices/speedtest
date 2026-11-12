# Quality tooling

## Go function complexity

The project enables `funlen` and `gocyclo` in `.golangci.yml`. The baseline was measured with Go 1.27.3 and golangci-lint v2.15.0 on 2026-11-12, including test files. Integration-tagged test files set the current maxima:

| Check | `go test` build | Integration build | Configured limit |
| --- | ---: | ---: | ---: |
| `funlen` lines | 177 | 270 | 271 lines |
| `funlen` statements | 79 | 192 | 193 statements |
| `gocyclo` complexity | 42 | 119 | 120 |

The integration maxima are `TestNotificationCenterEndToEndIntegration` (270 lines and 187 statements) and `TestOwnerStatisticsHTTPFlowIntegration` (complexity 119). No existing functions were changed to establish these limits. Both standard and integration lint commands include tests, so the limits are one above the measured worst cases. As quality work simplifies existing product and test functions, lower the limits in small steps and keep both lint commands green.

CI uses the same project configuration and explicitly enables both linters. Run `golangci-lint run` and `golangci-lint run --build-tags integration` locally.

## Installer under systemd

`make test-install-systemd` builds an Ubuntu 24.04 image with systemd as PID 1, serves fake release files inside the container, and runs the normal root install path. It checks that `speedtest-agent.service` is installed, enabled, and active, then runs `internal/center/installer/uninstall_remote.sh` and checks the service and installed files are removed. This test is separate from `make ci` because it needs Docker. It prints a clear `SKIP` and exits successfully only when the Docker CLI is missing. If the Docker daemon denies the current group, the script retries through `sg docker -c`.

## UI JavaScript types

`jsconfig.json` checks only `ui/assets/js/` using TypeScript's JavaScript checker. `allowJs`, `checkJs`, and `noEmit` keep the UI in plain JavaScript with no build step; use JSDoc annotations or narrow, reasoned `@ts-expect-error` comments for typing issues. Run `npm run typecheck` or `make typecheck`.

## Unit-test timing

`make test-timing` first runs the Python unit tests for the budget checker, then runs `go test -json ./...` without integration tags. `docs/test-budget.json` sets a 60-second default per package and a 20-second budget for `internal/center/auth`, based on the 8.74-second maximum observed in the 2026-11-12 baseline run. The script reports packages over budget and exits nonzero; update an override only after measuring a real run.

## Flaky-test record

Known timing-sensitive tests and their mitigations are tracked in [flaky-tests.md](flaky-tests.md). Add an entry when an intermittent failure is understood; do not weaken a test threshold to hide machine-load noise.
