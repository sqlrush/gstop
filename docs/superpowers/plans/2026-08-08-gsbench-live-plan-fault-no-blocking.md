# gsbench Live Plan Fault No-Blocking Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make 601/602 live catalog state advisory-only, remove all persistent plan-fault admission gates, allow repeated same-scenario and cross-scenario faults, and keep journal-before-mutation recovery evidence.

**Architecture:** Add a narrow catalog inspector for 601/602, call it inside the existing short operation lock, and report its result without returning an admission error. Convert `meta_runs` to best-effort terminal audit data, retain `meta_journal` as the mandatory write-before-mutation record, and reconcile scenario-scoped recovery output against live structure rather than journal state.

**Tech Stack:** Go 1.26.5, `database/sql`, openGauss catalog views, existing gsbench journal/recovery planner, Go table tests.

## Global Constraints

- CLI syntax and the public version string remain `gsbench v1.1.8`.
- `meta_journal` remains mandatory before persistent DDL/DML and is used for audit provenance and inverse SQL.
- No `meta_runs` or `meta_journal` value may block a 601–606 fault.
- No 601/602 live-state value may block a fault; it only produces state evidence and, where applicable, `PRECHECK_WARN`.
- Unsafe schema/identifier input, missing live init workload, journal persistence failure, connection failure, and actual SQL failure remain hard errors.
- Recover and restore remain display-only and must not execute or persist recovery state.
- Only the configured gsbench schema and canonical plan-test objects may be inspected.
- Tests must be written and observed failing before production code for each behavior.
- User preference excludes independent per-task review loops; run only the minimum platform-mandated Go and general code review once after implementation.

---

### Task 1: Add the 601/602 live catalog inspector

**Files:**
- Create: `internal/gsbench/plan_fault_state.go`
- Create: `internal/gsbench/plan_fault_state_test.go`
- Reuse: `internal/gsbench/plan_dataset.go`
- Reuse: `internal/gsbench/sqlstore.go`

**Interfaces:**
- Consumes: `planIndexDefinitionByName(string)`, `planIndexDDL(string, planIndexDefinition, bool)`, `planIndexDefinitionQuery`, and `datasetIndexMatches(string, string)`.
- Produces:

```go
type PlanFaultLiveState string

const (
    PlanFaultRestored    PlanFaultLiveState = "RESTORED"
    PlanFaultPresent     PlanFaultLiveState = "FAULT_PRESENT"
    PlanFaultDrifted     PlanFaultLiveState = "DRIFTED"
    PlanFaultUnavailable PlanFaultLiveState = "UNAVAILABLE"
)

type PlanFaultInspection struct {
    Code   ScenarioCode
    State  PlanFaultLiveState
    Object string
    Detail string
}

type planFaultCatalog interface {
    ScanReadOnly(context.Context, string, []any, ...any) error
}

func InspectPlanFaultState(
    context.Context,
    planFaultCatalog,
    string,
    ScenarioCode,
) (PlanFaultInspection, error)
```

- `InspectPlanFaultState` returns an error only for unsupported codes, unsafe identifiers, or invalid canonical definitions. Catalog read errors become `PlanFaultUnavailable` with sanitized detail.

- [ ] **Step 0: Verify the clean baseline**

```bash
go mod download
go test ./... -count=1
```

Expected: dependency resolution succeeds and every package test passes before feature edits.

- [ ] **Step 1: Write the failing state-matrix tests**

Create a focused fake that records read-only SQL and returns configured index definitions, usability counts, or `lookup_key.attoptions`. Add tests equivalent to:

```go
func TestInspectPlanFaultState601UsesOnlyLiveIndexStructure(t *testing.T) {
    tests := []struct {
        name          string
        definition    string
        definitionErr error
        usable        int
        want          PlanFaultLiveState
    }{
        {name: "canonical", definition: canonicalLookupIndexDDL(t), usable: 1, want: PlanFaultRestored},
        {name: "missing", definitionErr: sql.ErrNoRows, want: PlanFaultPresent},
        {name: "wrong shape", definition: `CREATE INDEX plan_data_lookup_idx ON gsbench.plan_data(dist_key)`, usable: 1, want: PlanFaultDrifted},
        {name: "unusable", definition: canonicalLookupIndexDDL(t), usable: 0, want: PlanFaultDrifted},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            db := &planFaultCatalogFake{
                definition: tt.definition, definitionErr: tt.definitionErr,
                usable: tt.usable,
            }
            got, err := InspectPlanFaultState(context.Background(), db, "gsbench", 601)
            if err != nil || got.State != tt.want {
                t.Fatalf("inspection=%+v error=%v want=%s", got, err, tt.want)
            }
            assertPlanFaultQueriesAreCatalogOnly(t, db.queries)
        })
    }
}

func TestInspectPlanFaultState602UsesOnlyLookupColumnOptions(t *testing.T) {
    tests := []struct {
        options string
        want    PlanFaultLiveState
    }{
        {options: "", want: PlanFaultRestored},
        {options: "n_distinct=1", want: PlanFaultPresent},
        {options: "n_distinct=0.25", want: PlanFaultDrifted},
    }
    for _, tt := range tests {
        db := &planFaultCatalogFake{options: tt.options}
        got, err := InspectPlanFaultState(context.Background(), db, "gsbench", 602)
        if err != nil || got.State != tt.want {
            t.Fatalf("options=%q inspection=%+v error=%v want=%s", tt.options, got, err, tt.want)
        }
        assertPlanFaultQueriesAreCatalogOnly(t, db.queries)
    }
    unavailable, err := InspectPlanFaultState(
        context.Background(), &planFaultCatalogFake{scanErr: errors.New("catalog unavailable")},
        "gsbench", 602,
    )
    if err != nil || unavailable.State != PlanFaultUnavailable {
        t.Fatalf("inspection=%+v error=%v", unavailable, err)
    }
}
```

- [ ] **Step 2: Run the tests and verify RED**

```bash
go test ./internal/gsbench -run 'TestInspectPlanFaultState' -count=1
```

Expected: build failure because the live-state types and inspector do not exist.

- [ ] **Step 3: Implement the minimal inspector**

Implement 601 using the canonical definition and:

```go
// Scan planIndexDefinitionQuery with []any{schema, definition.Name}.
// sql.ErrNoRows => FAULT_PRESENT.
// !datasetIndexMatches(actual, expected) => DRIFTED.
// Then require indisusable, indisready, and indisvalid count == 1.
```

Implement 602 with one read-only query:

```sql
SELECT COALESCE(array_to_string(attoptions,','),'')
FROM pg_attribute
WHERE attrelid='"<schema>".plan_data'::regclass
  AND attname='lookup_key'
```

Classify an empty/no-`n_distinct` value as restored, exact normalized `n_distinct=1` as fault present, and any other `n_distinct` value as drifted.

- [ ] **Step 4: Run focused tests and verify GREEN**

```bash
gofmt -w internal/gsbench/plan_fault_state.go internal/gsbench/plan_fault_state_test.go
go test ./internal/gsbench -run 'TestInspectPlanFaultState' -count=1
```

Expected: PASS with no write SQL recorded.

- [ ] **Step 5: Commit the inspector**

```bash
git add internal/gsbench/plan_fault_state.go internal/gsbench/plan_fault_state_test.go
git commit -m "feat(gsbench): inspect plan fault state from live catalog"
```

---

### Task 2: Remove state gates and make repeated faults advisory and auditable

**Files:**
- Modify: `internal/gsbench/app_plan_phases.go`
- Modify: `internal/gsbench/app_plan_phases_test.go`
- Modify: `internal/gsbench/plan_control.go`
- Modify: `internal/gsbench/plan_control_test.go`
- Modify: `internal/gsbench/plan_definitions.go`
- Modify: `internal/gsbench/plan_definitions_test.go`

**Interfaces:**
- Consumes: `InspectPlanFaultState` from Task 1 and the existing short plan database lock.
- Replaces `ResolveFault`, `MarkFaultActive`, and `MarkFaultFailed` with:

```go
type planFaultReporters struct {
    State   func(PlanFaultInspection)
    Warning func(PrecheckWarning)
}

type planActionBackend interface {
    Lock(context.Context) (func() error, error)
    ResolveWorkload(context.Context, ScenarioCode) (planRunRecord, error)
    WorkloadAlive(context.Context) (bool, error)
    InspectFaultState(context.Context, ScenarioCode) (PlanFaultInspection, error)
    RecordFaultStart(context.Context, string, ScenarioCode) error
    ApplyFault(context.Context, string, ScenarioCode) error
    VerifyFault(context.Context, ScenarioCode) error
    RecordFaultFinish(context.Context, string, Outcome, string) error
}
```

- Audit calls are best-effort. Their errors are warnings and never stop journal/DDL execution. `ApplyFault` remains the hard journal-before-mutation boundary.

- [ ] **Step 1: Write failing orchestration and audit tests**

Update `planActionBackendTest` to record `inspect-state`, `record-start`, and `record-finish`. Add tests equivalent to:

```go
func TestExecutePlanFaultActionAlwaysContinuesAfterLiveStateWarning(t *testing.T) {
    for _, state := range []PlanFaultLiveState{
        PlanFaultRestored, PlanFaultPresent, PlanFaultDrifted, PlanFaultUnavailable,
    } {
        backend := newLivePlanFaultBackend(601, state)
        _, err := executePlanFaultAction(
            context.Background(), 601, backend,
            func() string { return "fault-1" },
            recordingPlanFaultReporters(),
        )
        if err != nil { t.Fatalf("state=%s error=%v", state, err) }
        assertEventsContainInOrder(t, backend.events,
            "inspect-state:601", "record-start:fault-1",
            "apply-fault:fault-1", "verify-fault:601",
            "record-finish:fault-1")
    }
}

func TestExecutePlanFaultActionIgnoresAuditWriteFailures(t *testing.T) {
    backend := newLivePlanFaultBackend(601, PlanFaultRestored)
    backend.recordStartErr = errors.New("audit start failed")
    backend.recordFinishErr = errors.New("audit finish failed")
    reporters := recordingPlanFaultReporters()
    _, err := executePlanFaultAction(
        context.Background(), 601, backend,
        func() string { return "fault-1" }, reporters,
    )
    if err != nil || !containsEventPrefix(backend.events, "apply-fault:fault-1") {
        t.Fatalf("events=%v error=%v", backend.events, err)
    }
    if reporters.WarningCount() != 2 {
        t.Fatalf("warnings=%v", reporters.Warnings())
    }
}

func TestExecutePlanFaultActionFailsOnlyOnRealApplyError(t *testing.T) {
    backend := newLivePlanFaultBackend(601, PlanFaultPresent)
    backend.applyErr = errors.New("DROP INDEX failed")
    backend.recordFinishErr = errors.New("audit finish failed")
    reporters := recordingPlanFaultReporters()
    _, err := executePlanFaultAction(
        context.Background(), 601, backend,
        func() string { return "fault-1" }, reporters,
    )
    if err == nil || !strings.Contains(err.Error(), "DROP INDEX failed") {
        t.Fatalf("error=%v", err)
    }
    if reporters.WarningCount() == 0 {
        t.Fatal("terminal audit failure was not warned")
    }
}
```

Remove every expected `resolve-fault` event. Add a store test proving `RecordFaultFinish` writes phase, terminal status, detail, and run ID. Add a mutation test requiring:

```sql
DROP INDEX IF EXISTS "gsbench".plan_data_lookup_idx
```

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./internal/gsbench -run 'TestExecutePlanFaultAction|TestPlanControl.*Fault|TestPlanMutation601' -count=1
```

Expected: failures because persistent gates and hard audit errors remain, and 601 lacks `IF EXISTS`.

- [ ] **Step 3: Implement warn-only state and best-effort audit**

1. Remove `ResolveFault` from the backend and orchestration.
2. Make `RecordFaultStart` call only `planControlStore.StartFault`; delete the 601–606 scan.
3. Inspect only 601/602. Always emit `PLAN_FAULT_STATE ... action=continue`; warn for `FAULT_PRESENT`, `DRIFTED`, `UNAVAILABLE`, or inspection error.
4. Warn and continue when `RecordFaultStart` fails.
5. Keep `ApplyFault` as the hard operation.
6. Write `SUCCESS` or `COMPLETED_WITH_WARNINGS` on success; best-effort write `FAILED` and return only the SQL error on failure.
7. Add `planControlStore.RecordFaultFinish` with:

```sql
UPDATE "<schema>".meta_runs
SET phase=$1,status=$2,detail=$3,updated_at=current_timestamp
WHERE run_id=$4
```

8. Delete `ResolveFault` and `errPlanFaultNotFound` after removing all callers.
9. Change 601 forward SQL to `DROP INDEX IF EXISTS`.
10. Wrap fault lock acquisition with `acquirePlanFinalizationLock` and `planMaintenanceContext` so transient busy state waits rather than refuses.

- [ ] **Step 4: Run focused/package tests and verify GREEN**

```bash
gofmt -w internal/gsbench/app_plan_phases.go internal/gsbench/app_plan_phases_test.go internal/gsbench/plan_control.go internal/gsbench/plan_control_test.go internal/gsbench/plan_definitions.go internal/gsbench/plan_definitions_test.go
go test ./internal/gsbench -run 'TestExecutePlanFaultAction|TestPlanControl.*Fault|TestPlanMutation601' -count=1
go test ./internal/gsbench -count=1
```

Expected: PASS; workload liveness, journal-before-DDL, and 602 effect warnings remain covered.

- [ ] **Step 5: Commit the nonblocking flow**

```bash
git add internal/gsbench/app_plan_phases.go internal/gsbench/app_plan_phases_test.go internal/gsbench/plan_control.go internal/gsbench/plan_control_test.go internal/gsbench/plan_definitions.go internal/gsbench/plan_definitions_test.go
git commit -m "fix(gsbench): make plan fault state advisory only"
```

---

### Task 3: Make 601/602 recovery authoritative from live structure

**Files:**
- Modify: `internal/gsbench/recovery_plan.go`
- Modify: `internal/gsbench/recovery_plan_test.go`
- Modify: `internal/gsbench/app.go`
- Modify: `internal/gsbench/app_plan_test.go`

**Interfaces:**
- Consumes: `PlanFaultInspection` from Task 1 and `planIndexDDL`.
- Produces:

```go
func ReconcilePlanRecoveryWithLiveState(
    RecoveryPlan,
    PlanFaultInspection,
    string,
) (RecoveryPlan, error)

func planFaultRecoveryStatements(
    string,
    PlanFaultInspection,
) ([]string, error)

func planRecoveryDiscoveryCanUseLiveState(
    ScenarioCode,
    PlanFaultInspection,
) bool
```

- The reconciler replaces only items for `inspection.Code` and preserves every other scenario.

- [ ] **Step 1: Write failing live-authority recovery tests**

```go
func TestReconcile601RecoveryUsesOnlyLiveIndexState(t *testing.T) {
    source := RecoveryPlan{Items: []RecoveryPlanItem{{
        RunID: "old-601", ScenarioCode: 601, State: RecoveryPending,
        Statements: []string{"recorded inverse"},
    }}}
    restored, err := ReconcilePlanRecoveryWithLiveState(source,
        PlanFaultInspection{Code: 601, State: PlanFaultRestored}, "gsbench")
    if err != nil || len(restored.Items) != 1 ||
        restored.Items[0].State != RecoveryAlreadyRestored ||
        len(restored.Items[0].Statements) != 0 {
        t.Fatalf("plan=%+v error=%v", restored, err)
    }
    drifted, err := ReconcilePlanRecoveryWithLiveState(source,
        PlanFaultInspection{Code: 601, State: PlanFaultDrifted}, "gsbench")
    if err != nil || len(drifted.Items) != 1 || len(drifted.Items[0].Statements) != 2 ||
        !strings.HasPrefix(drifted.Items[0].Statements[0], "DROP INDEX IF EXISTS") ||
        !strings.HasPrefix(drifted.Items[0].Statements[1], "CREATE UNIQUE INDEX") {
        t.Fatalf("plan=%+v error=%v", drifted, err)
    }
}

