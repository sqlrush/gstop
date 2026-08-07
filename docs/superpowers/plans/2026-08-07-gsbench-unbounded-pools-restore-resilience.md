# gsbench Unbounded Pool Pressure and Restore Resilience Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make scenarios 401/402 ignore gsbench artificial connection/worker caps, repair session advisory-lock recovery after database resource exhaustion, and prove all v1.1.7 behavior with automated and local extreme tests before rebuilding delivery artifacts.

**Architecture:** Keep the existing scenario lifecycle and safety configuration for every other scenario. Split physical database headroom from configurable workload caps for 401/402, and centralize session advisory locks behind a dedicated one-connection session that uses safely quoted simple SQL rather than driver bind parameters. Preserve fail-closed recovery semantics while classifying only known connectivity/resource errors as retryable.

**Tech Stack:** Go 1.26.5, `database/sql`, `gitcode.com/opengauss/openGauss-connector-go-pq` v1.0.8, Go tests with custom `database/sql/driver` fakes, openGauss `og5`, shell release verification.

## Global Constraints

- Target version remains exactly `gsbench v1.1.7`; do not increment gstop or gsbench versions.
- Only scenarios 401/402 ignore `safety.max_connections` and `safety.max_workers`; every other scenario retains the current caps and safety gates.
- Database physical limits, percent range 1–100, target-above-baseline checks, real metric checks, duration, cleanup, and nonzero failure outcomes remain mandatory.
- Never convert database resource exhaustion into SUCCESS and never lower a requested target automatically.
- 402 must not accept active-backend fallback as percent-target evidence.
- Recovery lock SQL must be injection-safe, use zero bind arguments, preserve database `hashtext` compatibility, and keep all acquired locks on one physical session.
- Known transient connection/resource failures may reconnect; syntax, permission, ownership, unknown, and busy-lock failures remain immediate/fail-closed.
- Do not expose database passwords in command output, logs, test reports, or committed files.
- Use TDD for every production behavior change: add one failing test, run it and record the expected failure, implement the minimum change, rerun green, then commit.
- Do not add independent review agents or review phases; run only the verification required by this plan.

---

## File Structure

- Modify `internal/gsbench/scenario_connections.go`: 401 physical budget calculation and Prepare wiring.
- Modify `internal/gsbench/scenario_capacity_test.go`: 401/402 cap-removal and physical-boundary tests.
- Modify `internal/gsbench/scenario_threads.go`: 402 physical session headroom wiring.
- Create `internal/gsbench/session_advisory_lock.go`: safe simple-query generation, dedicated lock session ownership, close/discard, and retryable-error classification.
- Create `internal/gsbench/session_advisory_lock_test.go`: query safety, zero-argument driver path, dedicated session lifecycle, and error classification tests.
- Modify `internal/gsbench/run_lock.go`: run/plan lock use of the shared dedicated advisory session.
- Modify `internal/gsbench/run_lock_test.go`: unchanged high-level lock semantics plus dedicated-session failure assertions.
- Modify `internal/gsbench/app.go`: restore lock use of the shared advisory session, partial-release handling, and retry wrapping.
- Modify `internal/gsbench/app_plan_test.go`: restore lock acquisition/release, partial lock, retry, and no-double-close coverage using the new interface.
- Modify `docs/gsbench/README.md`, `docs/gsbench/CONFIG.md`, `docs/gsbench/INSTALL.md`, `README.md`, and `configs/gsbench.cfg`: published behavior and comments.
- Create `docs/gsbench/V117_EXTREME_TEST_REPORT_20260807.md`: exact automated/live commands, run IDs, outcomes, cleanup evidence, and environment limitations.
- Rebuild `/Users/sqlrush/gstop/gsbench-local/bin/gsbench`, `/Users/sqlrush/gstop/release/gsbench-v1.1.7-linux-arm64-20260807/`, its `.tar.gz`, and its `.tar.gz.sha256` only after the source and test report are committed and clean.

---

### Task 1: Remove the 401 Artificial Connection Cap

**Files:**
- Modify: `internal/gsbench/scenario_connections.go:18-84,165-187`
- Test: `internal/gsbench/scenario_capacity_test.go:9-80`

