# gsbench v1.1.8 Advisory Precheck and Read-Only Recovery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:executing-plans to implement this plan task-by-task and keep the
> checkbox state current.

**Goal:** Release gsbench v1.1.8 with implicit `ubtree` compatibility,
advisory-only scenario checks, no automatic optimization/recovery, and
read-only `recover`/`restore` commands that render manual recovery DDL/DML
without executing it.

**Architecture:** Separate hard request guards from read-only scenario
inspection. Carry structured warnings through `Runtime` and evidence while
keeping actual workload/mutation errors scenario-local. Remove production calls
to the execution-oriented restore coordinator. Add a read-only recovery planner
that merges live gsbench baseline inspection with v1.1.7 journal/ledger actions,
verifies current state, and renders ordered SQL or manual external actions.

**Tech Stack:** Go 1.26.5, `database/sql`, openGauss/GaussDB catalogs, existing
typed journal/ledger payloads, Go table tests, and custom database-driver fakes.

**Approved design:**
`docs/superpowers/specs/2026-08-07-gsbench-118-advisory-precheck-readonly-recovery-design.md`

## Global constraints

- Use TDD for every production behavior change: write one focused failing test,
  observe the expected failure, implement the minimum behavior, and rerun it.
- Never execute repair, optimization, stale recovery, or persistent inverse
  DDL/DML from `run`, `stop`, `recover`, or `restore`.
- Process-owned connections, goroutines, sessions, and transactions must still
  be cleaned up.
- Keep SQL/identifier safety, schema ownership, Risk-B/Risk-C authorization,
  and journal-before-persistent-mutation as hard guards.
- Treat actual SQL, connection, or provider failures as execution errors, but
  keep independent scenarios running.
- Inspect only the configured gsbench schema and recorded journal/ledger state.
- Read v1.1.7 journal/ledger state without rewriting it during plan display.
- Redact credentials and unsafe raw payloads from warnings and recovery output.
- User preference excludes independent review phases. The platform requires one
  minimum Go review and one minimum diff review after code changes; run those
  once at the end and limit them to the approved spec.

## Target file map

- Create `internal/gsbench/advisory.go` and
  `internal/gsbench/advisory_test.go` for structured warning behavior.
- Create `internal/gsbench/recovery_plan.go` and
  `internal/gsbench/recovery_plan_test.go` for read-only planning/rendering.
- Modify `internal/gsbench/model.go` and `internal/gsbench/runner.go` for the
  warning outcome, advisory lifecycle, scenario isolation, and no run-end
  restore.
- Modify `internal/gsbench/app.go` and
  `internal/gsbench/app_plan_phases.go` for report-only stale state,
  read-only restore/recover, and session-only stop/cleanup.
- Modify `internal/gsbench/config.go` and `internal/gsbench/cli.go` to remove
  policy ceilings while preserving structural parsing and Risk gates.
- Modify `internal/gsbench/sqlstore.go`, `plan_baseline.go`, and
  `plan_cache.go` for ubtree semantics and read-only inspection.
- Modify cap-aware scenarios in `scenario_connections.go`,
  `scenario_threads.go`, `scenario_workmem.go`, `scenario_memory.go`,
  `scenario_plan.go`, and `resource_workloads.go`.
- Update shipped config, CLI help, current operator documentation, and
  `scripts/build.sh` for v1.1.8.

---

### Task 1: Make implicit index methods compatible with btree and ubtree

**Files:**
- Modify: `internal/gsbench/sqlstore.go`
- Test: `internal/gsbench/sqlstore_test.go`
- Test: `internal/gsbench/plan_baseline_test.go`

- [ ] Add failing tests proving:
  - expected DDL without `USING` accepts actual `USING btree`;
  - expected DDL without `USING` accepts actual `USING ubtree`;
  - explicit `USING btree` does not accept actual `USING ubtree`; and
  - changed keys, predicates, uniqueness, or opclass remain mismatches.

- [ ] Run the red tests:

```bash
go test ./internal/gsbench -run 'TestDatasetIndexMatches(Implicit|Explicit)' -count=1
```

Expected: implicit ubtree currently fails because `datasetIndexMatches` compares
the parsed method exactly and omitted expected methods default to btree.

- [ ] Add `methodExplicit bool` to `datasetIndexShape`. Preserve whether
  `USING` appeared in the expected DDL. Use a helper with this contract:

```go
func datasetIndexMethodMatches(actual, expected datasetIndexShape) bool {
    if expected.methodExplicit {
        return actual.method == expected.method
    }
    return actual.method == "btree" || actual.method == "ubtree"
}
```

- [ ] Add a baseline catalog test where `pg_get_indexdef` returns ubtree and
  prove no drop/create recovery item is produced.

- [ ] Run and commit:

```bash
go test ./internal/gsbench -run 'TestDatasetIndex|TestVerifyPlanBaseline' -count=1
git add internal/gsbench/sqlstore.go internal/gsbench/sqlstore_test.go internal/gsbench/plan_baseline_test.go
git commit -m "fix(gsbench): accept implicit ubtree indexes"
```

---

### Task 2: Add structured advisory warnings and a zero-exit warning outcome

**Files:**
- Create: `internal/gsbench/advisory.go`
- Create: `internal/gsbench/advisory_test.go`
- Modify: `internal/gsbench/model.go`
- Modify: `internal/gsbench/runner.go`
- Modify: `internal/gsbench/evidence_test.go`

- [ ] Add failing tests for a stable line such as:

```text
PRECHECK_WARN scenario=401 name=connection_pool check=capacity object=max_connections actual=82.9% expected=90% impact=target_may_not_be_reached
```

Also test bounded values, control-character removal, credential redaction,
stable evidence fields, and deterministic warning order.

- [ ] Add a failing test that `COMPLETED_WITH_WARNINGS` has exit code zero but
  does not outrank or hide `FAILED`.

- [ ] Run red tests:

```bash
go test ./internal/gsbench -run 'TestPrecheckWarning|TestWarningOutcome' -count=1
```

- [ ] Implement `PrecheckWarning`, a concurrency-safe `AdvisoryCollector`,
  `Runtime.ReportWarning`, and `runtimeWarn`. Convert scenario warnings into
  structured evidence.

- [ ] Add `OutcomeCompletedWithWarnings` to `model.go` and
  `exitCodeForOutcome`. Preserve actual failure ranking.

- [ ] Run and commit:

```bash
go test ./internal/gsbench -run 'TestPrecheckWarning|TestWarningOutcome|TestEvidence' -count=1
git add internal/gsbench/advisory.go internal/gsbench/advisory_test.go internal/gsbench/model.go internal/gsbench/runner.go internal/gsbench/evidence_test.go
git commit -m "feat(gsbench): add advisory scenario warnings"
```

---

### Task 3: Convert configuration ceilings into deprecated advisory thresholds

**Files:**
- Modify: `internal/gsbench/config.go`
- Modify: `internal/gsbench/cli.go`
- Modify: `internal/gsbench/app.go`
- Test: `internal/gsbench/config_test.go`
- Test: `internal/gsbench/cli_test.go`

- [ ] Add failing config/CLI tests that accept:
  - concurrency above `safety.max_workers` and `safety.max_connections`;
  - fixed-worker and other scenario categories in one command;
  - `restore_on_exit=false`;
  - positive pool targets above 100 percent; and
  - profile/data/free-disk policy values outside the old safety ranges.

Keep tests that reject unsafe identifiers, duplicate scenario codes,
unparseable values, non-positive duration/workers, and missing Risk
authorization.

- [ ] Run the red tests:

```bash
go test ./internal/gsbench -run 'Test(ConfigAllowsAdvisory|CLIAllowsAdvisory)' -count=1
```

- [ ] Remove selected-worker/session accumulation errors, category-mixing
  errors, maximum percent/profile/data policy errors, and mandatory
  restore-on-exit. Keep structural dependencies such as positive workers and
  `sessions >= chain_depth + 1`.