func TestReconcile602RecoveryKeepsResetAndAnalyzeTogether(t *testing.T) {
    plan := RecoveryPlan{Items: []RecoveryPlanItem{
        {
            RunID: "old-602", ScenarioCode: 602, State: RecoveryPending,
            Target: `"gsbench".plan_data.lookup_key`,
            Statements: []string{`ALTER TABLE "gsbench".plan_data ALTER COLUMN lookup_key RESET (n_distinct)`},
        },
        {
            RunID: "old-602", ScenarioCode: 602, State: RecoveryAlreadyRestored,
            Target: `"gsbench".plan_data.lookup_key analyze`,
        },
    }}
    got, err := ReconcilePlanRecoveryWithLiveState(plan, PlanFaultInspection{
        Code: 602, State: PlanFaultPresent,
    }, "gsbench")
    if err != nil || len(got.Items) != 1 || len(got.Items[0].Statements) != 2 ||
        !strings.Contains(got.Items[0].Statements[0], "RESET (n_distinct)") ||
        !strings.HasPrefix(got.Items[0].Statements[1], "ANALYZE") {
        t.Fatalf("plan=%+v error=%v", got, err)
    }
}

func TestScenarioRecoverySurvivesUnavailablePlanAuditDiscovery(t *testing.T) {
    for _, code := range []ScenarioCode{601, 602} {
        inspection := PlanFaultInspection{Code: code, State: PlanFaultPresent}
        if !planRecoveryDiscoveryCanUseLiveState(code, inspection) {
            t.Fatalf("scenario %03d could not fall back to live state", code)
        }
    }
    inspection := PlanFaultInspection{Code: 603, State: PlanFaultUnavailable}
    if planRecoveryDiscoveryCanUseLiveState(603, inspection) {
        t.Fatal("scenario 603 incorrectly ignored audit discovery failure")
    }
}
```

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./internal/gsbench -run 'TestReconcile60[12]Recovery|TestScenarioRecoverySurvivesUnavailablePlanAuditDiscovery' -count=1
```

Expected: build failure because reconciliation does not exist; existing 602 logic also hides `ANALYZE` behind its constant verifier.

- [ ] **Step 3: Implement scenario-level reconciliation**

Use these exact rules:

```text
601 RESTORED      -> ALREADY_RESTORED, no SQL
601 FAULT_PRESENT -> canonical CREATE UNIQUE INDEX
601 DRIFTED       -> DROP INDEX IF EXISTS, canonical CREATE UNIQUE INDEX
602 RESTORED      -> ALREADY_RESTORED, no SQL
602 FAULT_PRESENT -> RESET (n_distinct), ANALYZE
602 DRIFTED       -> RESET (n_distinct), ANALYZE
UNAVAILABLE       -> preserve journal-derived UNVERIFIED/PENDING items
```

Use an existing journal run ID when available; otherwise use `baseline`. Do not mutate journal state.

In `commandRecoveryPlan`, inspect 601/602 live state and reconcile after `BuildRecoveryPlan` and baseline merging. For scenario-scoped 601/602 recovery, allow journal discovery failure to fall back to empty discovery plus canonical live recovery and log:

```text
RECOVERY_AUDIT authority=audit_only unavailable=true
```

For all-scenario restore, retain discovery errors needed to protect unrelated scenarios.

- [ ] **Step 4: Run focused/package tests and verify GREEN**

```bash
gofmt -w internal/gsbench/recovery_plan.go internal/gsbench/recovery_plan_test.go internal/gsbench/app.go internal/gsbench/app_plan_test.go
go test ./internal/gsbench -run 'TestReconcile60[12]Recovery|TestScenarioRecoverySurvivesUnavailablePlanAuditDiscovery|TestRecovery' -count=1
go test ./internal/gsbench -count=1
```

Expected: PASS; 602 renders `RESET + ANALYZE` while the override exists, and no rendered SQL executes.

