# gsbench Stabilization and Full-Scenario Validation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the published gsbench source reproducibly buildable, correct every actionable stabilization finding, continuously regulate configured load targets, and produce a truthful 65-scenario validation report around a dedicated 20 GiB dataset.

**Architecture:** Correctness and recovery safety become unconditional program invariants, while `run.validation_enabled` controls only optional result-model judgments. Dataset initialization is staged and mutually exclusive, plan changes share process/database locks, and target-based workloads use one continuous feedback controller across ramp and hold. Live validation is an external serial harness with gsbench generating load and gstop observing it.

**Tech Stack:** Go 1.26.5, `database/sql`, openGauss connector, existing gsbench test fakes, shell-based Linux release and validation scripts.

## Global Constraints

- Work from GitHub `main`, beginning after design commit `2368b44`.
- Keep the 65 implemented three-digit scenarios; do not implement deferred scenarios or legacy aliases.
- Keep `run.validation_enabled = false` as the shipped default.
- Never allow that switch to suppress SQL/connection errors, capacity protection, environment/version checks, mutual exclusion, journal preflight, restoration, or restore verification.
- Run live scenarios serially; do not pass the entire scenario list to one concurrent `run` command.
- Live data must use the dedicated schema `gsbench_e2e_20260801`; cleanup must not target `gsbench` or any business schema.
- Do not claim live 20 GiB or default-load success without new evidence from an accessible Linux GaussDB/openGauss host.

---

### Task 1: Restore clean-clone buildability

**Files:**
- Create: `internal/sqlshape/signature.go`
- Create: `internal/sqlshape/signature_test.go`
- Verify: `internal/gsbench/plan_cache.go`

**Interfaces:**
- Produces: `sqlshape.Canonical(sql string) string`
- Produces: `sqlshape.Signature(sql string) string`

- [ ] **Step 1: Reproduce the clean-tree failure**

Run:

```bash
go test -mod=readonly -count=1 ./internal/gsbench ./cmd/gsbench
```

Expected: FAIL with `package gstop/internal/sqlshape is not in std`.

- [ ] **Step 2: Add canonicalization tests**

Create tests covering numeric/string literals, `$1` and named bind markers,
quoted identifiers, SQL comments, and stable SHA-256 signatures. The central
assertion is:

```go
if Signature(literalSQL) != Signature(parameterizedSQL) {
	t.Fatalf("signatures differ: %q != %q", Canonical(literalSQL), Canonical(parameterizedSQL))
}
```

- [ ] **Step 3: Add the package implementation**

Implement a byte scanner that removes comments and semicolons, preserves
quoted identifiers and operators, lowercases unquoted words, and replaces
string/numeric/bind literals with `?`. Implement the signature as:

```go
func Signature(sql string) string {
	sum := sha256.Sum256([]byte(Canonical(sql)))
	return hex.EncodeToString(sum[:])
}
```

- [ ] **Step 4: Verify package and dependent build**

Run:

```bash
go test -mod=readonly -count=1 ./internal/sqlshape ./internal/gsbench ./cmd/gsbench
```

Expected: PASS.

- [ ] **Step 5: Commit the independently buildable source**

```bash
git add internal/sqlshape
git commit -m "fix(gsbench): restore reproducible sqlshape build dependency"
```

### Task 2: Correct TP SQL and make workload errors observable

**Files:**
- Modify: `internal/gsbench/scenario_tp.go`
- Modify: `internal/gsbench/scenario_cpu_test.go`
- Modify: `internal/gsbench/workers.go`
- Modify: `internal/gsbench/workers_test.go`
- Modify: `internal/gsbench/scenario_common.go`
- Modify: `internal/gsbench/runner.go`
- Modify: `internal/gsbench/runner_test.go`
- Modify: `internal/gsbench/model.go`

