# gsbench 602 Statistics Plan Change Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace scenario 602's index-UNUSABLE fault with the same unique-key traffic as 601, force all candidates to sequential scans by overriding `lookup_key n_distinct`, and require verified restoration of every index plan and journal action.

**Architecture:** Keep 602 inside the existing three-phase plan framework and universal journal/restore coordinator. Add explicit plan-shape verifiers at the database boundary, automatically restore rejected faults, and extend global baseline repair so interrupted statistics faults converge idempotently. Preserve old 602 names as input/migration aliases and recover old journal rows from their stored inverse actions.

**Tech Stack:** Go, `database/sql`, gsbench action journal and restore coordinator, GaussDB/openGauss `ALTER COLUMN ... SET/RESET (n_distinct)`, `ANALYZE`, and `EXPLAIN`.

## Global Constraints

- Canonical name: `planchange_stats_lookup`; accepted legacy alias: `planchange_index_unusable`.
- 602 uses the exact three SQL candidates and `plan_data_lookup_idx` token used by 601.
- Fault changes only `lookup_key n_distinct` and statistics; it never updates system catalogs, planner GUCs, or the index.
- `fault` succeeds only when every candidate contains `Seq Scan` and excludes `plan_data_lookup_idx`.
- Rejected/partial faults invoke universal recovery; a safely restored rejection is failed but no longer active.
- `recover` succeeds only after the override is absent, every candidate uses the index, no candidate uses `Seq Scan`, and the journal is empty.
- Strict 602 plan checks remain mandatory when `run.validation_enabled=false`; all other scenario validation behavior remains unchanged.
- Existing v1.1.6 602 journal rows retain and execute their stored `ALTER INDEX ... REBUILD` inverse.
- Do not add review agents or review phases. Preserve the five existing recovery-fix modifications.

---

### Task 0: Checkpoint the Existing Recovery Fix

**Files:**
- Existing modifications: `internal/gsbench/app.go`
- Existing modifications: `internal/gsbench/restore.go`
- Existing modifications: `internal/gsbench/restore_test.go`
- Existing modifications: `internal/gsbench/sqlstore.go`
- Existing modifications: `internal/gsbench/sqlstore_test.go`

**Interfaces:**
- Consumes: the implemented search-path-tolerant index comparison and post-baseline journal reconciliation.
- Produces: a committed baseline so the 602 work does not mix with the earlier recovery fix.

- [ ] **Step 1: Re-run the existing verification**

```bash
go test ./internal/gsbench -count=1
```

Expected: PASS, including the new index-equivalence and reconciliation tests.

- [ ] **Step 2: Check and commit those five paths only**

```bash
git diff --check -- internal/gsbench/app.go internal/gsbench/restore.go internal/gsbench/restore_test.go internal/gsbench/sqlstore.go internal/gsbench/sqlstore_test.go
git add internal/gsbench/app.go internal/gsbench/restore.go internal/gsbench/restore_test.go internal/gsbench/sqlstore.go internal/gsbench/sqlstore_test.go
git commit -m "fix(gsbench): reconcile restored plan baseline actions"
```

Expected: one commit containing only those five paths.

### Task 1: Replace the 602 Identity, Traffic, Alias, and Capability

**Files:**
- Modify: `internal/gsbench/plan_definitions.go`
- Modify: `internal/gsbench/scenario_catalog.go`
- Modify: `internal/gsbench/capability.go`
- Test: `internal/gsbench/plan_definitions_test.go`
- Test: `internal/gsbench/scenario_catalog_test.go`
- Test: `internal/gsbench/capability_test.go`

**Interfaces:**
- Consumes: `PlanScenarioDefinitions`, `ScenarioCatalog.Resolve`, and `validatePlanCapability`.
- Produces: canonical 602 definition, old-name alias resolution, and `PlanNDistinct` capability gating.

- [ ] **Step 1: Write failing tests**