**Interfaces:**
- Consumes: `probeConnectionCapacity(ctx, rt) (connectionCapacityFacts, error)` and `PoolTargets.ConnectionPercent`.
- Produces: `calculateConnectionBudget(instanceMax, reserved, existing, targetPercent int) (ConnectionBudget, error)` whose workload target is the complete physical baseline delta.

- [ ] **Step 1: Add a failing behavioral test proving a cap of one is ignored**

Add this test while the production function still accepts `safetyMax`; it must fail with `WorkloadTarget=1`:

```go
func TestConnectionBudgetIgnoresArtificialSafetyCap(t *testing.T) {
	budget, err := calculateConnectionBudget(103, 3, 20, 90, 1)
	if err != nil {
		t.Fatal(err)
	}
	if budget.UsableCapacity != 100 || budget.DesiredTotal != 90 ||
		budget.WorkloadTarget != 70 || budget.ReachableTotal != 90 ||
		budget.Limited {
		t.Fatalf("budget=%+v", budget)
	}
}
```

- [ ] **Step 2: Run the red test**

```bash
go test ./internal/gsbench -run TestConnectionBudgetIgnoresArtificialSafetyCap -count=1
```

Expected: FAIL because the current calculation clamps the workload target to 1.

- [ ] **Step 3: Implement the minimum physical-budget behavior**

```go
desired := int(math.Ceil(float64(usable) * float64(targetPercent) / 100))
needed := max(0, desired-existing)
headroom := max(0, usable-existing)
workloadTarget := min(needed, headroom)
reachable := min(usable, existing+workloadTarget)
```

Remove the `safetyMax < 1` validation and artificial `Limited` condition. Keep baseline validation and ceiling evidence based on physical capacity.

- [ ] **Step 4: Run the focused test green**

```bash
go test ./internal/gsbench -run 'TestConnection(Budget|Target)' -count=1
```

Expected: PASS after updating old tests that intentionally asserted the removed clamp.

- [ ] **Step 5: Refactor away the obsolete argument and add 100% coverage**

Change the signature and all calls:

```go
func calculateConnectionBudget(
	instanceMax, reserved, existing, targetPercent int,
) (ConnectionBudget, error)
```

Update `connectionTarget` and `ConnectionScenario.Prepare` so they no longer read or pass `rt.Config.Safety.MaxConnections`. Add:

```go
func TestConnectionBudgetUsesAllPhysicalHeadroomAtOneHundredPercent(t *testing.T) {
	budget, err := calculateConnectionBudget(103, 3, 20, 100)
	if err != nil {
		t.Fatal(err)
	}
	if budget.WorkloadTarget != 80 || budget.ReachableTotal != 100 {
		t.Fatalf("budget=%+v", budget)
	}
}
```

- [ ] **Step 6: Run 401 tests and commit**

```bash
go test ./internal/gsbench -run 'TestConnection' -count=1
git add internal/gsbench/scenario_connections.go internal/gsbench/scenario_capacity_test.go
git commit -m "feat(gsbench): remove artificial 401 connection cap"
```

Expected: all focused tests PASS; commit contains no 402 or lock changes.

---

### Task 2: Remove the 402 Artificial Worker and Connection Caps

**Files:**
- Modify: `internal/gsbench/scenario_threads.go:180-260`
- Test: `internal/gsbench/scenario_capacity_test.go:90-145`

**Interfaces:**
- Consumes: `connectionCapacityFacts{InstanceMax, Reserved, Existing}` and `ThreadPoolStatus`.
- Produces: `physicalSessionHeadroom(instanceMax, reserved, existing int) int`; 404 continues using the existing cap-aware `threadSessionCapacity` helper.

- [ ] **Step 1: Add a failing test for the new physical-headroom API**

```go
func TestThreadPressurePhysicalHeadroomIgnoresConfiguredCaps(t *testing.T) {
	if got := physicalSessionHeadroom(1000, 10, 100); got != 890 {
		t.Fatalf("physical headroom=%d want=890", got)
	}
	if got := physicalSessionHeadroom(500, 10, 490); got != 0 {
		t.Fatalf("exhausted headroom=%d want=0", got)
	}
}
```

