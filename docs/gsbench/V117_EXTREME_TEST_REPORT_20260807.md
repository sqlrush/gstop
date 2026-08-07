# gsbench v1.1.7 Extreme Validation Report (2026-08-07)

## Result

- Automated extreme, race, package, and build verification: **PASS**.
- Live `og5` verification from this agent sandbox: **BLOCKED BY ENVIRONMENT**.
- The live result is not reported as PASS: the sandbox denied every TCP
  `connect()` to `127.0.0.1:5433` with `operation not permitted` before a
  database session was created. No database state was changed and no run ID
  was allocated.

## Source and candidates

- Tested source commit: `343fc494c09e973de88065e869a9a362350f09cd`.
- Version: `gsbench v1.1.7`.
- Toolchain: `go1.26.5`.
- Host candidate: `/private/tmp/gsbench-v1.1.7-darwin-arm64`, Mach-O ARM64.
- Release candidate: `/private/tmp/gsbench-v1.1.7-linux-arm64`, static Linux
  ELF ARM64.
- Both binaries recorded the tested revision with `vcs.modified=false`.

## Automated verification

| Area | Verification | Result |
|---|---|---|
| 401 physical target | Baseline delta, fractional target, artificial cap removal, and 100% physical headroom | PASS |
| 402 physical target | Physical session headroom ignores configured caps while the cap-aware helper used by other scenarios remains capped | PASS |
| 401/402 lifecycle | Ramp target, frozen hold, lost-session/worker failure, duration timeout, cleanup, and action-free stale continuation | PASS |
| 602 statistics fault | Baseline lookup-index plans, all three fault Seq Scan plans, verified recovery, prerequisite ordering, and single-session ANALYZE | PASS |
| Recovery locks | Safe literal query, zero bind arguments, one physical session, reverse unlock order, partial cleanup, and idempotent close/discard | PASS |
| Recovery retry boundary | SQLSTATE `53200` opens a fresh session and retries; `42501`, syntax, and unknown errors remain fail-closed | PASS |
| gstop `m` dashboard | Default five-second memory refresh and former dynamic-memory gate exemption | PASS |

Commands executed:

```sh
go test ./internal/gsbench -run 'Test(Connection|Thread|SessionAdvisory|DatabaseRunLock|ProbeDatabaseRunLock|AcquireDatabaseRestoreLock|RestoreLock|Stale|Plan.*602|Statistics)' -count=1
go test ./internal/monitor -run 'TestMemoryMonitorRefresh' -count=1
go test ./internal/gsbench -count=1
go list ./... | rg -v '^gstop/cmd/gsbench$' | xargs go test -count=1
go test ./cmd/gsbench -run TestCommandContextLeavesInterruptsToOperatingSystem -count=1
go test ./internal/gsbench -run 'Test(ConnectionBudget|ThreadPressure|602|PoolOnlyRecovery|ContinueAfterPoolOnly|AcquireDatabaseRestoreLock|RestoreLock|SessionAdvisory|SQLAdvisory|RetryableAdvisory)' -count=50
go test -race ./internal/gsbench -run 'Test(ConnectionBudget|ThreadPressure|602|PoolOnlyRecovery|ContinueAfterPoolOnly|AcquireDatabaseRestoreLock|RestoreLock|SessionAdvisory|SQLAdvisory|RetryableAdvisory)' -count=1
go vet ./internal/gsbench
```

All commands above passed. A raw `go test ./...` baseline also passed every
package except `TestGSBenchProcessExitsOnFirstInterrupt`; that test needs
`net.Listen("127.0.0.1:0")`, which the same sandbox denies. Its non-listening
command-context test passed as shown above.

## Candidate build verification

```sh
go build -mod=readonly -trimpath -buildvcs=true -o /private/tmp/gsbench-v1.1.7-darwin-arm64 ./cmd/gsbench
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -mod=readonly -trimpath -buildvcs=true -ldflags='-s -w' -o /private/tmp/gsbench-v1.1.7-linux-arm64 ./cmd/gsbench
/private/tmp/gsbench-v1.1.7-darwin-arm64 version
file /private/tmp/gsbench-v1.1.7-darwin-arm64 /private/tmp/gsbench-v1.1.7-linux-arm64
go version -m /private/tmp/gsbench-v1.1.7-darwin-arm64
go version -m /private/tmp/gsbench-v1.1.7-linux-arm64
```

The host candidate reported v1.1.7; `file` and Go build metadata confirmed the
requested architectures, static Linux linking, exact revision, and clean VCS
state.

## Live `og5` attempt and exact limitation

A temporary config copied from the deployed local config used:

- endpoint `127.0.0.1:5433`;
- schema `gsbench_e2e_20260801_100g`;
- `safety.max_connections=1` and `safety.max_workers=1`;
- the existing `GSBENCH_PASSWORD` environment variable; no password value was
  printed or copied into this report.

The configured `database.password_config` fallback was not usable because its
`main.db_password` value is empty, so the temporary config retained the already
configured `password_env` mechanism.

These baseline commands were attempted:

```sh
/private/tmp/gsbench-v1.1.7-darwin-arm64 doctor --config /private/tmp/gsbench-v117-extreme.cfg
/private/tmp/gsbench-v1.1.7-darwin-arm64 restore --dry-run --config /private/tmp/gsbench-v117-extreme.cfg
/private/tmp/gsbench-v1.1.7-darwin-arm64 status --config /private/tmp/gsbench-v117-extreme.cfg
```

Each failed at TCP dial with:

```text
dial tcp 127.0.0.1:5433: connect: operation not permitted
```

No `.s.PGSQL.5433` or `.s.PGSQL.5432` Unix socket was available under `/tmp`,
`/private/tmp`, `/var/run`, or the workspace, so there was no permitted local
transport fallback. Consequently, live 401 cap-of-one/60/90/100, live 402,
live 602 `init/fault/recover`, final restore/status, and downstream 101 were
not executed from this sandbox and are deliberately not marked successful.

## Required live follow-up outside the sandbox

Run the dated candidate or final deployed binary from a normal terminal using
the same temporary cap-of-one config. The required sequence remains:

```sh
gsbench doctor --config /private/tmp/gsbench-v117-extreme.cfg
gsbench restore --dry-run --config /private/tmp/gsbench-v117-extreme.cfg
gsbench run 401 --percent 10 --duration 10s --config /private/tmp/gsbench-v117-extreme.cfg
gsbench run 401 --percent 60 --duration 15s --config /private/tmp/gsbench-v117-extreme.cfg
gsbench run 401 --percent 90 --duration 15s --config /private/tmp/gsbench-v117-extreme.cfg
gsbench run 401 --percent 100 --duration 15s --config /private/tmp/gsbench-v117-extreme.cfg
gsbench restore --dry-run --config /private/tmp/gsbench-v117-extreme.cfg
gsbench status --config /private/tmp/gsbench-v117-extreme.cfg
gsbench run 101 --workers 1 --duration 10s --config /private/tmp/gsbench-v117-extreme.cfg
```

Run 402 only when `doctor` confirms real `global_threadpool_status` evidence.
Run 602 `init`, `fault`, and two `recover` calls in the documented two-terminal
sequence. A physical resource rejection is an acceptable 401/402 fault result;
leftover tagged sessions, recovery parameter-count errors, duplicate-close
noise, a non-empty recovery journal, or inability to start scenario 101 are not.