- [ ] Add pure `deprecatedConfigWarnings(cfg)` output for
  `validation_enabled`, `restore_on_exit`, `max_workers`, `max_connections`,
  and `profile_cap_gb`. Log each warning once without raw config values that
  could contain secrets.

- [ ] Run and commit:

```bash
go test ./internal/gsbench -run 'Test(Config|CLI).*Advisory|TestDeprecatedConfig' -count=1
git add internal/gsbench/config.go internal/gsbench/config_test.go internal/gsbench/cli.go internal/gsbench/cli_test.go internal/gsbench/app.go internal/gsbench/advisory.go
git commit -m "feat(gsbench): make legacy limits advisory"
```

---

### Task 4: Make runner preflight and verification advisory

**Files:**
- Modify: `internal/gsbench/runner.go`
- Modify: `internal/gsbench/plan_cache.go`
- Test: `internal/gsbench/runner_test.go`
- Test: `internal/gsbench/plan_cache_test.go`

- [ ] Add failing two-scenario tests proving:
  - environment inapplicability warns and still constructs/prepares;
  - missing requirements warn and still attempt execution;
  - EXPLAIN/inspection failure warns and does not skip `Prepare`;
  - unmet post-run targets become `COMPLETED_WITH_WARNINGS`; and
  - one actual `Ramp`/`Hold` error remains `FAILED` while the other scenario
    completes.

- [ ] Run red tests:

```bash
go test ./internal/gsbench -run 'TestRunnerAdvisory|TestRunnerKeepsOtherScenario' -count=1
```

- [ ] Convert applicability and missing requirements to warnings. Remove
  restore-coordinator availability from preflight. Preserve
  `AuthorizeScenario` as a hard Risk gate and factory errors as actual errors.

- [ ] Split `EnsureWorkloadPlans` into a read-only `InspectWorkloadPlans` path
  that performs literal/EXPLAIN checks without writing `meta_plan_cache`.
  Preflight uses only this inspector.

- [ ] Always collect verification evidence. Convert an unmet model target,
  missing optional metric, or verification-query problem into a warning.
  Preserve `Ramp`/`Hold` execution errors, execution snapshot errors, and
  resource-cleanup errors.

- [ ] Run and commit:

```bash
go test ./internal/gsbench -run 'TestRunner|TestWorkloadPlan' -count=1
git add internal/gsbench/runner.go internal/gsbench/runner_test.go internal/gsbench/plan_cache.go internal/gsbench/plan_cache_test.go internal/gsbench/advisory.go
git commit -m "feat(gsbench): make scenario checks advisory"
```

---

### Task 5: Remove run-start stale recovery and run-end persistent restore

**Files:**
- Modify: `internal/gsbench/app.go`
- Modify: `internal/gsbench/runner.go`
- Test: `internal/gsbench/app_plan_test.go`
- Test: `internal/gsbench/runner_test.go`
- Test: `internal/gsbench/integration_test.go`

- [ ] Add failing tests proving:
  - stale actions are read and logged, but `RestoreActionGroup`,
    `RepairBaseline`, session termination, and journal state updates are never
    called before a new run;
  - Runner completion never calls `RestoreService.Restore`;
  - duration and cancellation still call each scenario's `Stop`; and
  - run status-recording failure is advisory for a non-persistent workload.

- [ ] Run red tests:

```bash
go test ./internal/gsbench -run 'Test(RunReportsStaleWithoutRestore|RunnerNeverExecutesRestore|RunMetadataBestEffort)' -count=1
```

- [ ] Replace the restore-coordinator call in `commandRunCore` with
  `ReadStaleRecoveryStatus` plus a `PRECHECK_WARN` and `gsbench restore` hint.
  Start the new run without a restore lock.

- [ ] Delete Runner's run-end restore phase. Keep `scenario.Stop`, worker joins,
  connection close, and transaction rollback. Report restore evidence as
  `manual_recovery` rather than claiming `restored`.

- [ ] Run and commit:

```bash
go test ./internal/gsbench -run 'Test(Run|Runner|Cleanup|Cancellation)' -count=1
git add internal/gsbench/app.go internal/gsbench/app_plan_test.go internal/gsbench/runner.go internal/gsbench/runner_test.go internal/gsbench/integration_test.go
git commit -m "feat(gsbench): remove automatic run recovery"
```

---

### Task 6: Remove scenario-specific artificial caps and reachability gates

**Files:**
- Modify: `internal/gsbench/scenario_connections.go`
- Modify: `internal/gsbench/scenario_threads.go`
- Modify: `internal/gsbench/scenario_workmem.go`
- Modify: `internal/gsbench/scenario_memory.go`
- Modify: `internal/gsbench/resource_workloads.go`
- Modify: `internal/gsbench/scenario_plan.go`
- Modify: `internal/gsbench/app_plan_phases.go`
- Test: `internal/gsbench/scenario_capacity_test.go`
- Test: `internal/gsbench/scenario_connections_test.go`
- Test: `internal/gsbench/scenario_workmem_test.go`
- Test: `internal/gsbench/resource_workloads_test.go`
- Test: `internal/gsbench/scenario_plan_test.go`

- [ ] Add failing table tests for:
  - 401 target at/below baseline and above 100;
  - 402 missing real metric, baseline above target, and unreachable ceiling;
  - 403 workers above the old max;
  - 404 queue sessions above old worker/connection caps;
  - 201/202 workers above the old max;
  - 204/205/207 memory pressure above the old max; and
  - 601-606 init/plan workers above both old caps.

Each case must emit a warning and reach its first actual operation. Separate
tests must retain failure when that operation itself errors.

- [ ] Run the red table:

```bash
go test ./internal/gsbench -run 'TestAdvisory(NoCap|Target|Capacity|PlanWorkers)' -count=1
```

- [ ] Make 401 budget calculation descriptive for any positive target. A target
  at/below baseline requests zero additional sessions and warns. An unreachable
  target uses physical headroom and warns. A real connection-open failure still
  fails.

- [ ] Make 402 real-metric and target-ceiling checks advisory. Use the existing
  fallback with an evidence warning. Freeze the workers reached at controller
  ceiling/deadline and continue Hold. Keep real worker/session errors failing.

- [ ] Size 403/404/201-208/plan worker groups from the positive requested target
  rather than legacy max settings. Remove direct comparisons to old caps.
  Preserve nil database, malformed work_mem, zero workers, and real session/SQL
  errors.

- [ ] Run and commit:

```bash
go test ./internal/gsbench -run 'Test(Connection|Thread|Resource|WorkMem|Plan).*' -count=1
git add internal/gsbench/scenario_connections.go internal/gsbench/scenario_threads.go internal/gsbench/scenario_workmem.go internal/gsbench/scenario_memory.go internal/gsbench/resource_workloads.go internal/gsbench/scenario_plan.go internal/gsbench/app_plan_phases.go internal/gsbench/scenario_capacity_test.go internal/gsbench/scenario_connections_test.go internal/gsbench/scenario_workmem_test.go internal/gsbench/resource_workloads_test.go internal/gsbench/scenario_plan_test.go
git commit -m "feat(gsbench): remove scenario pressure gates"
```

---

### Task 7: Replace plan baseline repair and rejected-fault rollback with findings

**Files:**
- Modify: `internal/gsbench/plan_baseline.go`
- Modify: `internal/gsbench/app_plan_phases.go`
- Modify: `internal/gsbench/plan_control.go`
- Test: `internal/gsbench/plan_baseline_test.go`
- Test: `internal/gsbench/app_plan_phases_test.go`
- Test: `internal/gsbench/plan_control_test.go`

- [ ] Add failing tests proving:
  - plan init inspects and warns but never repairs;
  - a successful fault mutation whose plan effect differs is retained with a
    warning and never rolled back;
  - a partially failed fault remains journaled and never calls `RestoreFault`;
    and
  - messages point to `gsbench run NNN recover` for display-only recovery SQL.