- [ ] **Step 2: Run the red test**

```bash
go test ./internal/gsbench -run TestThreadPressurePhysicalHeadroomIgnoresConfiguredCaps -count=1
```

Expected: build FAIL with `undefined: physicalSessionHeadroom`.

- [ ] **Step 3: Add the physical helper and wire only scenario 402 to it**

```go
func physicalSessionHeadroom(instanceMax, reserved, existing int) int {
	return max(0, instanceMax-reserved-existing)
}
```

In `ThreadScenario.Prepare`, replace the cap-aware call with:

```go
s.maxWorkers = physicalSessionHeadroom(
	facts.InstanceMax,
	facts.Reserved,
	facts.Existing,
)
```

Keep `threadSessionCapacity(..., maxWorkers, maxConnections)` unchanged for scenario 404 and other cap-aware workloads. Change the zero-capacity error to `no physical workload session capacity`.

- [ ] **Step 4: Run focused 402 and 404 capacity tests**

```bash
go test ./internal/gsbench -run 'Test(Thread|DynamicMemory|Capacity)' -count=1
```

Expected: PASS, including existing 404 cap tests.

- [ ] **Step 5: Add a source-level wiring regression**

Add package-private `threadPressureCapacity(facts connectionCapacityFacts) int` and compare it directly with the existing cap-aware helper:

```go
func TestThreadPressureCapacityLeavesCapAwareCapacityForOtherScenarios(t *testing.T) {
	facts := connectionCapacityFacts{InstanceMax: 1000, Reserved: 10, Existing: 100}
	if got := threadPressureCapacity(facts); got != 890 {
		t.Fatalf("402 capacity=%d want=890", got)
	}
	if got := threadSessionCapacity(1000, 10, 100, 1, 1); got != 1 {
		t.Fatalf("cap-aware capacity=%d want=1", got)
	}
}
```

Do not modify the cap-aware helper used by 404.

- [ ] **Step 6: Run and commit**

```bash
go test ./internal/gsbench -run 'Test(Thread|Capacity)' -count=1
git add internal/gsbench/scenario_threads.go internal/gsbench/scenario_capacity_test.go
git commit -m "feat(gsbench): remove artificial 402 worker caps"
```

Expected: focused tests PASS and no changes to other scenario caps.

---

### Task 3: Introduce a Safe Dedicated Session Advisory-Lock Component

**Files:**
- Create: `internal/gsbench/session_advisory_lock.go`
- Create: `internal/gsbench/session_advisory_lock_test.go`
- Modify: `internal/gsbench/run_lock.go:1-105`
- Test: `internal/gsbench/run_lock_test.go`

**Interfaces:**
- Consumes: `*Database`, `Database.cfg.DSN`, `Database.operationContext`, and the openGauss connector's `QuoteLiteral`.
- Produces:
  - `type advisoryLockSession interface { TryLock; Unlock; Scan; Exec; Close; Discard }`
  - `openAdvisoryLockSession(ctx context.Context, db *Database, applicationName string) (advisoryLockSession, error)`
  - `type advisoryLockSessionOpener func(context.Context, *Database, string) (advisoryLockSession, error)` for restore tests and default production wiring.
  - `sessionAdvisoryLockQuery(operation advisoryLockOperation, key string) (string, error)`
  - `isRetryableAdvisoryLockError(err error) bool`

- [ ] **Step 1: Add failing query-generation tests**

```go
func TestSessionAdvisoryLockQueryUsesSafeSimpleSQL(t *testing.T) {
	query, err := sessionAdvisoryLockQuery(
		advisoryTryLock,
		`gsbench/restore/postgres/schema'\\name`,
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(query, "$1") ||
		!strings.Contains(query, "pg_try_advisory_lock") ||
		!strings.Contains(query, "schema''") {
		t.Fatalf("query=%q", query)
	}
}

func TestSessionAdvisoryLockQueryRejectsUnknownOperation(t *testing.T) {
	if _, err := sessionAdvisoryLockQuery("drop", "key"); err == nil {
		t.Fatal("unknown advisory operation accepted")
	}
}
```

- [ ] **Step 2: Run red**

```bash
go test ./internal/gsbench -run TestSessionAdvisoryLockQuery -count=1
```

Expected: build FAIL because the component does not exist.

- [ ] **Step 3: Implement safe query generation**

```go
import pq "gitcode.com/opengauss/openGauss-connector-go-pq"

