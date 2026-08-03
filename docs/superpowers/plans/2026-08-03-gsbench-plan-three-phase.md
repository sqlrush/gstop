# gsbench 601–606 Three-Phase Plan Scenario Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the one-shot 601–606 runner lifecycle with `init`, `fault`, and `recover` run actions that share one persistent workload and can be controlled from two terminals.

**Architecture:** Parse the second positional run argument into a dedicated `PlanRunAction`. A new plan control service reuses `meta_runs`, `meta_journal`, and database advisory locks: init holds an activity lock for liveness, while short fault/recover commands take the existing plan mutation lock. A dedicated traffic engine owns fixed tagged sessions and continuously round-robins the canonical candidate SQL until duration expires.

**Tech Stack:** Go 1.22, `database/sql`, openGauss connector, existing gsbench SQL journal, database advisory locks, Go unit tests, Linux ARM64 cross-compilation.

## Global Constraints

- The public commands are exactly `gsbench run <601-606> init --worker N --duration D`, `gsbench run <601-606> fault`, and `gsbench run <601-606> recover`.
- `--duration` is the total init traffic duration; one Ctrl+C immediately terminates the foreground process.
- Fault and recover wait for their SQL work to finish, then exit; they do not create workers.
- The same worker sessions and deterministic SQL mix remain active across all three phases.
- Only one 601–606 experiment may be active for a target database/schema.
- Runtime plan/performance thresholds remain warnings when validation is disabled; SQL, state, and restoration errors remain failures.
- Keep the existing 601–606 SQL definitions and persistent mutation journal.
- Bump `v1.1.2` to `v1.1.3` and produce a Linux ARM64 release archive.
- Do not add a daemon, new dependency, or new dataset metadata table.

---

### Task 1: Three-phase CLI grammar

**Files:**
- Modify: `internal/gsbench/cli.go`
- Modify: `internal/gsbench/cli_test.go`

**Interfaces:**
- Produces: `type PlanRunAction string`
- Produces: `CLIOptions.PlanAction PlanRunAction`
- Produces: `CLIOptions.PlanWorkers int`
- Consumes later: `executeCommand` dispatches non-empty `PlanAction` to the three-phase service.

- [ ] **Step 1: Write failing parser tests**

Add table-driven tests equivalent to:

```go
func TestParseCLIArgsSupportsThreePhasePlanCommands(t *testing.T) {
    init, err := ParseCLIArgs([]string{"run", "601", "init", "--worker", "10", "--duration", "1m"})
    if err != nil || init.PlanAction != PlanRunInit || init.PlanWorkers != 10 || init.Duration != time.Minute {
        t.Fatalf("init=%+v err=%v", init, err)
    }
    fault, err := ParseCLIArgs([]string{"run", "606", "fault"})
    if err != nil || fault.PlanAction != PlanRunFault {
        t.Fatalf("fault=%+v err=%v", fault, err)
    }
    recover, err := ParseCLIArgs([]string{"run", "606", "recover"})
    if err != nil || recover.PlanAction != PlanRunRecover {
        t.Fatalf("recover=%+v err=%v", recover, err)
    }
}
```

Also assert rejection of old `run 601`, unknown actions, multiple scenarios, action on non-plan code, missing init worker/duration, fault/recover overrides, and simultaneous `--worker`/`--workers`.

- [ ] **Step 2: Run the focused tests and confirm failure**

Run:

```bash
go test ./internal/gsbench -run 'TestParseCLIArgs(SupportsThreePhasePlanCommands|RejectsInvalidThreePhasePlanCommands)' -count=1
```

Expected: FAIL because `PlanRunAction`, `PlanAction`, `PlanWorkers`, and the two-position grammar do not exist.

- [ ] **Step 3: Implement the typed action and positional parser**

Add:

```go
type PlanRunAction string

const (
    PlanRunInit    PlanRunAction = "init"
    PlanRunFault   PlanRunAction = "fault"
    PlanRunRecover PlanRunAction = "recover"
)
```

Extend `CLIOptions` with `PlanAction` and `PlanWorkers`. Register singular `--worker` separately and retain plural `--workers`. For `run`, move the leading scenario plus optional recognized plan action behind flags before `FlagSet.Parse`, then consume `[scenario, action]`. Route plural workers into `PlanWorkers` only when a plan action is present; otherwise preserve the existing fixed/memory worker path. Require exactly one plan scenario, require worker and explicit positive duration for init, and reject worker/duration for fault/recover.

- [ ] **Step 4: Update help and make parser tests pass**

Add these lines to usage:

```text
gsbench run 601 init --worker N --duration DURATION
gsbench run 601 fault
gsbench run 601 recover
--worker N  fixed workload workers for 601-606 init (--workers alias)
```

Run:

```bash
go test ./internal/gsbench -run 'Test(ParseCLIArgs|CLIHelp)' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit the CLI slice**

```bash
git add internal/gsbench/cli.go internal/gsbench/cli_test.go
git commit -m "feat(gsbench): parse three-phase plan commands"
```

### Task 2: Advisory liveness and persistent plan control state

**Files:**
- Modify: `internal/gsbench/run_lock.go`
- Modify: `internal/gsbench/app_plan_test.go`
- Create: `internal/gsbench/plan_control.go`
- Create: `internal/gsbench/plan_control_test.go`

**Interfaces:**
- Produces: `planActivityLockIdentity(BenchConfig) string`
- Produces: `databaseRunLockHeld(context.Context, *Database, string) (bool, error)`
- Produces: `planControlStore.start`, `findActive`, `findRecoverable`, `setPhase`, and `finishTraffic`.
- Consumes: existing `meta_runs`, `meta_journal`, `AcquireDatabaseRunLock`, `quoteDatasetSchema`.

- [ ] **Step 1: Write failing lock-liveness tests**

Extend the existing advisory-lock fake to verify:

```go
held, err := databaseRunLockHeld(ctx, db, "gsbench:plan-traffic:postgres:gsbench")
```

returns true without unlocking another session's lock, and returns false only after acquiring, releasing, and closing its own probe session.

- [ ] **Step 2: Implement the read-only liveness probe**

Use a dedicated `runLockSession`. `TryLock=false` means held by another session. `TryLock=true` must be paired with `Unlock` before close. Any uncertain unlock path discards the physical connection.

- [ ] **Step 3: Write failing control-store tests**

Use a recording `journalDatabase` fake to assert exact state transitions:

```text
plan_baseline -> plan_faulting -> plan_fault_active
plan_fault_active -> plan_recovering -> plan_recovered
```

Assert that active discovery only accepts a single `status='running'` three-phase row, while recoverable discovery joins `meta_journal` and still finds pending actions after init is no longer running.

- [ ] **Step 4: Implement `plan_control.go` against existing metadata**

Define phase constants `plan_baseline`, `plan_faulting`, `plan_fault_active`, `plan_recovering`, `plan_recovered`, `plan_fault_failed`, and `plan_recover_failed`. Insert init rows directly into `meta_runs` with `scenarios='601'`, `status='running'`, and `detail='three_phase workers=N'`. Query pending recovery by joining `meta_runs.run_id` to non-restored `meta_journal` rows for the requested scenario code. Do not add columns or tables.

Use two lock identities:

```go
func planActivityLockIdentity(cfg BenchConfig) string {
    return fmt.Sprintf("gsbench:plan-traffic:%s:%s", cfg.Database.Database, cfg.Data.Schema)
}

// Existing planDatabaseLockIdentity remains the short mutation/control lock.
```

- [ ] **Step 5: Run focused control tests**

```bash
go test ./internal/gsbench -run 'Test(DatabaseRunLockHeld|PlanControl)' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit the coordination slice**

```bash
git add internal/gsbench/run_lock.go internal/gsbench/app_plan_test.go internal/gsbench/plan_control.go internal/gsbench/plan_control_test.go
git commit -m "feat(gsbench): coordinate plan phases across processes"
```

### Task 3: Fixed-session plan traffic engine

**Files:**
- Create: `internal/gsbench/plan_traffic.go`
- Create: `internal/gsbench/plan_traffic_test.go`
- Modify: `internal/gsbench/scenario_plan.go`

**Interfaces:**
- Produces: `newPlanTraffic(context.Context, *Runtime, PlanScenarioDefinition, int) (*planTraffic, error)`
- Produces: `(*planTraffic).Run(context.Context, time.Duration) (WorkerSnapshot, error)`
- Produces: deterministic candidate selector `planCandidateIndex(workerID, operation, candidateCount int) int`.
- Consumes: `PlanScenarioDefinitions`, `newSQLWorkloadWithoutOperationTimeoutWithStartGate`, `PrepareSessions`, and `WorkerGroup.SetRunDeadline`.

- [ ] **Step 1: Write failing deterministic traffic tests**

Assert candidate selection exactly preserves a fixed mix:

```go
wantWorker0 := []int{0, 1, 2, 0, 1, 2}
wantWorker1 := []int{1, 2, 0, 1, 2, 0}
```

Use fake tagged connections to assert N sessions are prepared once, remain the same while a shared start gate runs operations, and all stop at the configured deadline.

- [ ] **Step 2: Run and confirm the tests fail**

```bash
go test ./internal/gsbench -run 'TestPlanTraffic' -count=1
```

Expected: FAIL because the dedicated traffic engine is absent.

- [ ] **Step 3: Implement the traffic engine**