- [ ] Run red tests:

```bash
go test ./internal/gsbench -run 'TestPlan(InitAdvisory|FaultNeverRollsBack|FaultMismatchWarns)' -count=1
```

- [ ] Add read-only `InspectPlanBaseline` returning typed findings and canonical
  suggested DDL. Keep `PlanBaselineRepairSteps` only as a statement-definition
  source for the later planner. Remove production `RepairPlanBaseline` calls.

- [ ] Replace `rejectPlanFault` with failure recording that never invokes
  `RestoreFault`. Postcondition mismatches are advisory; forward action errors
  fail with journal state preserved.

- [ ] Run and commit:

```bash
go test ./internal/gsbench -run 'TestPlan|TestExecutePlanFault' -count=1
git add internal/gsbench/plan_baseline.go internal/gsbench/plan_baseline_test.go internal/gsbench/app_plan_phases.go internal/gsbench/app_plan_phases_test.go internal/gsbench/plan_control.go internal/gsbench/plan_control_test.go
git commit -m "feat(gsbench): make plan faults manually recoverable"
```

---

### Task 8: Build the typed read-only recovery planner and renderer

**Files:**
- Create: `internal/gsbench/recovery_plan.go`
- Create: `internal/gsbench/recovery_plan_test.go`
- Modify: `internal/gsbench/action.go`
- Modify: `internal/gsbench/sqlstore.go`
- Reuse read-only merge/order helpers from: `internal/gsbench/restore.go`

- [ ] Add failing planner tests covering:
  - one complete SQL inverse with a trailing semicolon;
  - ordered `session_sql` statements;
  - safely scanned v1.1.7 legacy SQL;
  - verify success as `ALREADY_RESTORED` with no SQL;
  - no verifier as `UNVERIFIED` with visible SQL;
  - semantic deduplication;
  - descending inverse order within a run;
  - structural DDL before dependent ANALYZE;
  - conflicts remaining visible;
  - external actions as `MANUAL_ACTION` without provider calls; and
  - zero Exec, journal SetState, ledger write, session-stop, or provider
    mutation calls.

- [ ] Run red tests:

```bash
go test ./internal/gsbench -run 'TestRecoveryPlan' -count=1
```

- [ ] Implement explicit plan types:

```go
type RecoveryPlanState string

const (
    RecoveryPending         RecoveryPlanState = "PENDING"
    RecoveryAlreadyRestored RecoveryPlanState = "ALREADY_RESTORED"
    RecoveryUnverified      RecoveryPlanState = "UNVERIFIED"
    RecoveryConflict        RecoveryPlanState = "CONFLICT"
)

type RecoveryPlanItem struct {
    RunID       string
    ScenarioCode ScenarioCode
    Kind        ActionKind
    Target      string
    State       RecoveryPlanState
    Statements  []string
    ManualAction string
    Detail      string
}
```

The planner interface exposes discovery, verification, and baseline inspection
only. It must not expose Exec or state mutation methods.

- [ ] Reuse `decodeSQLActionPayload`, `legacySQLStatements`,
  `restoreActionOrder`, canonical JSON, and action identity helpers. Normalize
  one trailing semicolon without changing quoted SQL. Reject credential
  material and unresolved bind placeholders unless typed values can be encoded.

- [ ] Run and commit:

```bash
go test ./internal/gsbench -run 'TestRecoveryPlan|TestLegacySQL' -count=1
git add internal/gsbench/recovery_plan.go internal/gsbench/recovery_plan_test.go internal/gsbench/action.go internal/gsbench/sqlstore.go
git commit -m "feat(gsbench): add read-only recovery planner"
```

---

### Task 9: Add live gsbench baseline scanning to the recovery plan

**Files:**
- Modify: `internal/gsbench/recovery_plan.go`
- Modify: `internal/gsbench/plan_baseline.go`
- Modify: `internal/gsbench/dataset.go`
- Modify: `internal/gsbench/sqlstore.go`
- Test: `internal/gsbench/recovery_plan_test.go`
- Test: `internal/gsbench/dataset_test.go`
- Test: `internal/gsbench/plan_baseline_test.go`