```go
func TestPlanchangeStatsLookupReuses601PointTraffic(t *testing.T) {
	definitions, err := PlanScenarioDefinitions("Bench")
	if err != nil { t.Fatal(err) }
	var one, two PlanScenarioDefinition
	for _, definition := range definitions {
		if definition.Code == 601 { one = definition }
		if definition.Code == 602 { two = definition }
	}
	if two.Name != "planchange_stats_lookup" ||
		!reflect.DeepEqual(two.Candidates, one.Candidates) ||
		two.ExpectedBaselineToken != "plan_data_lookup_idx" {
		t.Fatalf("601=%+v 602=%+v", one, two)
	}
}

func TestDefaultScenarioCatalogResolvesLegacy602Alias(t *testing.T) {
	definition, err := DefaultScenarioCatalog().Resolve("planchange_index_unusable")
	if err != nil { t.Fatal(err) }
	if definition.Code != 602 || definition.Name != "planchange_stats_lookup" {
		t.Fatalf("definition=%+v", definition)
	}
}

func TestPlanchangeStatsLookupRequiresNDistinctCapability(t *testing.T) {
	if err := validatePlanCapability("planchange_stats_lookup", Capabilities{PlanNDistinct: true}); err != nil { t.Fatal(err) }
	if err := validatePlanCapability("planchange_stats_lookup", Capabilities{PlanIndexUnusable: true}); err == nil {
		t.Fatal("accepted obsolete capability")
	}
}
```

Update approved-identity and six-definition expectations to the new canonical name.

- [ ] **Step 2: Verify RED**

```bash
go test ./internal/gsbench -run 'TestPlanchangeStatsLookup|TestDefaultScenarioCatalogResolvesLegacy602Alias|TestPlanchangeDefinitionsUseApprovedIdentities' -count=1
```

Expected: FAIL on the old name, range traffic, token, alias, and capability.

- [ ] **Step 3: Implement the minimal contract**

Build the point-lookups slice once and clone it into both 601 and 602:

```go
{
	Code: 602, Name: "planchange_stats_lookup",
	Candidates: append([]string(nil), pointLookups...),
	ExpectedBaselineToken: "plan_data_lookup_idx",
},
```

Add a private aliases map to `ScenarioCatalog`, register only
`planchange_index_unusable -> catalog.byCode[602]` in `DefaultScenarioCatalog`, and resolve aliases after canonical names. Map both 602 names to `Capabilities.PlanNDistinct`; retain the environment's `PlanIndexUnusable` probe.

- [ ] **Step 4: Verify GREEN**

```bash
go test ./internal/gsbench -run 'TestPlanchangeStatsLookup|TestDefaultScenarioCatalogResolvesLegacy602Alias|TestPlanchangeDefinitionsUseApprovedIdentities|TestPlanDefinitionsCoverSixScenariosWithLiteralSQL' -count=1
```

- [ ] **Step 5: Commit**

```bash
git add internal/gsbench/plan_definitions.go internal/gsbench/scenario_catalog.go internal/gsbench/capability.go internal/gsbench/plan_definitions_test.go internal/gsbench/scenario_catalog_test.go internal/gsbench/capability_test.go
git commit -m "feat(gsbench): redefine scenario 602 traffic"
```

### Task 2: Journal the Statistics Fault and Ordered Recovery

**Files:**
- Modify: `internal/gsbench/plan_definitions.go`
- Test: `internal/gsbench/plan_definitions_test.go`

**Interfaces:**
- Consumes: `PlanMutationSet` and `restoreActionOrder`'s rule that ANALYZE inverses run last.
- Produces: two independently restorable mutations: `statistics_lookup_ndistinct` and `statistics_lookup_analyze`.

- [ ] **Step 1: Write the failing mutation test**

```go
func TestPlanchangeStatsLookupOverridesAndRestoresLookupCardinality(t *testing.T) {
	mutations, err := PlanMutationSet("run-602", "Bench", "planchange_stats_lookup")
	if err != nil { t.Fatal(err) }
	if len(mutations) != 2 { t.Fatalf("mutations=%+v", mutations) }
	wantForward := []string{
		`ALTER TABLE "Bench".plan_data ALTER COLUMN lookup_key SET (n_distinct=1)`,
		`ANALYZE "Bench".plan_data(lookup_key)`,
	}
	wantInverse := []string{
		`ALTER TABLE "Bench".plan_data ALTER COLUMN lookup_key RESET (n_distinct)`,
		`ANALYZE "Bench".plan_data(lookup_key)`,
	}
	for index := range mutations {
		if mutations[index].ForwardSQL != wantForward[index] || mutations[index].InverseSQL != wantInverse[index] {
			t.Fatalf("mutation[%d]=%+v", index, mutations[index])
		}
	}
}
```