**Interfaces:**
- Extends: `WorkerSnapshot` with `FirstError string`
- Produces: `WorkerGroup.ExecutionError() error`
- Adds: `OutcomeUnverified Outcome = "UNVERIFIED"`

- [ ] **Step 1: Write failing TP SQL assertions**

Assert that all account predicates include the expected distribution key and
the order insert contains every non-null runtime column:

```go
statements := TPStatements("gsbench", 7, 11, 12.50)
if !strings.Contains(statements[0], "dist_key=8 AND id=7") { t.Fatal(statements[0]) }
if !strings.Contains(statements[2], "orders(dist_key,id,customer_id,status,amount,created_at)") { t.Fatal(statements[2]) }
```

Run `go test ./internal/gsbench -run TestTPStatements -count=1`; expect FAIL.

- [ ] **Step 2: Correct TP statements**

Derive `distKey := id + 1`, include it in account predicates, and insert it
before `orders.id`. Run the focused test; expect PASS.

- [ ] **Step 3: Write failing worker error tests**

Create a worker whose operation returns `errors.New("sentinel workload error")`.
Wait until the snapshot error count is positive and assert:

```go
if got := group.Snapshot().FirstError; !strings.Contains(got, "sentinel workload error") { t.Fatal(got) }
if err := group.ExecutionError(); err == nil { t.Fatal("execution error was swallowed") }
```

Also assert the first error is bounded and workers apply a small cancellable
backoff rather than spinning on a deterministic SQL error. Run focused tests;
expect compile/test failure because the API is absent.

- [ ] **Step 4: Capture the first real execution error**

Add a mutex-protected or `sync.Once` first-error field to `WorkerGroup`, retain
the atomic count, and expose it through `WorkerSnapshot` and
`ExecutionError()`. On error, wait on a 10 ms timer or context cancellation
before retrying.

- [ ] **Step 5: Write failing runner-result tests**

With `validation_enabled=false`, run a fake scenario that records a worker
execution error. Assert `FAILED`, not `SUCCESS`. Run a clean fake scenario and
assert `UNVERIFIED` with a zero exit code and explicit `validation_skipped`
evidence. Run the focused runner tests; expect FAIL.

- [ ] **Step 6: Enforce execution outcomes independently of validation**

After Hold/Stop, inspect any scenario implementing:

```go
type executionReporter interface {
	ExecutionSnapshot() WorkerSnapshot
}
```

Return `FAILED` whenever `Errors > 0`. When optional Verify is skipped and no
real error occurred, return `UNVERIFIED`. Add `OutcomeUnverified` at the same
exit-code rank as success and attach operations/errors/first-error evidence.

- [ ] **Step 7: Verify and commit**

Run `go test ./internal/gsbench -run 'TestTP|TestWorker|TestRunner' -count=1`,
then `go test ./internal/gsbench -count=1`. Commit:

```bash
git add internal/gsbench
git commit -m "fix(gsbench): fail runs on real workload errors"
```

### Task 3: Separate optional model validation from mandatory safety

**Files:**
- Modify: `internal/gsbench/journal.go`
- Modify: `internal/gsbench/journal_test.go`
- Modify: `internal/gsbench/restore.go`
- Modify: `internal/gsbench/restore_test.go`
- Modify: `internal/gsbench/app.go`
- Modify: `internal/gsbench/app_plan_test.go`
- Modify: `internal/gsbench/dataset.go`
- Modify: `internal/gsbench/dataset_test.go`
- Modify: `internal/gsbench/runner.go`
- Modify: `internal/gsbench/runner_test.go`

**Interfaces:**
- Keeps: `RunConfig.ValidationEnabled bool` for result-model verification only
- Removes validation booleans from safety-critical constructors

- [ ] **Step 1: Write failing journal safety tests**

Construct a journal through the default-off runtime path and assert that
`Preflight` is called before Apply, and both `Preflight` and `VerifyRestored`
are called before a restored action is marked `restored`. Run focused tests;
expect FAIL because those calls are currently gated.

