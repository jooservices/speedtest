# JOOST-106 verification evidence

Run on 2026-11-17 after `/home/joos/Workspaces/scripts/dev/guard` completed successfully:

```text
postgres       ok
tree joost-106: ok
```

## Complexity scan

Ran the default build with test files included, enabling only `gocyclo` at threshold 30:

```sh
golangci-lint run --config .tmp/joost-106-gocyclo30.yml --timeout 10m
```

The temporary config set `run.tests: true`, `linters.default: none`, enabled `gocyclo`, and set `linters.settings.gocyclo.min-complexity: 30`. The scan reported one function:

```text
../internal/center/alerts/emit_test.go:83:1: cyclomatic complexity 44 of func `(*fakeTx).QueryRow` is high (> 30) (gocyclo)
1 issues:
* gocyclo: 1
```

## Internal Go tests

The inherited shell umask was `0077`. The requested command, run with that umask, failed only at `TestLoadCredentialWorldReadable`:

```sh
go test ./internal/... -count=1
```

```text
--- FAIL: TestLoadCredentialWorldReadable
    enroll_test.go:327: LoadCredential() error = <nil>, want a permission error mentioning 0600
FAIL github.com/jooservices/speedtest/internal/agent/enroll
```

All other `internal/...` packages passed in that run. With a conventional `0022` umask, the same test command passed across all internal packages:

```sh
(umask 0022; go test ./internal/... -count=1)
```