- [ ] Add failing catalog tests for a missing index, a wrong index shape, a
  valid UStore ubtree index, incorrect statistics options, missing extended
  statistics, pending data-baseline DML, and unrelated business-schema objects.

- [ ] Assert only configured gsbench objects generate items; ubtree generates
  none; journal original-state actions outrank generic canonical suggestions;
  and all rendered items are complete.

- [ ] Run red tests:

```bash
go test ./internal/gsbench -run 'TestRecoveryPlanBaseline' -count=1
```

- [ ] Expose findings from existing dataset and plan catalog queries without
  calling migration, repair, ANALYZE, or DDL execution. Map findings to
  canonical statements. Do not invent data-restoration DML for unjournaled
  business values.

- [ ] Merge canonical and journal items by target and desired semantic state.
  Keep conflicts explicit.

- [ ] Run and commit:

```bash
go test ./internal/gsbench -run 'TestRecoveryPlanBaseline|TestDatasetIndex|TestPlanBaseline' -count=1
git add internal/gsbench/recovery_plan.go internal/gsbench/recovery_plan_test.go internal/gsbench/plan_baseline.go internal/gsbench/plan_baseline_test.go internal/gsbench/dataset.go internal/gsbench/dataset_test.go internal/gsbench/sqlstore.go
git commit -m "feat(gsbench): scan live recovery baseline"
```

---

### Task 10: Wire read-only restore/recover and session-only stop/cleanup

**Files:**
- Modify: `internal/gsbench/app.go`
- Modify: `internal/gsbench/app_plan_phases.go`
- Modify: `internal/gsbench/cli.go`
- Test: `internal/gsbench/cli_test.go`
- Test: `internal/gsbench/app_plan_test.go`
- Test: `internal/gsbench/status_test.go`
- Test: `internal/gsbench/restore_test.go`

- [ ] Add failing command tests proving:
  - `restore` and `restore --dry-run` have identical read-only output;
  - each 601-606 `recover` contains only its scenario;
  - `RESTORE_PLAN_EMPTY`, `ALREADY_RESTORED`, `UNVERIFIED`, and
    `MANUAL_ACTION` render correctly;
  - `stop` performs only stop request, tagged cancellation/termination, and
    quiescence inspection; and
  - plain `cleanup` is session-only while `cleanup --data` retains existing
    ownership/target guards.

- [ ] Run red tests:

```bash
go test ./internal/gsbench -run 'Test(CommandRestoreReadOnly|PlanRecoverReadOnly|CommandStopSessionsOnly|CleanupSessionsOnly)' -count=1
```

- [ ] Make `commandRestore` build/render the all-scenario plan. `--run-id`
  filters journal/ledger records. Pending work is not an error.

- [ ] Route 601-606 `recover` to the same planner with a scenario filter.
  Remove its production call to `executePlanRecoverAction` and print a v1.1.8
  display-only banner.

- [ ] Route `stop` and plain `cleanup` directly to safe stop request and tagged
  session cleanup. Never construct the recovery executor. Extend `status` with
  pending counts and restore/recover hints.

- [ ] Run and commit:

```bash
go test ./internal/gsbench -run 'Test(Command|PlanRecover|Status|Cleanup|Restore)' -count=1
git add internal/gsbench/app.go internal/gsbench/app_plan_phases.go internal/gsbench/cli.go internal/gsbench/cli_test.go internal/gsbench/app_plan_test.go internal/gsbench/status_test.go internal/gsbench/restore_test.go
git commit -m "feat(gsbench): make recovery commands read-only"
```

---

### Task 11: Update version, shipped config, help, and operator documentation