- [ ] **Step 2: Make journal safety unconditional**

Remove `validationEnabled` branching from `ApplyAction` and `restoreActions`.
Keep action validation, persistence order, inverse execution, and verification
unchanged. Preserve constructor compatibility only if tests require it, but
ignore no safety check.

- [ ] **Step 3: Write failing restore and preflight tests**

Assert that topology redetection, tagged-session checks, pending-action checks,
provider checks, and restore verification run with model validation disabled.
Assert unsupported product, missing initialized schema, active plan run, and
unknown dataset version all fail before mutation. Run focused tests; expect
FAIL at each currently gated boundary.

- [ ] **Step 4: Make correctness preconditions unconditional**

Delete `ValidationEnabled` branches around product/schema/version/plan-run
preconditions and restore safety. Keep only scenario target-model calls and
dataset hard-target tolerance behind the switch.

- [ ] **Step 5: Correct phase reporting**

Only write `PhaseVerify` when model Verify runs. Always write
`PhaseVerifyRestore` because restore verification is now mandatory. Assert
phase history matches the actual calls.

- [ ] **Step 6: Verify and commit**

Run:

```bash
go test ./internal/gsbench -run 'TestJournal|TestRestore|Test.*Validation|Test.*Preflight|Test.*Phase' -count=1
go test ./internal/gsbench -count=1
```

Commit `fix(gsbench): keep safety checks enabled independently of model validation`.

### Task 4: Protect initialization state and enforce data configuration

**Files:**
- Create: `internal/gsbench/run_lock.go`
- Create: `internal/gsbench/run_lock_test.go`
- Modify: `internal/gsbench/app.go`
- Modify: `internal/gsbench/app_plan_test.go`
- Modify: `internal/gsbench/dataset.go`
- Modify: `internal/gsbench/dataset_test.go`
- Modify: `internal/gsbench/config.go`
- Modify: `internal/gsbench/config_test.go`

**Interfaces:**
- Produces: `AcquireDatabaseRunLock(ctx context.Context, db *Database, identity string) (func() error, error)`
- Enforces: `DataConfig.ReuseExisting` and `SafetyConfig.ProfileCapGB`

- [ ] **Step 1: Write failing advisory-lock tests**

Use a fake queryer to assert the exact lock lifecycle:

```sql
SELECT pg_try_advisory_lock(hashtext($1))
SELECT pg_advisory_unlock(hashtext($1))
```

The identity is `gsbench:init:<database>:<schema>`. Assert false acquisition
returns `initialization already running`. Run tests; expect missing API.

- [ ] **Step 2: Implement session-scoped run locking**

Open one pinned `sql.Conn`, acquire the advisory lock, and return an idempotent
release closure that unlocks and closes that same connection. Acquire it before
dataset inspection and defer release through all init stages.

- [ ] **Step 3: Write failing configuration-behavior tests**

Assert:

- requested bytes greater than `ProfileCapGB << 30` fail before DDL;
- `ReuseExisting=false` plus an existing `meta_dataset` returns an instruction
  to run `cleanup --data` and never emits DROP;
- unknown dataset version fails with validation enabled or disabled.

- [ ] **Step 4: Enforce cap, reuse, and version behavior**

Check profile cap in the init command after parsing `--size`. Use the object
catalog to distinguish an existing dataset before `ensureSchema`. Remove the
validation branch from unsupported dataset versions.

- [ ] **Step 5: Reject unsupported no-op safety settings**

At config validation, require `safety.restore_on_exit=true`. If
`safety.restore_original_role=true` has no provider implementation, return an
explicit unsupported-setting error instead of silently accepting it. Document
both constraints in `configs/gsbench.cfg`.

- [ ] **Step 6: Verify and commit**

