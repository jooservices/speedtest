# JOOST-107 verification evidence

Run on 2026-11-17 in the ticket worktree at commit `c52ed26`, with golangci-lint v2.15.0 and Go 1.27.3.

## Integration build (AC2)

```text
$ golangci-lint run --build-tags integration
0 issues.
exit=0
```

## Scratch non-integration function of complexity 31

A temporary file `internal/center/alerts/zz_scratch_complexity.go` holding `scratchComplexity31` (30 `if` statements, complexity 31) was added, `golangci-lint run` was run, and the file was removed afterwards.

```text
$ golangci-lint run   (scratch file internal/center/alerts/zz_scratch_complexity.go, non-integration, complexity 31)
internal/center/alerts/zz_scratch_complexity.go:3:1: cyclomatic complexity 31 of func `scratchComplexity31` is high (> 30) (gocyclo)
func scratchComplexity31(x int) int {
^
1 issues:
* gocyclo: 1
exit=1
```

The scratch function is reported at complexity 31 against the limit of 30, and the integration build exits zero.