Build a per-worker counter and execute the next canonical literal SQL with `QueryContext` and `consumeRows`. Create all tagged sessions before closing the start gate. Install the absolute deadline before release. Disable the per-operation timeout so a fault-slowed query does not retire a worker; the run deadline/context still cancels in-flight SQL.

Return a real error for session preparation failure, requested/started worker mismatch, non-cancellation query failure, or bounded stop failure. Do not implement CPU/QPS feedback or sleep.

- [ ] **Step 4: Preserve plan observations without blocking workers**

Keep `ObserveLiteralPlan` for boundary observations, but make plan traffic use the candidate list directly instead of selecting a single post-fault query. Log baseline plan signatures before the start gate and allow the init phase watcher to log signatures after `fault` and `recover` transitions.

- [ ] **Step 5: Run traffic and existing plan tests**

```bash
go test ./internal/gsbench -run 'Test(PlanTraffic|PlanScenario|LiteralPlan)' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit the traffic slice**

```bash
git add internal/gsbench/plan_traffic.go internal/gsbench/plan_traffic_test.go internal/gsbench/scenario_plan.go
git commit -m "feat(gsbench): sustain plan traffic across phases"
```

### Task 4: Init, fault, and recover command execution

**Files:**
- Create: `internal/gsbench/app_plan_phases.go`
- Create: `internal/gsbench/app_plan_phases_test.go`
- Modify: `internal/gsbench/app.go`
- Modify: `internal/gsbench/journal.go`
- Modify: `internal/gsbench/journal_test.go`
- Modify: `internal/gsbench/plan_definitions.go`

**Interfaces:**
- Produces: `commandPlanRunAction(context.Context, *Database, BenchConfig, Environment, Capabilities, *RunLog, CLIOptions, string) int`
- Produces: `runPlanInit`, `runPlanFault`, and `runPlanRecover` internal functions.
- Consumes: `planControlStore`, activity/control locks, `PlanMutationSet`, `NewSQLJournalWithValidation`, and `planTraffic`.

- [ ] **Step 1: Write failing phase-command tests**

Dependency-inject lock, control-store, journal, and traffic operations. Assert:

- init takes the short control lock, repairs the canonical baseline, takes and holds the activity lock, records `plan_baseline`, then releases traffic after exactly the configured duration;
- fault refuses without a live activity lock, is idempotent in `plan_fault_active`, journals every forward SQL, sets `plan_fault_active`, and exits without stopping traffic;
- recover loads only the selected run's pending plan actions, restores them in reverse sequence, does not call `StopTaggedSessions`, sets `plan_recovered`, and works when activity is no longer held;
- SQL failure leaves `plan_fault_failed` or `plan_recover_failed` plus journal state and returns exit code 1.

- [ ] **Step 2: Implement command dispatch**

Generate a run ID only for normal runs and plan init. Fault/recover use the discovered run ID; their separate log identity must not become the mutation run ID. In the `run` switch, dispatch a non-empty `PlanAction` before `commandRun` so the old Runner cannot auto-inject or auto-restore.

- [ ] **Step 3: Implement init setup and duration lifecycle**

Under the short plan control lock: reject a live three-phase experiment, refuse a stale experiment with pending actions and print its recover command, converge the canonical plan baseline, acquire the activity lock, and insert the run row. Release the control lock before starting workers. Keep the activity lock until traffic has stopped. On normal duration end, reacquire the control lock and mark traffic complete; if mutation remains active, retain a recoverable status and print the exact recover command.

- [ ] **Step 4: Implement synchronous fault**

Under the short control lock, discover the matching `plan_baseline` run and prove the activity lock is held. Change to `plan_faulting`, apply all `PlanMutationSet` actions using the init run ID, then set `plan_fault_active`. If any action fails, persist `plan_fault_failed` and return failure without discarding journal recovery data.

- [ ] **Step 5: Implement non-disruptive recover**

Under the short control lock, discover the newest matching run with non-restored journal actions. Set `plan_recovering`, sort its actions by descending sequence, and call the journal restoration path directly. Do not invoke the universal restore coordinator because it cancels/terminates tagged sessions. After verified inverse actions, set `plan_recovered`; keep status running if the activity lock is held, otherwise finalize it as `UNVERIFIED`.

- [ ] **Step 6: Make planned-action crash recovery idempotent**

Before executing an inverse for an action still in `MutationPlanned`, call `VerifyRestored`. If the original state is already present, atomically mark the action restored and skip the inverse. This covers process death after journal insert but before forward SQL, where 604/605/606 would otherwise try to add already-existing statistics/index objects. Do not use this shortcut for `MutationApplied`, because ANALYZE verify actions intentionally return success regardless of whether their inverse ANALYZE has run.

Add crash-boundary tests for planned-but-never-applied 604, 605, and 606 actions, plus applied actions that still execute inverse SQL.

- [ ] **Step 7: Disable the old one-shot plan path**

Parser-level validation must make a bare `gsbench run 601` fail with the three required commands. Keep the old `PlanChangeScenario` implementation source-compatible for internal restore/catalog tests, but make it unreachable from public CLI without an action.

- [ ] **Step 8: Run app, journal, restore, and plan tests**

```bash
go test ./internal/gsbench -run 'Test(CommandPlan|PlanControl|PlanTraffic|PlanMutation|Restore)' -count=1
```

Expected: PASS, including the assertion that recover does not stop active tagged worker sessions.

- [ ] **Step 9: Commit the command slice**

```bash
git add internal/gsbench/app.go internal/gsbench/app_plan_phases.go internal/gsbench/app_plan_phases_test.go internal/gsbench/journal.go internal/gsbench/journal_test.go internal/gsbench/plan_definitions.go
git commit -m "feat(gsbench): split plan scenarios into three phases"
```

### Task 5: Documentation, compatibility script, and version

**Files:**
- Modify: `internal/gsbench/cli.go`
- Modify: `internal/gsbench/cli_test.go`
- Modify: `docs/gsbench/README.md`
- Modify: `docs/gsbench/CONFIG.md`
- Modify: `docs/gsbench/INSTALL.md`
- Modify: `scripts/validate-gsbench-scenarios.sh`

**Interfaces:**
- Produces: `Version == "v1.1.3"`
- Produces: user-visible three-phase examples for all 601–606 codes.

- [ ] **Step 1: Write the failing version/help assertions**

Update the hard-coded version expectation to `v1.1.3` and assert help contains all three command forms and the `--worker` alias description.

- [ ] **Step 2: Bump version and update docs**

Change `Version` to `v1.1.3`. Replace one-shot 601–606 examples with two-terminal sequences and state that duration is the total init lifetime. Document that fault/recover wait for ANALYZE/index DDL completion and recover remains available after init exits.

- [ ] **Step 3: Update the scenario validation script**

For 601–606, start init in the background, wait until the metadata phase is `plan_baseline`, run fault, run recover, then wait for init. Preserve the existing direct run path for all other scenarios and never call `cleanup --data`.

- [ ] **Step 4: Run help/version and shell syntax checks**

```bash
go test ./internal/gsbench -run 'Test(CLIHelp|RunCLIVersion)' -count=1
bash -n scripts/validate-gsbench-scenarios.sh
```

Expected: PASS.

- [ ] **Step 5: Commit documentation and version**

```bash
git add internal/gsbench/cli.go internal/gsbench/cli_test.go docs/gsbench/README.md docs/gsbench/CONFIG.md docs/gsbench/INSTALL.md scripts/validate-gsbench-scenarios.sh
git commit -m "chore(gsbench): release three-phase plan scenarios v1.1.3"
```

### Task 6: Minimum verification and Linux ARM64 release

**Files:**
- Create/update: `release/gsbench-v1.1.3-linux-arm64-20260803/`
- Create: `release/gsbench-v1.1.3-linux-arm64-20260803.tar.gz`

**Interfaces:**
- Consumes: clean `v1.1.3` source and the existing release packaging layout.
- Produces: Linux ARM64 binary, checksum, config, and focused user manual.

- [ ] **Step 1: Run the package test suite**

```bash
go test ./internal/gsbench ./cmd/gsbench -count=1
```

Expected: PASS.

- [ ] **Step 2: Check the requested commands locally**

```bash
go run ./cmd/gsbench version
go run ./cmd/gsbench run 601 init --worker 10 --duration 1m --help
```

Expected: version banner reports `v1.1.3`; usage documents the three phases. Do not inject a live DDL fault as part of this syntax check.

- [ ] **Step 3: Cross-compile Linux ARM64**

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o release/gsbench-v1.1.3-linux-arm64-20260803/gsbench ./cmd/gsbench
```

- [ ] **Step 4: Package and checksum**

Copy the shipped `gsbench.cfg`, installation guide, and a focused 601–606 three-phase manual into the release directory, then run:

```bash
tar -C release -czf release/gsbench-v1.1.3-linux-arm64-20260803.tar.gz gsbench-v1.1.3-linux-arm64-20260803
shasum -a 256 release/gsbench-v1.1.3-linux-arm64-20260803.tar.gz
```

- [ ] **Step 5: Verify archive contents and binary format**

```bash
tar -tzf release/gsbench-v1.1.3-linux-arm64-20260803.tar.gz
file release/gsbench-v1.1.3-linux-arm64-20260803/gsbench
```

Expected: archive contains the binary/config/manual and `file` reports an ARM aarch64 Linux ELF executable.

- [ ] **Step 6: Commit the implementation plan progress and report artifact paths**

Do not commit generated release binaries unless the repository's existing release policy already tracks them. Report the source commit, archive path, SHA-256, and the three commands.
