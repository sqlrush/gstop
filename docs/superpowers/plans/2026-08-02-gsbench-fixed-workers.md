# gsbench 101–103 Fixed-Worker Implementation Plan

> **Goal:** Replace CPU-feedback control in scenarios 101–103 with the
> sysbench model: exact fixed workers, unlimited per-worker event loops, and a
> hard workload duration.

**Architecture:** Extend `WorkerGroup` with an initialization/start barrier and
started/peak counters. Pre-open one tagged session per requested worker during
Prepare, stage all worker goroutines during Ramp, release them together at the
beginning of Hold, and stop/join them at the duration deadline. A small shared
fixed-worker runner handles one lane for 101/102 and two synchronized lanes for
103. CLI/config selects worker counts; no CPU sample changes those counts.

**Reference:** Upstream sysbench `src/sysbench.c` uses fixed `--threads`, waits
on `worker_barrier`, then repeatedly checks `sb_more_events()` and executes the
next event until `--time` expires. Its default `--rate=0` path has no producer
queue.

---

## Task 1: Add worker parameters to CLI and configuration

**Files:**

- Modify: `internal/gsbench/cli.go`
- Modify: `internal/gsbench/cli_test.go`
- Modify: `internal/gsbench/config.go`
- Modify: `internal/gsbench/config_test.go`
- Modify: `internal/gsbench/app.go`

1. Add failing CLI tests for `--workers`, `--tp-workers`, and `--ap-workers`,
   including positive-number validation, pair validation, command/scenario
   compatibility, and help text.
2. Run the focused CLI tests and confirm they fail for missing options.
3. Add failing config tests for the four scenario worker defaults, CLI override
   precedence, and total hard-cap validation.
4. Run the focused config tests and confirm they fail.
5. Add the CLI fields and parse/usage logic. Extend `Overrides` and introduce a
   strongly typed fixed-worker scenario config with defaults of one worker.
6. Apply overrides after final scenario resolution. Keep
   `safety.max_workers/max_connections` only as hard ceilings. Remove global CPU
   target parsing/validation because 101–103 no longer consume it.
7. Run focused CLI/config tests.

## Task 2: Add a sysbench-style worker start barrier

**Files:**

- Modify: `internal/gsbench/workers.go`
- Modify: `internal/gsbench/workers_test.go`

1. Add failing tests proving N workers initialize but execute no operations
   before release, all N execute after one shared release, and deadline/stop
   prevents any further operation starts.
2. Add failing tests for `Started` and `PeakActive` evidence and waiting for all
   workers to initialize.
3. Run focused worker tests and confirm RED.
4. Add a start-gated WorkerGroup constructor, readiness notification,
   `WaitReady`, and immutable started/peak counters. Preserve the existing
   immediately-running constructor for all other scenarios.
5. Run focused worker tests and the existing worker suite.

## Task 3: Add fixed SQL workload lifecycle

**Files:**

- Modify: `internal/gsbench/scenario_common.go`
- Modify: `internal/gsbench/scenario_common_test.go`
- Add: `internal/gsbench/fixed_workers.go`
- Add: `internal/gsbench/fixed_workers_test.go`

1. Add failing tests for concurrent tagged-session preparation, synchronized
   one/two-lane release, full-duration execution, simultaneous deadline stop,
   final snapshot preservation, and cancellation error normalization.
2. Run focused tests and confirm RED.
3. Refactor tagged-session acquisition so different worker IDs can connect in
   parallel without holding the session map lock across network I/O.
4. Add fixed workload construction with a shared start gate and pre-opened
   sessions.
5. Implement the fixed run lifecycle: Prepare sessions, Ramp to exact targets
   and wait ready, start timer then release, stop all lanes concurrently at the
   deadline, join workers, and retain final snapshots.
6. Add fixed-worker evidence helpers for requested versus started/peak workers,
   operations/errors, elapsed duration, throughput, and separate 103 lanes.
7. Run focused tests.

## Task 4: Replace scenarios 101–103

**Files:**

- Modify: `internal/gsbench/scenario_tp.go`
- Replace: `internal/gsbench/scenario_ap.go`
- Replace: `internal/gsbench/scenario_mixed.go`
- Modify: `internal/gsbench/scenario_cpu_test.go`
- Modify: `internal/gsbench/workload_catalog.go`
- Modify: `internal/gsbench/runner.go`
- Modify: `internal/gsbench/runner_test.go`

1. Replace feedback-oriented scenario tests with failing fixed-count tests for
   101, 102, and separate 103 TP/AP lanes. Assert strategy names no longer say
   feedback and CPU samples never participate.
2. Add a runner test proving these scenarios own their pressure timer only
   after all workers reach the start barrier, while existing scenarios retain
   the shared ramp+hold deadline behavior.
3. Run focused tests and confirm RED.
4. Implement 101 and 102 as one-lane fixed runs and 103 as a two-lane fixed run
   sharing one release gate. Use the full 102 AP statements in 103.
5. Make Verify check execution/setup errors and exact started counts only; CPU
   utilization is absent from result evidence and outcome logic.
6. Run the scenario and runner focused suites.

## Task 5: Remove 101–103 CPU feedback code

**Files:**

- Delete: `internal/gsbench/cpu_actuator.go`
- Delete: `internal/gsbench/cpu_actuator_test.go`
- Modify: `internal/gsbench/scenario_common.go`
- Modify: `internal/gsbench/scenario_common_test.go`
- Modify: `internal/gsbench/app.go`
- Modify: `internal/gsbench/runner.go`

1. Remove actuator, fractional duty throttle, CPU target verification/evidence,
   and runtime CPU sampler wiring from scenarios 101–103.
2. Keep shared `Controller`, `continuousControl`, and safety fields used by
   non-101–103 scenarios.
3. Use `rg` to prove no 101–103 source or documentation references CPU target,
   feedback, duty throttling, TP percentage splitting, or dynamic max workers.
4. Run `go test ./internal/gsbench -count=1`.

## Task 6: Update shipped configuration and operator documentation

**Files:**

- Modify: `configs/gsbench.cfg`
- Modify: `docs/gsbench/CONFIG.md`
- Modify: `README.md`
- Modify scenario validation/report docs or scripts only where they describe
  101–103 CPU target behavior.

1. Replace the CPU target/max-worker examples with exact worker keys and
   commands.
2. Document defaults, CLI precedence, 103 total concurrency, full-speed loops,
   deadline semantics, and the fact that CPU is observation-only.
3. Update scenario validation expectations from CPU convergence to tagged
   session counts and traffic cutoff.
4. Run relevant shell/help tests if scripts changed.

## Task 7: Build and verify in `og5` with gstop

**Files:**

- Add/update only test artifacts under an ignored temporary/release directory
  if needed; do not alter or clean the retained 100GB schema.

1. Run `gofmt` on changed Go files.
2. Run focused tests, then `go test ./internal/gsbench -count=1` and build the
   Linux ARM64 test binary.
3. Start gstop passive observation for container `og5`.
4. Run short, explicit tests such as 101 with 3 workers, 102 with 2 workers,
   and 103 with 2 TP plus 1 AP worker against the retained initialized schema.
5. During Hold, query tagged sessions and compare observed per-scenario TP/AP
   counts with the supplied parameters. After duration, confirm they return to
   zero and operations no longer increase.
6. Stop after all three counts match. Preserve the 100GB dataset and wait for
   further user instruction.