Extend the statistics restore-order test to assert RESET precedes lookup-key ANALYZE.

- [ ] **Step 2: Verify RED**

```bash
go test ./internal/gsbench -run 'TestPlanchangeStatsLookupOverridesAndRestoresLookupCardinality|TestCombinedPlanStatisticsRestoreExecutesPrerequisitesBeforeAnalyze' -count=1
```

Expected: FAIL because the new name has no mutation set and old 602 uses UNUSABLE/REBUILD.

- [ ] **Step 3: Implement two mutations**

Handle both the canonical and legacy names. Use exact forward/inverse SQL from the test. The first mutation verifies that `pg_attribute.attoptions` for `lookup_key` no longer contains `n_distinct=`; the ANALYZE mutation verifies `SELECT 1`. Map both names to code 602 in `planChangeCodeForName`.

Do not transform stored legacy actions: the restore coordinator executes their serialized inverse payload.

- [ ] **Step 4: Verify GREEN**

```bash
go test ./internal/gsbench -run 'TestPlanchangeStatsLookupOverridesAndRestoresLookupCardinality|TestCombinedPlanStatisticsRestoreExecutesPrerequisitesBeforeAnalyze|TestEveryPlanMutationHasInverseAndRestoreVerification' -count=1
```

- [ ] **Step 5: Commit**

```bash
git add internal/gsbench/plan_definitions.go internal/gsbench/plan_definitions_test.go
git commit -m "feat(gsbench): inject 602 lookup statistics fault"
```

### Task 3: Add Strict Plan Verification and Baseline Convergence

**Files:**
- Modify: `internal/gsbench/plan_baseline.go`
- Test: `internal/gsbench/plan_baseline_test.go`

**Interfaces:**
- Consumes: `planBaselineExplainer.Explain` and `PlanScenarioDefinition`.
- Produces: `verifyPlanFaultPlans(context.Context, planBaselineExplainer, PlanScenarioDefinition) error`; strict all-candidate 602 baseline validation; idempotent lookup statistics repair.

- [ ] **Step 1: Write failing plan tests**

```go
type mappedPlanExplainer map[string]string

func (e mappedPlanExplainer) Explain(_ context.Context, query string) (string, error) {
	return e[query], nil
}

func Test602FaultVerificationRequiresEveryCandidateToUseSeqScan(t *testing.T) {
	definitions, _ := PlanScenarioDefinitions("gsbench")
	definition := selectPlanBaselineDefinitions(definitions, []ScenarioCode{602})[0]
	plans := mappedPlanExplainer{}
	for _, query := range definition.Candidates { plans[query] = "Seq Scan on plan_data" }
	if err := verifyPlanFaultPlans(context.Background(), plans, definition); err != nil { t.Fatal(err) }
	plans[definition.Candidates[1]] = "Index Scan using plan_data_lookup_idx"
	if err := verifyPlanFaultPlans(context.Background(), plans, definition); err == nil {
		t.Fatal("mixed plans accepted")
	}
}

func Test602BaselineVerificationRequiresEveryCandidateToUseLookupIndex(t *testing.T) {
	definitions, _ := PlanScenarioDefinitions("gsbench")
	definition := selectPlanBaselineDefinitions(definitions, []ScenarioCode{602})[0]
	plans := mappedPlanExplainer{}
	for _, query := range definition.Candidates { plans[query] = "Index Scan using plan_data_lookup_idx" }
	plans[definition.Candidates[2]] = "Seq Scan on plan_data"
	if err := verifyPlanBaselinePlans(context.Background(), plans, []PlanScenarioDefinition{definition}); err == nil {
		t.Fatal("partial baseline accepted")
	}
}
```