Run `go test ./internal/gsbench -run 'Test.*Init|Test.*DatasetVersion|Test.*Reuse|Test.*ProfileCap|Test.*RunLock' -count=1`, then the package suite. Commit `fix(gsbench): serialize and bound dataset initialization`.

### Task 5: Retain physical sizing while speeding initialization

**Files:**
- Modify: `internal/gsbench/dataset.go`
- Modify: `internal/gsbench/dataset_test.go`
- Modify: `internal/gsbench/dataset_physical_test.go`
- Modify: `internal/gsbench/dataset_dialect.go`
- Modify: `internal/gsbench/dataset_dialect_test.go`
- Modify: `internal/gsbench/plan_dataset.go`
- Modify: `internal/gsbench/plan_dataset_test.go`
- Modify: `internal/gsbench/app.go`
- Modify: `internal/gsbench/init_reporting_test.go`

**Interfaces:**
- Adds: `TableBatch` change tracking returned by `applyDatasetBatch`
- Adds: staged DDL slices for tables, primary constraints, and secondary indexes

- [ ] **Step 1: Write failing physical-size tests for validation-off mode**

Assert the inspector is sampled initially and after committed batches even when
model validation is disabled, progress contains `size_bytes`, and loading stops
at the configured physical target policy. Separately assert overshoot-model
rejection is skipped. Run focused tests; expect FAIL.

- [ ] **Step 2: Decouple measurement from result validation**

Set `inspectPhysical` solely from provider availability. Always sample, report,
and use the target to control optional-table growth. Guard only
`enforceDatasetHardTarget` and layout-model judgment with
`validationEnabled`. Always run disk-capacity checks before a batch.

- [ ] **Step 3: Write failing staged-index and analyze tests**

For a new dataset, assert CREATE TABLE precedes data batches and CREATE INDEX
follows them. For a resume with no new rows, assert no user-table ANALYZE runs.
For changed tables, assert one start/end report and one ANALYZE per changed
table. Run focused tests; expect FAIL.

- [ ] **Step 4: Stage secondary indexes after data**

Classify parsed dataset objects into tables and indexes. Ensure tables first,
load/migrate data, then ensure/validate secondary indexes. Preserve restart
idempotence with object-existence checks.

- [ ] **Step 5: Analyze only changed tables with progress**

Track successful non-empty batch ranges in a map. Emit:

```text
dataset phase=analyze table=<name> action=start
dataset phase=analyze table=<name> action=finish elapsed=<duration>
```

Skip ANALYZE for unchanged tables. Add migration name/range/percent/elapsed
reporting around each plan-data migration batch.

- [ ] **Step 6: Remove unconditional plan-baseline work from init**

Do not call `RepairPlanBaseline` from ordinary init. Plan scenarios preflight
and restoration remain responsible for baseline readiness.

- [ ] **Step 7: Verify and commit**

Run the dataset, physical, dialect, migration, and init-reporting tests, followed
by the package suite. Commit `perf(gsbench): stage indexes and report dataset initialization progress`.

### Task 6: Fix plan compatibility, cache schema, and restoration fidelity

**Files:**
- Modify: `internal/gsbench/plan_cache.go`
- Modify: `internal/gsbench/plan_cache_test.go`
- Modify: `internal/gsbench/scenario_plan.go`
- Modify: `internal/gsbench/scenario_plan_test.go`
- Modify: `internal/gsbench/register_lock_plan.go`
- Modify: `internal/gsbench/app.go`
- Modify: `internal/gsbench/app_plan_test.go`
- Modify: `internal/gsbench/plan_definitions.go`
- Modify: `internal/gsbench/plan_definitions_test.go`
- Modify: `internal/gsbench/plan_baseline.go`
- Modify: `internal/gsbench/plan_baseline_test.go`

**Interfaces:**
- Changes: plan-cache key from scenario name to `ScenarioCode`
- Produces: `scanExplainRows(rows *sql.Rows) (string, error)`
- Produces: one shared `*PlanCoordinator` for 601-606