- [ ] **Step 5: Commit live-authority recovery**

```bash
git add internal/gsbench/recovery_plan.go internal/gsbench/recovery_plan_test.go internal/gsbench/app.go internal/gsbench/app_plan_test.go
git commit -m "fix(gsbench): derive plan recovery from live structure"
```

---

### Task 4: Remove metadata-active messaging and document audit-only semantics

**Files:**
- Modify: `internal/gsbench/app.go`
- Modify: `internal/gsbench/app_plan_phases.go`
- Modify: `internal/gsbench/app_plan_test.go`
- Modify: `internal/gsbench/app_plan_phases_test.go`
- Modify: `internal/gsbench/status.go`
- Modify: `internal/gsbench/status_test.go`
- Modify: `docs/gsbench/PLAN_601_606_CN.md`
- Modify: `docs/gsbench/README.md`

**Interfaces:**
- Consumes: `InspectPlanFaultState` and `PlanFaultInspection`.
- Removes: `findActivePlanRun`, `storedScenarioCodesContainPlanChange`, and all `ResolveFault`-based init warnings.

- [ ] **Step 1: Write failing status/precheck wording tests**

Assert output contains:

```text
PLAN_FAULT_STATE scenario=601 state=RESTORED source=live_catalog action=continue
PLAN_FAULT_STATE scenario=602 state=FAULT_PRESENT source=live_catalog action=continue
RECOVERY_AUDIT database_records=N authority=audit_only
```

Assert plan-fault output does not contain:

```text
plan fault ... remains active
recorded_active
stale recovery
pending recovery
```

Add a plan-init test with an old `meta_runs status=running` fault row and prove no `ResolveFault` query or active warning occurs.

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./internal/gsbench -run 'Test.*(Status|Audit|ActivePlan|PlanInit).*' -count=1
```

Expected: failures because current output still uses `stale recovery`, `recorded_active`, and `remains active`.

- [ ] **Step 3: Implement audit-only logging and docs**

1. Remove the `runPlanInit` loop that resolves active fault rows.
2. Remove the `commandRunCore` `findActivePlanRun` warning and delete its helpers.
3. Relabel journal/ledger summaries as `RECOVERY_AUDIT` and `audit_run_id`.
4. In `commandStatus`, print 601 and 602 live-state lines.
5. Preserve raw historical `meta_runs` rows without interpreting `running` as an active fault.
6. Add the following rule to both operator documents:

```text
601/602 的恢复状态只看实时对象结构；meta_runs/meta_journal 仅作审计和恢复 SQL 来源。
同场景和不同场景的历史/实时 fault 状态都不会拒绝新的 fault；状态异常只告警并继续。
```

- [ ] **Step 4: Run focused tests and verify GREEN**

```bash
gofmt -w internal/gsbench/app.go internal/gsbench/app_plan_phases.go internal/gsbench/app_plan_test.go internal/gsbench/app_plan_phases_test.go internal/gsbench/status.go internal/gsbench/status_test.go
go test ./internal/gsbench -run 'Test.*(Status|Audit|ActivePlan|PlanInit).*' -count=1
go test ./internal/gsbench -count=1
git diff --check
```

Expected: PASS and no metadata-derived active/stale wording for plan faults.

- [ ] **Step 5: Commit logging and documentation**

```bash
git add internal/gsbench/app.go internal/gsbench/app_plan_phases.go internal/gsbench/app_plan_test.go internal/gsbench/app_plan_phases_test.go internal/gsbench/status.go internal/gsbench/status_test.go docs/gsbench/PLAN_601_606_CN.md docs/gsbench/README.md
git commit -m "docs(gsbench): describe plan fault audit-only state"
```

---

### Task 5: Verify, deploy, live-test, package, and publish

**Files:**
- Modify only if verification finds a defect: files owned by Tasks 1–4.
- Deploy: `/Users/sqlrush/gstop/gsbench-local/bin/gsbench`
- Release: `/Users/sqlrush/gstop/release/gsbench-v1.1.8-linux-arm64-20260808-r3.tar.gz`

**Interfaces:**
- Consumes: the completed feature branch and existing v1.1.8 deployment/release layout.
- Produces: verified Darwin ARM64 deployment, verified Linux ARM64 r3 archive, committed source, and synchronized GitHub main.

- [ ] **Step 1: Run the complete verification suite**

```bash
gofmt -d internal/gsbench
git diff --check
go test ./... -count=1
go vet ./...
go test -race ./internal/gsbench -count=1
```

Expected: all commands exit 0 and formatting/diff checks print nothing.

- [ ] **Step 2: Run the minimum platform-mandated reviews**

Run one Go-focused review and one general code review over the final branch diff only. Do not add per-task review loops. Resolve only concrete correctness, safety, or maintainability findings, then rerun affected tests.

- [ ] **Step 3: Commit any final verified adjustment**

If Steps 1–2 required changes:

```bash
git add internal/gsbench docs/gsbench
git commit -m "fix(gsbench): finalize nonblocking plan faults"
```

If no changes were required, do not create an empty commit.

- [ ] **Step 4: Build and atomically deploy Darwin ARM64**

```bash
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 \
  go build -mod=readonly -buildvcs=true -trimpath \
  -o /private/tmp/gsbench-v118-live-plan-fault ./cmd/gsbench