Extend `TestPlanBaselineRepairSQLIsScopedAndComplete` to require exact lookup-key RESET and ANALYZE tokens, with RESET ordered first.

- [ ] **Step 2: Verify RED**

```bash
go test ./internal/gsbench -run 'Test602FaultVerificationRequiresEveryCandidateToUseSeqScan|Test602BaselineVerificationRequiresEveryCandidateToUseLookupIndex|TestPlanBaselineRepairSQLIsScopedAndComplete' -count=1
```

Expected: FAIL because the fault verifier and lookup repair are absent and baseline validation accepts one matching candidate.

- [ ] **Step 3: Implement strict shape verification**

For every 602 candidate, `verifyPlanFaultPlans` requires:

```go
strings.Contains(plan, "Seq Scan") &&
	!strings.Contains(plan, definition.ExpectedBaselineToken)
```

For `definition.Code == 602`, make `verifyPlanBaselinePlans` require every candidate to contain `ExpectedBaselineToken` and exclude `Seq Scan`. Preserve the current at-least-one-token rule for 601 and 603–606.

- [ ] **Step 4: Implement idempotent baseline repair**

Add these ordered repair statements:

```sql
ALTER TABLE <schema>.plan_data ALTER COLUMN lookup_key RESET (n_distinct)
ANALYZE <schema>.plan_data(lookup_key)
```

In `RepairPlanBaseline`, execute RESET only if the following count is zero, record `ALREADY_OK` otherwise, and always run column-level ANALYZE before the existing full-table ANALYZE:

```sql
SELECT count(*) FROM pg_attribute
WHERE attrelid='<schema>.plan_data'::regclass
  AND attname='lookup_key'
  AND COALESCE(array_to_string(attoptions,','),'') NOT LIKE '%n_distinct=%'
```

Add the same metadata condition to `verifyPlanBaseline`.

- [ ] **Step 5: Verify GREEN**

```bash
go test ./internal/gsbench -run 'Test602FaultVerificationRequiresEveryCandidateToUseSeqScan|Test602BaselineVerificationRequiresEveryCandidateToUseLookupIndex|TestPlanBaselineRepair|TestRepairPlanBaseline' -count=1
```

- [ ] **Step 6: Commit**

```bash
git add internal/gsbench/plan_baseline.go internal/gsbench/plan_baseline_test.go
git commit -m "feat(gsbench): verify and repair 602 plan state"
```

### Task 4: Reject Unverified Faults and Automatically Restore Them

**Files:**
- Modify: `internal/gsbench/app_plan_phases.go`
- Modify: `internal/gsbench/plan_control.go`
- Test: `internal/gsbench/app_plan_phases_test.go`
- Test: `internal/gsbench/plan_control_test.go`

**Interfaces:**
- Consumes: `verifyPlanFaultPlans`, `databasePlanBaselineExplainer`, and `databasePlanActionBackend.RestoreFault`.
- Produces: `planActionBackend.VerifyFault(context.Context, ScenarioCode) error` and `MarkFaultFailed(context.Context, string, error, bool) error`; the boolean states whether automatic recovery succeeded.

- [ ] **Step 1: Write failing orchestration tests**

Extend `planActionBackendTest` with a `VerifyFault` event and the new `MarkFaultFailed` boolean. Assert successful 602 ordering:

```go
[]string{
	"lock", "resolve-workload", "workload-alive", "resolve-fault",
	"start-fault:fault-602", "apply-fault:fault-602",
	"verify-fault:602", "mark-active:fault-602", "unlock",
}
```

For a verification error, assert `restore-fault:fault-602` then `mark-failed:fault-602:restored=true`, no `mark-active`, and a returned error containing `fault plan`. Add a control-store test proving restored rejection writes `phase=restore,status=FAILED`; failed rollback writes `status=restore_failed`.

- [ ] **Step 2: Verify RED**

```bash
go test ./internal/gsbench -run 'TestExecutePlanFaultAction|TestPlanControlMarksRejectedFault' -count=1
```

Expected: FAIL because there is no verify step, no automatic rollback, and no safe-rejection state.

- [ ] **Step 3: Implement the fault verification boundary**