type advisoryLockOperation string

const (
	advisoryTryLock advisoryLockOperation = "try"
	advisoryUnlock  advisoryLockOperation = "unlock"
)

func sessionAdvisoryLockQuery(
	operation advisoryLockOperation,
	key string,
) (string, error) {
	var function string
	switch operation {
	case advisoryTryLock:
		function = "pg_try_advisory_lock"
	case advisoryUnlock:
		function = "pg_advisory_unlock"
	default:
		return "", fmt.Errorf("unsupported advisory lock operation %q", operation)
	}
	return "SELECT " + function + "(hashtext(" + pq.QuoteLiteral(key) + "))", nil
}
```

- [ ] **Step 4: Add failing driver/lifecycle tests**

Use a custom `driver.Connector` that records query text, argument count, physical close count, and pool creation count. Assert:

```go
if got := len(state.args); got != 0 {
	t.Fatalf("advisory query args=%d want=0", got)
}
if state.physicalClose != 1 {
	t.Fatalf("physical close=%d want=1", state.physicalClose)
}
```

Also assert `Discard()` followed by `Close()` returns nil and does not increment physical close a second time.

- [ ] **Step 5: Run lifecycle tests red**

```bash
go test ./internal/gsbench -run 'Test(SQLAdvisory|SessionAdvisory)' -count=1
```

Expected: FAIL because the dedicated session lifecycle is missing.

- [ ] **Step 6: Implement the dedicated session**

Create a concrete session owning both pool and conn:

```go
type sqlAdvisoryLockSession struct {
	db       *Database
	pool     *sql.DB
	conn     *sql.Conn
	once     sync.Once
	closeErr error
}
```

`openAdvisoryLockSession` opens a new `sql.DB` from the same DSN, sets max open/idle connections to one, obtains one `sql.Conn`, and closes the pool if acquisition fails. `TryLock` and `Unlock` call `QueryRowContext(opCtx, query)` with no args. `Scan` and `Exec` expose the protected connection for restore work. `Close` closes conn then pool exactly once. `Discard` marks the physical connection bad, normalizes `sql.ErrConnDone`/`driver.ErrBadConn`, closes the pool, and shares the same once state.

Add `openAdvisorySession advisoryLockSessionOpener` to `databaseRestoreBackend`; `newDatabaseRestoreBackend` assigns `openAdvisoryLockSession`, while tests inject deterministic fake sessions without opening the real driver.

- [ ] **Step 7: Replace run/plan lock openers**

In `AcquireDatabaseRunLock` and `DatabaseRunLockHeld`, replace `db.pool.Conn` with:

```go
return openAdvisoryLockSession(
	openCtx,
	db,
	db.cfg.Database.ApplicationName+"/advisory-lock",
)
```

Keep `acquireDatabaseRunLock` and `probeDatabaseRunLock` interface-driven semantics intact.

- [ ] **Step 8: Add retryable-error classification tests and implementation**

Test `driver.ErrBadConn`, `sql.ErrConnDone`, SQLSTATE `08006`, `53200`, `53300`, `57P01`, and the exact openGauss memory message as true; test permission `42501`, syntax `42601`, and arbitrary errors as false. Implement classification using `errors.Is`, an interface with `SQLState() string`, SQLSTATE class checks, and the narrow compatibility text.

- [ ] **Step 9: Run and commit**

```bash
go test ./internal/gsbench -run 'Test(DatabaseRunLock|ProbeDatabaseRunLock|SQLAdvisory|SessionAdvisory|RetryableAdvisory)' -count=1
git add internal/gsbench/session_advisory_lock.go internal/gsbench/session_advisory_lock_test.go internal/gsbench/run_lock.go internal/gsbench/run_lock_test.go
git commit -m "fix(gsbench): isolate session advisory locks"
```

Expected: all lock component tests PASS; journal transaction lock code is unchanged.

---

### Task 4: Migrate Restore Locks and Repair Retry/Cleanup Semantics

**Files:**
- Modify: `internal/gsbench/app.go:1210-1260,1580-1710,1840-2030`
- Test: `internal/gsbench/app_plan_test.go:17-95,423-860,1030-1100`
- Test: `internal/gsbench/restore_test.go`

**Interfaces:**
- Consumes: `advisoryLockSession`, `openAdvisoryLockSession`, `isRetryableAdvisoryLockError` from Task 3.
- Produces: restore lock acquisition that never reuses the main pool, retries known resource/connectivity failures through the existing plan-first recovery flow, and closes uncertain sessions exactly once.

- [ ] **Step 1: Add a failing restore regression for zero-argument lock SQL**

Extend the restore test driver to reject any advisory query with arguments:

```go
if strings.Contains(query, "pg_try_advisory_lock") && len(args) != 0 {
	return nil, fmt.Errorf("advisory query used %d bind arguments", len(args))
}
```

Acquire and release a restore lock and assert no argument error and reverse key order.

- [ ] **Step 2: Run red**

```bash
go test ./internal/gsbench -run TestAcquireDatabaseRestoreLockUsesSimpleDedicatedSession -count=1
```

Expected: FAIL because current restore SQL uses `$1` and one argument.

- [ ] **Step 3: Convert `databaseRestoreLock` to the shared session interface**

```go
type databaseRestoreLock struct {
	once    sync.Once
	session advisoryLockSession
	keys    []string
	local   RestoreLock
	err     error
}
```

Implement `DatasetVersion` via `session.Scan`, `Exec` via `session.Exec`, and Release by unlocking keys in reverse then `session.Close()`. On any uncertain unlock, call `session.Discard()` and do not call Close again.

- [ ] **Step 4: Replace acquisition and partial cleanup**

Open one dedicated session before iterating keys. For each key call `session.TryLock`; append only confirmed keys. On query error, release confirmed keys, discard once, and return the joined primary/partial-release errors. On busy false, release confirmed keys and close the known-clean session.

- [ ] **Step 5: Add a failing retry test for resource exhaustion**

Inject an opener whose first session returns fake SQLSTATE `53200`, whose second session succeeds, and a pinger that succeeds after one wait. Assert two distinct sessions were opened and the final lock succeeds. Also inject SQLSTATE `42501` and assert no retry.

- [ ] **Step 6: Run retry test red**

```bash
go test ./internal/gsbench -run 'TestRestoreLock(RetriesResourceExhaustion|DoesNotRetryPermission)' -count=1
```

Expected: FAIL because query errors are not yet wrapped as restore connectivity errors.

- [ ] **Step 7: Implement narrow retry wrapping**

When a dedicated-session open or query error satisfies `isRetryableAdvisoryLockError`, return `newRestoreDatabaseConnectivityError(acquireErr)` after cleanup. Let the existing `acquireRestoreLockPlanFirst` local-control-plane/wait/retry path open the second session. Busy, permission, syntax, ownership, and unknown errors remain direct failures.

- [ ] **Step 8: Add and pass the no-double-close regression**

Reproduce the incident sequence: TryLock error, Discard returns success, wrapper Close would return `sql.ErrConnDone`. Assert the final error contains the primary query error but not `connection is already closed`, and physical close count is exactly one.

- [ ] **Step 9: Run restore suites and commit**

```bash
go test ./internal/gsbench -run 'Test(AcquireDatabaseRestoreLock|RestoreLock|RestoreCoordinator|DatabaseRestore)' -count=1
git add internal/gsbench/app.go internal/gsbench/app_plan_test.go internal/gsbench/restore_test.go
git commit -m "fix(gsbench): recover advisory locks after resource exhaustion"
```

Expected: focused restore tests PASS with no changes to stale fail-closed policy.

---

### Task 5: Update Configuration and Release Documentation

**Files:**
- Modify: `configs/gsbench.cfg:51-56,109-118`
- Modify: `docs/gsbench/CONFIG.md:137-160,220-230`
- Modify: `docs/gsbench/README.md:55-100`
- Modify: `docs/gsbench/INSTALL.md`
- Modify: `README.md`
- Test: `internal/gsbench/config_test.go`

**Interfaces:**
- Consumes: final 401/402 and advisory-lock behavior from Tasks 1–4.
- Produces: operator guidance that caps remain active for other scenarios and physical failures remain failures.

- [ ] **Step 1: Add a cross-behavior configuration regression assertion**

Load `max_connections=1`, `max_workers=1`, select 401/402, and confirm config loading retains both values. Assert the Task 1/2 physical helpers still return the full physical delta/headroom, while an existing cap-aware scenario helper still returns one.

- [ ] **Step 2: Run the focused assertion**

```bash
go test ./internal/gsbench -run TestPoolTargetsRetainButIgnoreOtherScenarioCaps -count=1
```

Expected: PASS using the production helpers completed in Tasks 1/2. This documentation/config task does not add production behavior.

- [ ] **Step 3: Update prose and comments**

Use this exact semantic distinction throughout:

```text
safety.max_connections and safety.max_workers remain hard caps for all
cap-aware scenarios except 401/402. Scenarios 401/402 derive their attempt
capacity from database physical headroom and fail on real resource rejection.
```

Document that session advisory locks use a dedicated connection and reconnect after known temporary connection/resource failures; unknown recovery errors remain fail-closed.

- [ ] **Step 4: Validate docs and commit**

```bash
git diff --check
rg -n "401/402|max_connections|max_workers|advisory|resource" README.md configs/gsbench.cfg docs/gsbench
go test ./internal/gsbench -run 'Test(PoolTargets|Config)' -count=1
git add README.md configs/gsbench.cfg docs/gsbench/README.md docs/gsbench/CONFIG.md docs/gsbench/INSTALL.md internal/gsbench/config_test.go
git commit -m "docs(gsbench): describe unbounded pool pressure"
```

Expected: docs consistently distinguish artificial caps from physical failures.

---

### Task 6: Run Automated Regression and Build Verification

**Files:**
- No production changes expected.
- Modify tests only if a test exposes a real regression; return to the owning TDD task before editing production code.

**Interfaces:**
- Consumes: all implementation commits.
- Produces: a clean automated verification record and candidate binaries.

- [ ] **Step 1: Run focused feature suites**

```bash
go test ./internal/gsbench -run 'Test(Connection|Thread|SessionAdvisory|DatabaseRunLock|ProbeDatabaseRunLock|AcquireDatabaseRestoreLock|RestoreLock|Stale|Plan.*602|Statistics)' -count=1
go test ./internal/monitor -run 'TestMemoryMonitorRefresh' -count=1
```

Expected: PASS, including the `m` dashboard's default five-second refresh and
its exemption from the former dynamic-memory health gate.

- [ ] **Step 2: Run the complete gsbench package suite**

```bash
go test ./internal/gsbench -count=1
```

Expected: PASS.

- [ ] **Step 3: Run the feasible repository suite**

```bash
go list ./... | rg -v '^gstop/cmd/gsbench$' | xargs go test -count=1
go test ./cmd/gsbench -run TestCommandContextLeavesInterruptsToOperatingSystem -count=1
```

Expected: PASS. If the full process test is attempted and sandbox rejects `net.Listen("127.0.0.1:0")`, record only that exact environment limitation.

- [ ] **Step 4: Build host and Linux ARM64 candidates**

```bash
go build -mod=readonly -trimpath -buildvcs=true -o /private/tmp/gsbench-v1.1.7-darwin-arm64 ./cmd/gsbench
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -mod=readonly -trimpath -buildvcs=true -ldflags='-s -w' -o /private/tmp/gsbench-v1.1.7-linux-arm64 ./cmd/gsbench
/private/tmp/gsbench-v1.1.7-darwin-arm64 version
file /private/tmp/gsbench-v1.1.7-linux-arm64
go version -m /private/tmp/gsbench-v1.1.7-linux-arm64
```

Expected: v1.1.7 and Linux ELF aarch64 static. `vcs.modified` may be true only if the extreme-test report has not yet been committed; final delivery is rebuilt after the report commit.

---

### Task 7: Execute Local og5 Extreme Tests and Commit the Evidence Report

**Files:**
- Create: `docs/gsbench/V117_EXTREME_TEST_REPORT_20260807.md`
- Use without committing secrets: `/private/tmp/gsbench-v117-extreme.cfg`, copied from `/Users/sqlrush/gstop/gsbench-local/gsbench.cfg`, with both caps set to 1 and `database.password_config=/Users/sqlrush/gstop/gsbench-local/gstop.cfg` so no password value enters commands or reports.

**Interfaces:**
- Consumes: verified host binary, local og5 endpoint `127.0.0.1:5433`, schema `gsbench_e2e_20260801_100g`.
- Produces: exact run IDs and cleanup/recovery evidence for 401, 402, 602, stale recovery, and the advisory-lock incident path.

- [ ] **Step 1: Establish a clean baseline**

Copy the deployed config, then edit only the temporary copy with `apply_patch`:

```bash
cp /Users/sqlrush/gstop/gsbench-local/gsbench.cfg /private/tmp/gsbench-v117-extreme.cfg
```

Set `safety.max_connections=1`, `safety.max_workers=1`, and
`database.password_config=/Users/sqlrush/gstop/gsbench-local/gstop.cfg` in the
temporary file. Remove any inline `database.password` value before displaying
or recording the config. Then run:

```bash
/private/tmp/gsbench-v1.1.7-darwin-arm64 doctor --config /private/tmp/gsbench-v117-extreme.cfg
/private/tmp/gsbench-v1.1.7-darwin-arm64 restore --dry-run --config /private/tmp/gsbench-v117-extreme.cfg
/private/tmp/gsbench-v1.1.7-darwin-arm64 status --config /private/tmp/gsbench-v117-extreme.cfg
```

Expected: target identity confirmed and pending actions known. Resolve real actions before pressure testing without deleting the dataset.

- [ ] **Step 2: Prove 401 ignores a cap of one**

Set both caps to 1. The known local baseline is below 10%; rerun baseline discovery immediately before this command, then run the exact 10% target:

```bash
/private/tmp/gsbench-v1.1.7-darwin-arm64 run 401 --percent 10 --duration 10s --config /private/tmp/gsbench-v117-extreme.cfg
```

Expected: `workload_connection_target > 1`, SUCCESS, approximately 10 seconds hold, then zero tagged connections.

- [ ] **Step 3: Exercise 401 physical exhaustion**

Run serially and verify cleanup between commands:

```bash
/private/tmp/gsbench-v1.1.7-darwin-arm64 run 401 --percent 60 --duration 15s --config /private/tmp/gsbench-v117-extreme.cfg
/private/tmp/gsbench-v1.1.7-darwin-arm64 run 401 --percent 90 --duration 15s --config /private/tmp/gsbench-v117-extreme.cfg
/private/tmp/gsbench-v1.1.7-darwin-arm64 run 401 --percent 100 --duration 15s --config /private/tmp/gsbench-v117-extreme.cfg
```

Expected on this constrained container: a physical resource error is acceptable, but every run must close tagged sessions, restore without parameter-count/double-close errors, and allow the next command to start.

- [ ] **Step 4: Exercise 402 capability and extremes**

Record doctor evidence. If real thread-pool evidence is present, repeat the cap-of-one proof and run 90/100 targets. If absent, record the exact capability result and rely on automated controller/driver integration coverage; do not enable fallback and do not claim live SUCCESS.

- [ ] **Step 5: Exercise 602 fault and recovery**

Start init in a managed terminal session:

```bash
/private/tmp/gsbench-v1.1.7-darwin-arm64 run 602 init --worker 10 --duration 5m --config /private/tmp/gsbench-v117-extreme.cfg
```

After `RUNNING`, execute:

```bash
/private/tmp/gsbench-v1.1.7-darwin-arm64 run 602 fault --config /private/tmp/gsbench-v117-extreme.cfg
/private/tmp/gsbench-v1.1.7-darwin-arm64 run 602 recover --config /private/tmp/gsbench-v117-extreme.cfg
/private/tmp/gsbench-v1.1.7-darwin-arm64 run 602 recover --config /private/tmp/gsbench-v117-extreme.cfg
```

Expected: fault proves all three Seq Scan plans; first recover proves override cleared, three unique-index plans, and empty journal; second recover is idempotent. Stop init and confirm no workload remains.

- [ ] **Step 6: Exercise stale recovery boundaries**

Use controlled test fixtures rather than hand-editing production metadata. Prove an action-free 401/402 failed run allows a subsequent run, while an action-bearing or unknown-scenario stale run blocks. Restore fixture state before continuing.

- [ ] **Step 7: Final cleanup and downstream proof**

```bash
/private/tmp/gsbench-v1.1.7-darwin-arm64 restore --dry-run --config /private/tmp/gsbench-v117-extreme.cfg
/private/tmp/gsbench-v1.1.7-darwin-arm64 status --config /private/tmp/gsbench-v117-extreme.cfg
/private/tmp/gsbench-v1.1.7-darwin-arm64 run 101 --workers 1 --duration 10s --config /private/tmp/gsbench-v117-extreme.cfg
```

Expected: zero recovery actions, no active pool/plan faults, and scenario 101 completes after extreme tests.

- [ ] **Step 8: Write and commit the exact report**

Record source commit, binary VCS metadata, environment/capability evidence, every command and run ID, requested/actual targets, expected/actual outcome, primary error, restore outcome, tagged-session cleanup, journal/ledger state, downstream proof, and any sandbox capability limitation. Exclude credentials and password-bearing DSNs.

```bash
git add docs/gsbench/V117_EXTREME_TEST_REPORT_20260807.md
git commit -m "test(gsbench): record v1.1.7 extreme validation"
```

---

### Task 8: Rebuild Local Deployment and Linux ARM64 Release Package

**Files:**
- Replace after backup: `/Users/sqlrush/gstop/gsbench-local/bin/gsbench`
- Update if needed: `/Users/sqlrush/gstop/gsbench-local/env.sh`
- Create: `/Users/sqlrush/gstop/release/gsbench-v1.1.7-linux-arm64-20260807/`
- Create: `/Users/sqlrush/gstop/release/gsbench-v1.1.7-linux-arm64-20260807.tar.gz`
- Create: `/Users/sqlrush/gstop/release/gsbench-v1.1.7-linux-arm64-20260807.tar.gz.sha256`

**Interfaces:**
- Consumes: final clean source commit and Task 7 report.
- Produces: locally deployed v1.1.7 and a traceable static Linux ARM64 archive.

- [ ] **Step 1: Verify a clean tree and rebuild from the final commit**

```bash
git status --porcelain=v1
go build -mod=readonly -trimpath -buildvcs=true -ldflags='-s -w' -o /private/tmp/gsbench-v1.1.7-final-darwin-arm64 ./cmd/gsbench
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -mod=readonly -trimpath -buildvcs=true -ldflags='-s -w' -o /private/tmp/gsbench-v1.1.7-final-linux-arm64 ./cmd/gsbench
```

Expected: clean tree and both binaries report the final `vcs.revision` with `vcs.modified=false`.

- [ ] **Step 2: Back up and install the host binary**

Preserve the deployed binary as `/Users/sqlrush/gstop/gsbench-local/bin/gsbench.pre-v1.1.7-unbounded-20260807`, after checking that exact path does not exist, then use `install -m 0755` to replace `gsbench-local/bin/gsbench`. Verify `version`, `scenarios`, and `help` output for `--percent` from the deployed path.

- [ ] **Step 3: Assemble and checksum the dated Linux package**

Copy the final Linux binary, current config, operator docs, final test report, and exact BUILD_INFO into the package directory. Generate package-internal `SHA256SUMS`, verify it, create the tar archive, and generate the archive `.sha256`.

- [ ] **Step 4: Verify an extracted archive**

Extract into the new explicit `/private/tmp/gsbench-v1.1.7-final-verify-20260807` directory, run `shasum -a 256 -c SHA256SUMS`, inspect `file bin/gsbench`, and confirm BUILD_INFO commit equals `go version -m` VCS revision.

- [ ] **Step 5: Check GitHub publication readiness**

```bash
gh auth status
git status -sb
git log --oneline main..HEAD
```

If authentication is valid, push the feature branch and create a Draft PR per the GitHub publication workflow. If the token remains invalid, do not bypass authentication; report `gh auth login -h github.com` while handing over the verified local/package artifacts.