- [ ] **Step 1: Write failing cache-column tests**

Assert generated SQL is exactly:

```sql
DELETE FROM "Bench".meta_plan_cache WHERE signature=$1 AND scenario_code=$2
INSERT INTO "Bench".meta_plan_cache(signature,scenario_code,sql_text,plan_text) VALUES($1,$2,$3,$4)
```

and the second bind argument is an integer `ScenarioCode`. Run focused tests;
expect FAIL on current `scenario` SQL.

- [ ] **Step 2: Align cache code and legacy migration**

Change cache mutation signatures to accept `ScenarioCode`. Add idempotent
metadata migration from a legacy text `scenario` column to non-null integer
`scenario_code`, rejecting unmappable rows rather than silently assigning 0.

- [ ] **Step 3: Write failing old-Gauss EXPLAIN scan tests**

Provide test rows with one and five returned columns. Assert all text columns
are joined into a deterministic plan string without `expected 5 destination
arguments`. Run tests; expect the five-column case to fail.

- [ ] **Step 4: Decode EXPLAIN dynamically**

Call `rows.Columns()`, allocate `[]any` values and pointers for its length,
scan every row, format non-null values, and join rows with newlines. Propagate
`rows.Err()`.

- [ ] **Step 5: Write failing canonical-index tests**

For every plan index, compare initial DDL, mutation inverse, and repair SQL after
normalization. Assert full definitions include the required trailing
`dist_key,id` columns.

- [ ] **Step 6: Use one plan-index definition source**

Define a small immutable catalog containing index name, table, and ordered
columns. Generate dataset DDL, plan mutations, and repair SQL from it.

- [ ] **Step 7: Write and implement plan isolation tests**

Assert registrations 601-606 receive the same coordinator pointer. Add a fake
database lock test for identity `gsbench:plan:<database>:<schema>` and ensure a
busy lock fails before any plan mutation. Implement shared coordinator wiring
and the database lock using Task 4's lock primitive.

- [ ] **Step 8: Verify and commit**

Run all plan/cache/baseline/app-plan tests and the package suite. Commit `fix(gsbench): isolate plan changes and support legacy explain rows`.

### Task 7: Stabilize config, logs, and recovery paths

**Files:**
- Modify: `internal/gsbench/cli.go`
- Modify: `internal/gsbench/cli_test.go`
- Modify: `internal/gsbench/config.go`
- Modify: `internal/gsbench/config_test.go`
- Modify: `internal/gsbench/app.go`
- Modify: `internal/gsbench/runlog.go`
- Modify: `internal/gsbench/runlog_test.go`
- Modify: `internal/gsbench/recovery_ledger.go`
- Modify: `internal/gsbench/recovery_ledger_test.go`
- Modify: `docs/gsbench/README.md`

**Interfaces:**
- Produces: `resolveConfigPath(explicit string, executable string, cwd string) (string, error)`
- Produces: absolute config-anchored log and ledger paths

- [ ] **Step 1: Write failing path-resolution tests**

Cover explicit config, current-directory `configs/gsbench.cfg`, executable
sibling `gsbench.cfg`, and executable-parent `configs/gsbench.cfg`. Assert
explicit missing paths fail and discovered paths are absolute.

- [ ] **Step 2: Implement deterministic config discovery**

Resolve candidates in the order: explicit argument, CWD config, CWD configs,
executable directory config, executable parent configs. Return the selected
absolute clean path.

- [ ] **Step 3: Write failing state-path tests**

Load the same config from two different current directories and assert identical
absolute run-log and recovery-ledger paths rooted at the config directory.

- [ ] **Step 4: Anchor runtime state to resolved config**

Store the resolved config path/dir in `BenchConfig`. Resolve relative log and
ledger settings against that directory; never use process CWD after config
loading.

- [ ] **Step 5: Update documentation and verify**