Add to `planActionBackend`:

```go
VerifyFault(context.Context, ScenarioCode) error
MarkFaultFailed(context.Context, string, error, bool) error
```

`databasePlanActionBackend.VerifyFault` is a no-op for other codes. For 602 it resolves the definition and calls:

```go
verifyPlanFaultPlans(ctx, databasePlanBaselineExplainer{db: b.db}, definition)
```

This check is unconditional and ignores `ValidationEnabled`.

- [ ] **Step 4: Implement automatic rejection recovery**

Route apply, verify, and mark-active failures through one helper. It calls `RestoreFault(runID)`, computes `restored := restoreErr == nil`, calls `MarkFaultFailed(..., restored)`, and returns `errors.Join(primaryErr, restoreErr, markErr)`. Only verified faults reach `MarkFaultActive`.

When `restored=true`, `planControlStore.MarkFaultFailed` updates `phase=restore,status=FAILED`; `ResolveFault` therefore does not consider it active. When false, retain `status=restore_failed` so `602 recover` discovers it.

- [ ] **Step 5: Verify GREEN**

```bash
go test ./internal/gsbench -run 'TestExecutePlanFaultAction|TestExecutePlanRecoverAction|TestPlanControlMarksRejectedFault' -count=1
```

- [ ] **Step 6: Commit**

```bash
git add internal/gsbench/app_plan_phases.go internal/gsbench/plan_control.go internal/gsbench/app_plan_phases_test.go internal/gsbench/plan_control_test.go
git commit -m "feat(gsbench): reject unverified 602 faults"
```

### Task 5: Require Strict 602 Baselines During Init and Recover

**Files:**
- Modify: `internal/gsbench/app.go`
- Modify: `internal/gsbench/sqlstore.go`
- Test: `internal/gsbench/app_plan_test.go`
- Test: `internal/gsbench/sqlstore_test.go`

**Interfaces:**
- Consumes: `preparePlanRunBaseline`, `databaseRestoreBackend.verifyPlanBaselineForActions`, and `legacyJournalScenarioCodes`.
- Produces: `planBaselineVerificationRequired(bool, []ScenarioCode) bool` and `restorePlanVerificationRequired(bool, []Action) bool`.

- [ ] **Step 1: Write failing policy and compatibility tests**

```go
func TestPlanBaselineVerificationIsMandatoryFor602(t *testing.T) {
	if !planBaselineVerificationRequired(false, []ScenarioCode{602}) { t.Fatal("602 init skipped verification") }
	if planBaselineVerificationRequired(false, []ScenarioCode{601}) { t.Fatal("601 behavior changed") }
}

func TestRestorePlanVerificationIsMandatoryFor602(t *testing.T) {
	actions := []Action{{ScenarioCode: 602, Kind: ActionSQLMutation}}
	if !restorePlanVerificationRequired(false, actions) { t.Fatal("602 recover skipped verification") }
}

func TestLegacyJournalScenarioCodesPreserveOld602Name(t *testing.T) {
	codes := legacyJournalScenarioCodes()
	if codes["planchange_index_unusable"] != 602 || codes["planchange_stats_lookup"] != 602 {
		t.Fatalf("codes=%v", codes)
	}
}
```

- [ ] **Step 2: Verify RED**

```bash
go test ./internal/gsbench -run 'TestPlanBaselineVerificationIsMandatoryFor602|TestRestorePlanVerificationIsMandatoryFor602|TestLegacyJournalScenarioCodesPreserveOld602Name' -count=1
```

- [ ] **Step 3: Implement mandatory verification policies**

`planBaselineVerificationRequired` returns true when ordinary validation is enabled or codes contain 602. Use it in `preparePlanRunBaseline`. `restorePlanVerificationRequired` applies the same rule to plan-change actions; use it in `verifyPlanBaselineForActions`. The existing `VerifyRestore` pending-action check remains the journal-empty postcondition.

- [ ] **Step 4: Preserve legacy migration names**

Keep `plan_index_unusable -> 602`, explicitly add `planchange_index_unusable -> 602`, and point the short legacy name's canonical lookup to `planchange_stats_lookup`. Do not change stored action payloads.