**Files:**
- Modify: `internal/gsbench/cli.go`
- Modify: `internal/gsbench/cli_test.go`
- Modify: `internal/gsbench/banner_test.go`
- Modify: `scripts/build.sh`
- Modify: `configs/gsbench.cfg`
- Modify: `docs/gsbench/README.md`
- Modify: `docs/gsbench/CONFIG.md`
- Modify: `docs/gsbench/INSTALL.md`
- Modify: `docs/gsbench/PLAN_601_606_CN.md`
- Modify: `README.md`

- [ ] Change the expected public banner to `gsbench v1.1.8` and add failing
  help assertions describing restore/recover as display-only.

- [ ] Run red version/help tests:

```bash
go test ./internal/gsbench -run 'Test(CLI|Banner).*Version|TestCLIHelp' -count=1
```

- [ ] Set `Version = "v1.1.8"` and update only the gsbench line in
  `scripts/build.sh`. Do not change the independent gstop version.

- [ ] Rewrite current docs to cover advisory prechecks, retained Risk gates,
  deprecated non-enforcing keys, ubtree compatibility, session-only stop, and
  this manual cycle:

```text
gsbench restore
# operator reviews and executes selected SQL
gsbench restore
# expect RESTORE_PLAN_EMPTY or ALREADY_RESTORED
```

Remove current claims of automatic stale/run-end recovery. Historical v1.1.7
reports may remain historical.

- [ ] Run consistency checks and commit:

```bash
go test ./internal/gsbench -run 'Test(CLI|Banner|Install|Config)' -count=1
rg -n 'v1\.1\.7|automatically restore|自动恢复|执行恢复' internal/gsbench scripts/build.sh configs/gsbench.cfg docs/gsbench README.md
git add internal/gsbench/cli.go internal/gsbench/cli_test.go internal/gsbench/banner_test.go scripts/build.sh configs/gsbench.cfg docs/gsbench/README.md docs/gsbench/CONFIG.md docs/gsbench/INSTALL.md docs/gsbench/PLAN_601_606_CN.md README.md
git commit -m "docs(gsbench): publish version 1.1.8 behavior"
```

---

### Task 12: Verify integrated v1.1.8 behavior

- [ ] Run focused behavior groups:

```bash
go test ./internal/gsbench -run 'TestDatasetIndex|TestPrecheck|TestRunner|TestRecoveryPlan|TestPlan|TestCommandRestore|TestCommandStop' -count=1
go test ./internal/gsbench -run 'Test(Connection|Thread|Resource|WorkMem|Memory)' -count=1
```

- [ ] Run the complete repository suite and vet:

```bash
go test ./... -count=1
go vet ./...
```

- [ ] Run focused race coverage:

```bash
go test -race ./internal/gsbench -run 'Test(Runner|RecoveryPlan|FileRecoveryLedger|Concurrent)' -count=1
```

- [ ] Build and smoke-test a temporary local binary:

```bash
mkdir -p /tmp/gsbench-v118-build
go build -trimpath -o /tmp/gsbench-v118-build/gsbench ./cmd/gsbench
/tmp/gsbench-v118-build/gsbench version
/tmp/gsbench-v118-build/gsbench help
```

Expected version: `gsbench v1.1.8`.

- [ ] If the configured local test database is available, run display-only
  smoke commands against the dedicated gsbench schema and compare journal rows
  plus catalog fingerprints before and after:

```bash
/tmp/gsbench-v118-build/gsbench run 601 recover --config /Users/sqlrush/gstop/gsbench-local/gsbench.cfg
/tmp/gsbench-v118-build/gsbench restore --config /Users/sqlrush/gstop/gsbench-local/gsbench.cfg
```

Do not execute any displayed recovery SQL automatically. Also run one short
multi-scenario command with an intentionally unreachable advisory target and
prove another selected scenario continues.

- [ ] Perform the platform-mandated minimum Go review and minimum overall diff
  review once. Fix only material findings and rerun the smallest affected tests.

- [ ] Check final source state:

```bash
git diff --check
git status --short --branch
git log --oneline -12
```

Do not deploy into `/Users/sqlrush/gstop/gsbench-local`, publish an archive, or
push GitHub unless the user explicitly requests those external delivery actions
after source verification.