Document package-relative discovery and recommended absolute `--config` usage.
Run CLI/config/runlog/ledger tests and the full package suite. Commit
`fix(gsbench): make config and recovery paths independent of cwd`.

### Task 8: Continuously regulate CPU, connection, and thread targets

**Files:**
- Modify: `internal/gsbench/controller.go`
- Modify: `internal/gsbench/controller_test.go`
- Modify: `internal/gsbench/scenario_common.go`
- Create: `internal/gsbench/scenario_common_test.go`
- Modify: `internal/gsbench/scenario_tp.go`
- Modify: `internal/gsbench/scenario_ap.go`
- Modify: `internal/gsbench/scenario_mixed.go`
- Modify: `internal/gsbench/scenario_connections.go`
- Modify: `internal/gsbench/scenario_threads.go`
- Modify: `internal/gsbench/scenario_cpu_test.go`
- Modify: `internal/gsbench/scenario_capacity_test.go`
- Modify: `internal/gsbench/resource_workloads.go`
- Modify: `internal/gsbench/resource_workloads_test.go`
- Modify: `configs/gsbench.cfg`

**Interfaces:**
- Adds: `Controller.RunUntil(ctx context.Context) ControlResult`
- Adds: `ControlResult.ReachableMax float64`
- Adds: target sampler/actuator implementations for 401 and 402

- [ ] **Step 1: Write failing proportional-ramp tests**

For `MinWorkers=1`, `MaxWorkers=640`, and no explicit step, assert the first
adjustment is greater than one but bounded so the whole range needs at most ten
upward adjustments. Assert small ranges still use step one.

- [ ] **Step 2: Write failing hold-regulation tests**

Feed samples that first enter the target band, then drop below it. Assert the
controller keeps running until context cancellation and increases workers again
after the drop. Assert accumulated worker errors return a non-nil control error.

- [ ] **Step 3: Implement continuous control**

Default step to:

```go
step := max(1, (maxWorkers-minWorkers+9)/10)
```

Retain consecutive in-band tracking for evidence but do not return merely
because the band was reached. Return on context completion, unrecoverable
actuator/sample error, or a confirmed capacity ceiling. Persist latest actual,
peak reachable value, workers, samples, and reached state.

- [ ] **Step 4: Use the controller through scenario hold**

Create a per-scenario control context spanning Ramp and Hold. Ramp establishes
initial pressure; Hold continues the same controller until the configured
duration. Ensure Stop cancels control before stopping workers.

- [ ] **Step 5: Make connection/thread control capacity-aware**

For 401, compute the exact target connection count after subtracting reserved
and existing sessions; top up failed/closed connections during Hold and report
the reachable maximum. For 402, sample `(actual-idle)/actual*100`, continuously
adjust active workers, and report the topology-derived maximum percentage when
640 workers cannot reach 95 percent.

- [ ] **Step 6: Correct scenario 207's advertised behavior**

Until a reliable total-memory sampler is available on every supported product,
rename its strategy/result evidence to bounded `memory_pressure_workers`, expose
configurable worker count, and remove the false percentage-target claim. Keep
the scenario implemented and executable.

- [ ] **Step 7: Verify and commit**

Run controller, CPU, capacity, connection/thread, and resource workload tests,
then the full package suite. Commit `fix(gsbench): continuously regulate configured load targets`.

### Task 9: Add the serial validation harness and scenario report schema

**Files:**
- Create: `scripts/validate-gsbench-scenarios.sh`
- Create: `docs/gsbench/FULL_SCENARIO_TEST.md`
- Modify: `docs/gsbench/README.md`

**Interfaces:**
- Consumes: fixed `gsbench`, `gstop`, dedicated configs, Linux database host
- Produces: `results.tsv`, per-scenario logs, gstop samples, status/restore logs

- [ ] **Step 1: Add a self-test mode for the harness**