```

Verify `version`, `file`, `go version -m`, and SHA256. Back up the current target to a unique explicit path under `/Users/sqlrush/gstop/gsbench-local/bin`, then install through a same-directory temporary file and atomic rename. Verify `v1.1.8`, final revision, and `vcs.modified=false`.

- [ ] **Step 5: Run live 601 and 602 acceptance**

Use `/Users/sqlrush/gstop/gsbench-local/gsbench.cfg`.

601 sequence:

```bash
gsbench run 601 init --worker 1 --duration 3m
gsbench run 601 fault
gsbench run 601 fault
gsbench run 601 recover
```

Expected: both faults succeed; the second logs `state=FAULT_PRESENT action=continue`; recover displays canonical index recovery. Execute the displayed recovery DDL manually, then confirm `ALREADY_RESTORED`.

602 sequence after retaining old 601 audit rows:

```bash
gsbench run 602 init --worker 1 --duration 3m
gsbench run 602 fault
gsbench run 602 fault
gsbench run 602 recover
```

Expected: old 601 audit rows do not block 602; both faults succeed; the second logs `state=FAULT_PRESENT action=continue`; recover displays ordered `RESET (n_distinct)` and `ANALYZE`. Execute both manually and confirm the next recover reports `ALREADY_RESTORED`.

Stop or let both init workloads finish, then verify no tagged sessions remain.

- [ ] **Step 6: Build and verify Linux ARM64 r3**

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -mod=readonly -buildvcs=true -trimpath \
  -o /private/tmp/gsbench-v118-linux-arm64-r3 ./cmd/gsbench
```

Follow the r2 layout and create:

```text
/Users/sqlrush/gstop/release/gsbench-v1.1.8-linux-arm64-20260808-r3/
/Users/sqlrush/gstop/release/gsbench-v1.1.8-linux-arm64-20260808-r3.tar.gz
/Users/sqlrush/gstop/release/gsbench-v1.1.8-linux-arm64-20260808-r3.tar.gz.sha256
```

Verify in a new temporary extraction directory:

```text
outer checksum OK
inner SHA256SUMS OK
ELF 64-bit ARM aarch64, statically linked
vcs.revision equals the full SHA printed by `git rev-parse HEAD`
vcs.modified=false
no .DS_Store or AppleDouble files
```

- [ ] **Step 7: Integrate and publish**

Fast-forward local `main` from the verified feature branch, push `main` to GitHub, and confirm local `main`, `origin/main`, deployed-binary revision, and r3 `BUILD_INFO` revision are identical. Do not create a PR or tag unless repository state requires one.

- [ ] **Step 8: Record final evidence**

Update `task_plan.md`, `findings.md`, and `progress.md` with test results, live run IDs, binary/archive SHA256 values, final commit, deployment backup path, and GitHub synchronization evidence.
