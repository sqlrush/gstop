# gsbench 202 Hash Cursor Hint Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bind scenario 202 calibration and pressure cursors to the same openGauss Hash Join plan so each ready worker retains the calibrated Hash table.

**Architecture:** Add one query-scoped openGauss Plan Hint to both 202 SQL builders. Keep the existing transaction-local GUCs and worker lifecycle unchanged, and prove the fix with SQL-shape regression tests plus one bounded live run.

**Tech Stack:** Go, `database/sql`, openGauss Plan Hint syntax, existing gsbench SQL-builder and controlled-connector tests.

## Global Constraints

- Change only scenario 202 SQL shape and its focused tests/documentation.
- Use `/*+ leading((p h)) hashjoin(p h) set(enable_index_nestloop off) */` immediately after `SELECT`.
- Do not change scenario 201 SQL.
- Do not change worker count, `work_mem`, calibration, duration, cancellation, cleanup, or recovery behavior.
- Do not set any user-, database-, or instance-level parameter.

---

### Task 1: Bind the 202 SQL builders to Hash Join

**Files:**
- Modify: `internal/gsbench/scenario_workmem_test.go`
- Modify: `internal/gsbench/scenario_workmem.go`
- Modify: `docs/gsbench/README.md`

**Interfaces:**
- Consumes: `workMemCalibrationSQL(workMemKind, string, int64) (string, error)` and `workMemCursorSQL(workMemKind, string, int64) (string, error)`.
- Produces: both `workMemHash` builders return SQL containing the canonical query-level Hint; `workMemSort` output is unchanged.

- [x] **Step 1: Write the failing SQL-shape tests**

Add a focused test that calls both Hash builders and requires each SQL to contain exactly one:

```text
SELECT /*+ leading((p h)) hashjoin(p h) set(enable_index_nestloop off) */
```

Also call both Sort builders and fail if either contains `hashjoin(` or
`enable_index_nestloop`.

- [x] **Step 2: Run the focused test and verify RED**

Run:

```bash
go test ./internal/gsbench -run TestWorkMemHashSQLBindsCursorAndCalibrationToHashJoin -count=1
```

Expected: FAIL because the current Hash SQL starts with an unhinted `SELECT`.

- [x] **Step 3: Add the minimal canonical Hint**

Define:

```go
const workMemHashPlanHint = "/*+ leading((p h)) hashjoin(p h) set(enable_index_nestloop off) */ "
```

Insert `workMemHashPlanHint` immediately after `SELECT ` in the Hash branches of
`workMemCalibrationSQL` and `workMemCursorSQL`. Do not modify the Sort branches.

- [x] **Step 4: Run focused and package tests and verify GREEN**

Run:

```bash
gofmt -w internal/gsbench/scenario_workmem.go internal/gsbench/scenario_workmem_test.go
go test ./internal/gsbench -run 'TestWorkMem(HashSQLBindsCursorAndCalibrationToHashJoin|HashWorkerForcesOneHashStrategy)' -count=1
go test ./internal/gsbench -count=1
```

Expected: PASS.

- [x] **Step 5: Document the query-scoped binding**

Update the 201/202 README paragraph to state that 202 uses a query-level
`leading/hashjoin` Hint and disables only the hinted SQL's index-nestloop path, so the
formal cursor cannot drift away from the calibrated Hash plan.

- [x] **Step 6: Run minimum repository verification**

Run:

```bash
go test ./...
go vet ./...
```

Expected: PASS.

- [x] **Step 7: Build and perform a bounded live acceptance run**

Build a Darwin ARM64 candidate, verify `gsbench version`, then run scenario 202 with one
worker, `64MB`, and a short duration. While it is in hold, require the tagged worker's
statement-history plan to contain `Hash Left Join` and its session memory to contain a
non-trivial `HashBatchContext`. After completion, require the tagged session count to be
zero.

- [x] **Step 8: Commit the focused fix**

```bash
git add internal/gsbench/scenario_workmem.go internal/gsbench/scenario_workmem_test.go docs/gsbench/README.md docs/superpowers/specs/2026-08-08-gsbench-202-hash-cursor-hint-design.md docs/superpowers/plans/2026-08-08-gsbench-202-hash-cursor-hint.md
git commit -m "fix(gsbench): bind scenario 202 cursor to hash join"
```
