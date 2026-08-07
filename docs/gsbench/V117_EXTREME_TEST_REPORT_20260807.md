# gsbench v1.1.7 Extreme Validation Report (2026-08-07)

## Result

- Automated, race, live database, recovery, interrupt, and downstream smoke
  verification: **PASS**.
- Runtime source tested: `973a37c8c41370410a4fa70e3e1d8922770dd23d`.
- Test-harness-only follow-up: `b9467fc1c9af133de831e59fa09ef82c639915a8`.
- Version/toolchain: `gsbench v1.1.7`, Go `1.26.5`.
- Live target: openGauss-lite 5.0.3 standalone `og5`, endpoint
  `127.0.0.1:5433`, dataset `gsbench_e2e_20260801_100g`.

The high-percentage 401/402 cases intentionally reached the database's real
resource boundary. Their workload outcome is `FAILED` because openGauss
returned `memory is temporarily unavailable`; gsbench did not silently lower
the requested percentage. The acceptance condition for those cases was a
truthful failure plus automatic `restore SUCCESS` and zero residual recovery
state, which passed.

## Automated verification

| Area | Verification | Result |
|---|---|---|
| 401 target model | Baseline delta, target validation, 100% physical target, and no `safety.max_connections` clamp | PASS |
| 402 target model | Physical worker/session target and no `safety.max_workers` or connection clamp | PASS |
| 401/402 lifecycle | Hold, target failure, database rejection, duration cleanup, Ctrl+C cleanup, and action-free stale recovery | PASS |
| Recovery control plane | Dedicated advisory session, zero-argument ownership query, fresh ownership retries, isolated run recovery pool, and idempotent release | PASS |
| 602 statistics fault | Baseline unique index scan, fault full-table scan, statistics recovery, restored unique index scan, and repeated recover | PASS |
| gstop `m` | Five-second default memory-dashboard refresh without the old dynamic-memory health gate | PASS |
| Regression | All repository packages, vet, focused repeat tests, and race detector | PASS |

Final commands included:

```sh
go test ./... -count=1
go vet ./...
go test ./cmd/gsbench -run '^(TestCommandContextCancelsOnFirstInterrupt|TestGSBenchSecondInterruptTerminatesBlockedDriver)$' -count=10
go test ./internal/monitor -run '^(TestMemoryMonitorDefaultsToFiveSecondInterval|TestMemoryMonitorRefreshIgnoresDynamicMemoryHealthGate)$' -count=10
go test -race ./internal/gsbench ./cmd/gsbench ./internal/monitor -count=1
```

All commands passed. Unlike the earlier restricted run, the full process test
could bind its local listener and is now included in `go test ./...`.

## Live configuration

A temporary copy of the local configuration used:

- `safety.max_connections=1` and `safety.max_workers=1`;
- runtime validation enabled;
- existing `GSBENCH_PASSWORD` environment lookup (no password copied here);
- normal `2s` ramp for 401, then `100ms` for bounded 402 extreme tests.

The cap-of-one values deliberately prove that only scenarios 401/402 ignore
their artificial safety clamps. Scenario 602 still enforced `max_workers=1`:
`--worker 5` was rejected, while `--worker 1` ran successfully.

## 401 connection-pool evidence

| Test | Run ID | Observed result | Recovery |
|---|---|---|---|
| 10%, cap-of-one | `20260807T103401-1yq0s` | target 75, actual 75, 10.03%, zero errors | SUCCESS |
| 90%, final code | `20260807T110807-1ggpo` | physical memory rejection after 614 operations | SUCCESS |
| 100%, final code | `20260807T110820-9o30q` | physical memory rejection after 661 operations | SUCCESS |
| target below baseline | `20260807T111207-8yahq` | rejected: target 1.0% below baseline 2.5% | SUCCESS |
| Ctrl+C before duration | `20260807T112118-14zme` | `hold: context canceled`, 900 operations, zero workload errors | SUCCESS |

Both 90% and 100% were followed by `restore --dry-run` with `runs=0 actions=0`.
The target was never reduced to an artificial “reachable” percentage.

## 402 thread-pool evidence

| Test | Run ID | Observed result | Recovery |
|---|---|---|---|
| 10%, cap-of-one | `20260807T110952-5qvjf` | reached 10.0%; 1,106 operations; zero errors | SUCCESS |
| 90%, final code | `20260807T111037-5iovq` | physical memory rejection; 60 recorded errors | SUCCESS |
| 100%, final code | `20260807T111129-22uks` | physical memory rejection; 40 recorded errors | SUCCESS |
| Ctrl+C before duration | `20260807T112957-3lvvx` | `hold: context canceled`; 5,145 operations; zero workload errors | SUCCESS |

The successful 10% run reported 800 actual thread-pool workers and therefore
could not have been clamped by the configured `safety.max_workers=1`. Each high
pressure or interrupt case was followed by zero residual recovery state.

## 602 statistics plan-change evidence

Two-terminal sequence:

```sh
gsbench run 602 init --worker 1 --duration 2m
gsbench run 602 fault
gsbench run 602 recover
gsbench run 602 recover
```

- Workload run `20260807T112612-7tynx` started with three verified
  `plan_data_lookup_idx` index-scan candidates and completed 274,690 operations.
- Fault run `20260807T112726-2918d` returned `plan fault SUCCESS`; its fault
  verifier confirmed the lookup statements had changed to sequential scans.
- Recovery restored the statistics and ANALYZE baseline, then returned
  `plan recover SUCCESS`; its verifier confirmed the unique index scans again.
- A second recover returned `ALREADY_RECOVERED` with exit status 0.

## Bugs found during live pressure and permanent fixes

The extreme run found issues that unit-only testing had not exposed:

1. Recovery advisory locks previously shared a stressed pool and could lose
   ownership. Locks now use one dedicated physical session.
2. The openGauss v1.0.8 driver misparsed recovery prepared parameters under
   pressure. Advisory and ownership queries now use safe literals/zero bind
   arguments.
3. Retrying ownership on a protocol-uncertain connection produced parameter
   count and simple-query response errors. Failed ownership sessions are now
   discarded and retried on fresh dedicated sessions.
4. Later discovery still reused the workload control pool. Each run now owns a
   separate, initially unconnected recovery control pool for the complete
   recovery path.
5. SIGINT previously terminated the process before Runner cleanup. The first
   interrupt now cancels gracefully so Stop/Restore runs; signal defaults are
   restored immediately so a second interrupt can terminate a stuck driver.

Final 90%/100% and Ctrl+C reruns prove the corrected paths return
`restore.state=restored`, `restore.outcome=SUCCESS`, and leave no pending run or
action. Explicit restore remains available for a process killed by SIGKILL or a
second interrupt.

## Downstream and final state

After all high-pressure and plan-change tests, scenario 101 run
`20260807T113651-1r7rm` completed 3,444 operations in 10 seconds with zero
errors and `restore SUCCESS`. The final command was:

```text
restore SUCCESS (dry run) runs=0 actions=0
```

This verifies that failed pressure scenarios do not block subsequent scenarios
and that the test database ended without pending gsbench recovery work.
