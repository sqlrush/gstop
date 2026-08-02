# gsbench 201/202 Work-Memory Pressure Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make scenarios 201 and 202 run fixed, user-selected workers that hold one calibrated in-memory Sort or Hash operator for the requested duration and exit on the first Ctrl+C.

**Architecture:** Add typed memory-pressure configuration and CLI overrides, isolate calibration/SQL/lifecycle code in a dedicated memory scenario module, and route 201/202 factories to it while leaving other resource scenarios unchanged. Calibration parses real EXPLAIN ANALYZE evidence and derives a bounded `dist_key` range before worker sessions start.

**Tech Stack:** Go, `database/sql`, openGauss SQL, existing gsbench Runner/WorkerGroup/TaggedConn abstractions.

## Global Constraints

- Preserve all existing uncommitted 101–103 fixed-worker and single-SIGINT changes.
- Default 201/202 configuration remains one worker and 256MB.
- `work_mem` input is canonicalized to integer kB before SQL interpolation.
- No workload row-count CLI option is exposed.
- Runtime model validation remains disabled by default; mandatory workload calibration is independent.
- A single Ctrl+C terminates the OS process immediately.

---

### Task 1: CLI and typed configuration

**Files:**
- Modify: `internal/gsbench/cli.go`
- Modify: `internal/gsbench/cli_test.go`
- Modify: `internal/gsbench/config.go`
- Modify: `internal/gsbench/config_test.go`
- Modify: `internal/gsbench/app.go`
- Modify: `configs/gsbench.cfg`

**Interfaces:**
- Consumes: existing `CLIOptions`, `Overrides`, `BenchConfig`, and 101–103 worker override behavior.
- Produces: `CLIOptions.WorkMemKB`, `Overrides.WorkMemKB`, and per-code memory pressure policies for 201/202.

- [ ] Write failing CLI tests for `run 201/202 --workers --work-mem --duration`, unsafe units, zero values, and incompatible scenarios.
- [ ] Run the focused CLI tests and confirm failure because `--work-mem` is absent and 201/202 reject `--workers`.
- [ ] Implement strict work-memory parsing, compatible worker routing, usage text, override propagation, and config defaults.
- [ ] Run focused CLI/config tests and confirm they pass.

### Task 2: SQL builders, plan evidence, and calibration search

**Files:**
- Create: `internal/gsbench/scenario_workmem.go`
- Create: `internal/gsbench/scenario_workmem_test.go`
- Modify: `internal/gsbench/resource_workloads.go`
- Modify: `internal/gsbench/resource_workloads_test.go`

**Interfaces:**
- Consumes: quoted dataset schema, typed work-memory policy, `scanExplainRows`, and tagged connections.
- Produces: bounded 201/202 calibration SQL, cursor SQL, `memoryPlanEvidence`, and `calibrateMemoryRange`.

- [ ] Write failing tests that require 201 to force one Sort, 202 to force one wide Hash build without `GROUP BY`, and both to use `dist_key` bounds.
- [ ] Write failing table-driven tests for quicksort/hash plan parsing and in-memory/spill calibration decisions.
- [ ] Run focused tests and confirm failures name the missing builders/parser/calibrator.
- [ ] Implement the minimal safe SQL builders, plan parser, and bounded search.
- [ ] Run focused tests and confirm they pass.

### Task 3: Fixed worker cursor lifecycle and cancellation

**Files:**
- Modify: `internal/gsbench/scenario_workmem.go`
- Modify: `internal/gsbench/scenario_workmem_test.go`
- Modify: `internal/gsbench/scenario_common.go`
- Modify: `internal/gsbench/scenario_common_test.go`
- Modify: `internal/gsbench/runner.go` only if the existing duration-owner contract cannot express the lifecycle.

**Interfaces:**
- Consumes: `sqlWorkload`, `WorkerGroup`, tagged connections, and `workloadDurationOwner`.
- Produces: 201/202 scenarios whose Ramp waits for every cursor to become ready, Hold owns the exact duration, and Stop closes/rolls back each session.

- [ ] Write failing lifecycle tests using a controlled SQL driver for BEGIN/SET/DECLARE/FETCH/hold/CLOSE/ROLLBACK ordering.
- [ ] Write a failing cancellation test proving Hold returns promptly and cleanup runs after one context cancellation.
- [ ] Run focused lifecycle tests and confirm the expected failures.
- [ ] Implement the dedicated scenario factory, ready barrier, duration ownership, evidence, cleanup, and factory routing.
- [ ] Run focused lifecycle and factory tests and confirm they pass.

### Task 4: Documentation and verification

**Files:**
- Modify: `docs/gsbench/CONFIG.md`
- Modify: `docs/gsbench/README.md`
- Modify: `configs/gsbench.cfg`

**Interfaces:**
- Consumes: final CLI/config behavior.
- Produces: operator-facing commands and exact calibration semantics.

- [ ] Document 201/202 commands, defaults, target band, spill failure, and Ctrl+C behavior.
- [ ] Run `gofmt` on changed Go files.
- [ ] Run focused tests for CLI, config, memory scenario, worker lifecycle, and SIGINT.
- [ ] Run `go test ./internal/gsbench ./cmd/gsbench` and `go build ./cmd/gsbench`.
- [ ] Run one short og5 smoke test with a safe work_mem and verify tagged sessions disappear after completion/cancellation.
- [ ] Request the platform-required single focused code review and resolve Critical/Important findings.