- [ ] **Step 5: Verify GREEN**

```bash
go test ./internal/gsbench -run 'TestPlanBaselineVerificationIsMandatoryFor602|TestRestorePlanVerificationIsMandatoryFor602|TestLegacyJournalScenarioCodesPreserveOld602Name|TestVerifyRestorePlanBaseline' -count=1
```

- [ ] **Step 6: Commit**

```bash
git add internal/gsbench/app.go internal/gsbench/sqlstore.go internal/gsbench/app_plan_test.go internal/gsbench/sqlstore_test.go
git commit -m "fix(gsbench): require verified 602 recovery"
```

### Task 6: Update the v1.1.7 User Contract

**Files:**
- Modify: `internal/gsbench/cli.go`
- Test: `internal/gsbench/cli_test.go`
- Modify: `docs/gsbench/PLAN_601_606_CN.md`

**Interfaces:**
- Consumes: CLI `Version`, `DefaultScenarioCatalog`, and the three-phase manual.
- Produces: `gsbench v1.1.7`, canonical CLI output `602=planchange_stats_lookup`, old-name input compatibility, and accurate operator instructions.

- [ ] **Step 1: Write failing CLI expectations**

Expect:

```go
strings.HasPrefix(stdout.String(), "gsbench v1.1.7\n")
```

Change scenario/help expectations to `602=planchange_stats_lookup`, and assert parsing `planchange_index_unusable` still returns code 602.

- [ ] **Step 2: Verify RED**

```bash
go test ./internal/gsbench -run 'TestCLIVersionPrintsAuthor|TestCLIHelp|TestParseCLIArgs' -count=1
```

Expected: FAIL on the old version and displayed canonical name.

- [ ] **Step 3: Update version and manual**

Set:

```go
const Version = "v1.1.7"
```

Change the 602 manual row to:

```text
fault: lookup_key SET (n_distinct=1) 后 ANALYZE，三条点查全部变为 Seq Scan
recover: RESET (n_distinct) 后 ANALYZE，三条点查全部恢复 plan_data_lookup_idx
```

Document mandatory 602 verification when validation is disabled and the need to recover an outstanding v1.1.6 602 journal using its stored REBUILD action before injecting a new fault.

Do not edit the dated historical scenario report and do not claim a v1.1.7 Linux release package exists before the user's remaining v1.1.7 features and packaging work are finished.

- [ ] **Step 4: Verify GREEN**

```bash
go test ./internal/gsbench -run 'TestCLIVersionPrintsAuthor|TestCLIHelp|TestParseCLIArgs' -count=1
```

- [ ] **Step 5: Commit**

```bash
git add internal/gsbench/cli.go internal/gsbench/cli_test.go docs/gsbench/PLAN_601_606_CN.md
git commit -m "docs(gsbench): document v1.1.7 scenario 602"
```

### Task 7: Run Minimum Verification

**Files:**
- Verify only; no planned source edits.

**Interfaces:**
- Consumes: Tasks 0–6.
- Produces: package-test, vet, build, and version evidence.

- [ ] **Step 1: Run the package suite once**

```bash
go test ./internal/gsbench -count=1
```

Expected: PASS.

- [ ] **Step 2: Run vet and build**

```bash
go vet ./internal/gsbench
go build -o /private/tmp/gsbench-v1.1.7-602 ./cmd/gsbench
/private/tmp/gsbench-v1.1.7-602 version
```

Expected: every command exits 0 and version output begins `gsbench v1.1.7`.

- [ ] **Step 3: Check diff and repository state**

```bash
git diff --check
git status --short --branch
git log -8 --oneline
```

Expected: no whitespace errors and no unexplained files. Do not build or publish a release archive in this task.

- [ ] **Step 4: Record the live acceptance boundary**

Do not claim target-database acceptance until this sequence is run against the release GaussDB/openGauss environment with at least one million `plan_data` rows:

```bash
gsbench run 602 init --worker 2 --duration 2m
gsbench run 602 fault
gsbench run 602 recover
```

Inspect all three logged EXPLAIN plans after fault/recover and confirm zero pending database journal actions.