Implement `--list` to print exactly the 65 codes in serial order and
`--validate-config` to reject any schema other than a name beginning with
`gsbench_e2e_`. Before live commands exist, run the script tests and observe
failure.

- [ ] **Step 2: Implement guarded initialization**

Run doctor, display the resolved database/schema, require the exact environment
acknowledgement `GSBENCH_E2E_SCHEMA=gsbench_e2e_20260801`, and execute:

```bash
gsbench init --config "$cfg" --size 20GB
```

Do not auto-confirm cleanup for any other schema.

- [ ] **Step 3: Implement serial scenario execution**

Loop over the explicit 65-code list. Use 60 seconds for 101-103, 401-402, and
404; 20 seconds for 601-606 and 801; 10 seconds otherwise. Capture run ID,
exit code, outcome, status, restore result, and artifact path. Start gstop once
before the loop and record synchronized timestamps.

- [ ] **Step 4: Implement guarded cleanup and report format**

After final restore and residual-session checks, execute cleanup only for the
acknowledged schema. Emit columns:

```text
code name target observed ceiling operations errors outcome applicability evidence_path
```

- [ ] **Step 5: Shell-check and commit**

Run `sh -n scripts/validate-gsbench-scenarios.sh`, its list/config self-tests,
and verify 65 unique codes. Commit `test(gsbench): add guarded full-scenario validation harness`.

### Task 10: Minimum full verification, release, and live matrix

**Files:**
- Modify: `docs/gsbench/FULL_SCENARIO_TEST.md`
- Create: `release/gsbench-v1.1.1-linux-arm64-20260801/`
- Create: `release/gsbench-v1.1.1-linux-arm64-20260801.tar.gz`

**Interfaces:**
- Produces: reproducible Linux ARM64 binary, checksums, installation/config guide, and final 65-scenario report

- [ ] **Step 1: Run automated verification**

```bash
go test -mod=readonly -count=1 ./...
go vet -mod=readonly ./...
go test -mod=readonly -race -count=1 ./internal/gsbench
```

Expected: all PASS with no build overlay.

- [ ] **Step 2: Build and inspect Linux ARM64 artifacts**

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o release/gsbench-v1.1.1-linux-arm64-20260801/bin/gsbench ./cmd/gsbench
file release/gsbench-v1.1.1-linux-arm64-20260801/bin/gsbench
go version -m release/gsbench-v1.1.1-linux-arm64-20260801/bin/gsbench
```

Expected: statically linked AArch64/Linux binary whose module revision matches
the final commit. Package config, guides, `BUILD_INFO.txt`, and SHA-256 files.

- [ ] **Step 3: Execute live validation when environment is available**

On the Linux database host, run the Task 9 harness with gstop observation. Do
not substitute historical July evidence. Confirm initialization physical size
is 19-21 GiB and complete all 65 serial scenario invocations.

- [ ] **Step 4: Verify restoration and cleanup**

Confirm no `gsbench/<run>/...` sessions, no pending recovery actions, and no
dedicated schema after guarded cleanup. Confirm the ordinary `gsbench` and all
business schemas were untouched.

- [ ] **Step 5: Publish a truthful report**

Populate the final table with `PASS`, `FAIL`, `NOT_APPLICABLE`, or
`NOT_TESTABLE`. If the Linux/database environment remains inaccessible, mark
the live init, load targets, and cleanup `NOT_TESTABLE` and state the exact
environment blocker; do not mark the overall live matrix passed.

## Self-Review

- Spec coverage: all actionable P0/P1/P2 findings, default-load control, 20 GiB initialization, 65 serial scenarios, gstop observation, restore, cleanup, and environment constraints map to Tasks 1-10.
- Placeholder scan: the release version is fixed to `v1.1.1`; no deferred implementation markers remain.
- Type consistency: `ScenarioCode`, `WorkerSnapshot`, `ControlResult`, `RunConfig.ValidationEnabled`, and the shared lock/controller interfaces match the current Go package vocabulary.
