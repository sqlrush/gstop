# gsbench 601 Unique Index and 501 NULL Scan Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace scenario 601 with a fixed-worker composite-unique-key lookup whose fault drops the index, fix scenario 501 lock evidence NULL scanning, and publish gsbench `v1.1.4`.

**Architecture:** Scenario 601 keeps the existing three-process phase controller and worker engine, but changes its candidates and mutation catalog to a dedicated canonical `UNIQUE (dist_key,lookup_key)` index. Lock evidence keeps its SQL and verification semantics while centralizing row decoding in a NULL-safe scanner using `sql.NullString`. The two Go changes are file-disjoint and can be implemented in parallel, then integrated for versioning and packaging.

**Tech Stack:** Go 1.22, `database/sql`, openGauss connector, Go tests, macOS local build, Linux ARM64 cross-build, Docker `og5` smoke test.

## Global Constraints

- Keep the existing CLI forms: `gsbench run 601 init|fault|recover`.
- Do not drop or alter the `plan_data` primary key.
- The 601 unique index must contain the distribution column and be exactly `UNIQUE (dist_key,lookup_key)`.
- Do not automatically execute DROP/CREATE of the large index on the 100GB primary schema during packaging verification.
- All production changes follow RED-GREEN TDD.
- Preserve unrelated user work and do not reset or rewrite the branch.
- Publish version `v1.1.4` for macOS local use and Linux ARM64 distribution.

---

### Task 1: Scenario 601 unique-key baseline and three-phase mutation

**Files:**
- Modify: `internal/gsbench/plan_dataset.go`
- Modify: `internal/gsbench/plan_definitions.go`
- Test: `internal/gsbench/plan_dataset_test.go`
- Test: `internal/gsbench/plan_definitions_test.go`
- Test: `internal/gsbench/plan_baseline_test.go`

**Interfaces:**
- Consumes: existing `planIndexDefinition`, `planIndexDDL`, `PlanScenarioDefinitions`, and `PlanMutationSet`.
- Produces: canonical `plan_data_lookup_idx` as a unique `(dist_key,lookup_key)` index; 601 point-query candidates; 601 DROP/CREATE mutation pair.

- [ ] **Step 1: Add failing canonical-index tests**

Update the expected first canonical definition to carry uniqueness and exact columns:

```go
{
    Name: "plan_data_lookup_idx", Table: "plan_data",
    Columns: []string{"dist_key", "lookup_key"}, Unique: true,
}
```

Assert `planIndexDDL` returns:

```sql
CREATE UNIQUE INDEX plan_data_lookup_idx ON "Bench".plan_data (dist_key,lookup_key)
```

- [ ] **Step 2: Add failing 601 definition and mutation tests**

Assert all 601 candidates select `id,payload`, contain equality predicates for both key columns, do not reference `stats_target_key` or `BETWEEN`, use rows present in the minimum one-million-row dataset, and set `ExpectedBaselineToken` to `plan_data_lookup_idx`.

Assert `PlanMutationSet(..., "planchange_stats_target")` has one mutation with forward SQL:

```sql
DROP INDEX "Bench".plan_data_lookup_idx
```

Its inverse and verification must use the exact canonical `CREATE UNIQUE INDEX` definition. Assert 605 remains tied to `plan_index_drop_idx`.

- [ ] **Step 3: Run focused tests and verify RED**

Run:

```bash
go test ./internal/gsbench -run 'Test(PlanIndexDefinitions|PlanIndexDDL|PlanScenarioDefinitions|PlanMutations|PlanMutation|PlanBaseline)' -count=1
```

Expected: FAIL because the index has no uniqueness metadata, 601 still uses `stats_target_key`, and its fault still changes statistics.

- [ ] **Step 4: Implement canonical unique-index DDL**

Add `Unique bool` to `planIndexDefinition`. Mark only `plan_data_lookup_idx` unique and change its columns to `dist_key,lookup_key`. Teach `planIndexDDL` to emit `CREATE UNIQUE INDEX` only when `Unique` is true and preserve all other index SQL.

- [ ] **Step 5: Implement 601 point lookups and DROP/CREATE mutation**

Build three literal candidates from keys within `1..1_000_000`. For each key `g`, calculate `dist_key = mod(g,1048576)+1` and emit:

```sql
SELECT id,payload FROM <schema>.plan_data
WHERE dist_key=<calculated> AND lookup_key=<g>
```

Set the baseline token to `plan_data_lookup_idx`. Replace only the 601 mutation case with the canonical-index DROP/inverse pattern already used by 605. Keep scenario code, name, CLI, and worker behavior unchanged.

- [ ] **Step 6: Run focused tests and verify GREEN**

Run the command from Step 3. Expected: PASS with no warnings.

- [ ] **Step 7: Format and check the task diff**

Run:

```bash
gofmt -w internal/gsbench/plan_dataset.go internal/gsbench/plan_definitions.go internal/gsbench/plan_dataset_test.go internal/gsbench/plan_definitions_test.go internal/gsbench/plan_baseline_test.go
git diff --check
```

Do not stage unrelated files.

---

### Task 2: Scenario 501 NULL-safe lock evidence scanning

**Files:**
- Modify: `internal/gsbench/lock_engine.go`
- Create: `internal/gsbench/lock_evidence_scan_test.go`

**Interfaces:**
- Consumes: `*sql.Rows`, `LockDefinition`, and existing row-chain object normalization.
- Produces: `scanLockEvidenceRows(*sql.Rows, LockDefinition) ([]LockEvidence, error)` used by `observeLockEvidence`.

- [ ] **Step 1: Add a real database/sql NULL regression test**

Use connector-backed test rows to return nine columns, with real `nil` driver values for node, relation, holder mode, waiter mode, blocker tag, and waiter tag. Include a `transactionid` row and:

```go
LockDefinition{ExpectedKind: "row_chain", Object: "lock_targets"}
```

Assert scanning succeeds, nullable strings become empty strings, wait age is preserved, and `Object` is normalized to `lock_targets`. Table-drive standalone and distributed labels because both query forms share the decoder.

- [ ] **Step 2: Run the regression test and verify RED**

Run:

```bash
go test ./internal/gsbench -run TestScanLockEvidenceRowsAcceptsNullableText -count=1
```

Expected: FAIL because the decoder is absent or current scanning reports `converting NULL to string is unsupported`.

- [ ] **Step 3: Implement the NULL-safe decoder**

Move the row loop from `observeLockEvidence` into `scanLockEvidenceRows`. Use `sql.NullString` for node, object, lock type, both modes, and both application tags. Scan `Granted` and wait seconds as their existing types, assign `.String` into `LockEvidence`, and preserve row-chain object replacement, duration conversion, and `rows.Err()` propagation.

- [ ] **Step 4: Run the regression test and verify GREEN**

Run the command from Step 2. Expected: PASS.

- [ ] **Step 5: Run existing lock-engine regression tests**

```bash
go test ./internal/gsbench -run 'Test(Lock|ConfiguredLock|ScanLockEvidence)' -count=1
```

Expected: PASS.

- [ ] **Step 6: Format and check the task diff**

```bash
gofmt -w internal/gsbench/lock_engine.go internal/gsbench/lock_evidence_scan_test.go
git diff --check
```

Do not stage unrelated files.

---

### Task 3: Integrate behavior, documentation, and version `v1.1.4`

**Files:**
- Modify: `internal/gsbench/cli.go`
- Modify: `internal/gsbench/cli_test.go`
- Modify: `docs/gsbench/PLAN_601_606_CN.md`
- Modify: `docs/gsbench/INSTALL.md`
- Modify: `docs/gsbench/CONFIG.md`

**Interfaces:**
- Consumes: Tasks 1 and 2 behavior.
- Produces: user-visible `v1.1.4` banner and accurate 601 instructions.

- [ ] **Step 1: Change the CLI version expectation first**

Change the banner test to expect `gsbench v1.1.4`, run it, and verify RED while production remains `v1.1.3`.

- [ ] **Step 2: Bump production version and update docs**

Set `Version = "v1.1.4"`. Update installation/configuration version references. In the plan manual describe the 601 composite unique-key lookup, DROP/CREATE SQL, and warn that first conversion and recovery may take time on a large dataset.

- [ ] **Step 3: Run focused checks**

```bash
go test ./internal/gsbench -run 'TestRunCLIPrintsBanner|TestParseCLIArgsSupportsPlanThreePhaseCommands' -count=1
rg -n 'v1\.1\.3|stats_target_key SET STATISTICS 1' docs/gsbench internal/gsbench/cli_test.go internal/gsbench/cli.go
```

Expected: tests PASS; search finds no stale active-version or 601 instruction except explicitly historical text.

- [ ] **Step 4: Commit the integrated source change**

Stage only the changed source, tests, and docs, then commit with:

```bash
git commit -m "feat(gsbench): rebuild 601 as unique-index plan fault"
```

---

### Task 4: Minimum verification and mandatory review gate

**Files:**
- Inspect: all files changed since `f4cbc44`

**Interfaces:**
- Consumes: clean integrated `v1.1.4` source.
- Produces: test/build evidence and resolution of blocker-only findings.

- [ ] **Step 1: Run focused and package-level tests**

```bash
go test ./internal/gsbench -run 'Test(ScanLockEvidence|Lock|Plan|RunCLI)' -count=1
go test ./internal/gsbench -count=1
go test ./cmd/gsbench -count=1
go vet ./internal/gsbench ./cmd/gsbench
```

All commands must exit zero.

- [ ] **Step 2: Inspect repository state**

```bash
git status --short --branch
git diff --check HEAD~1..HEAD
```

Confirm no unrelated files were included.

- [ ] **Step 3: Run only platform-mandated reviews**

Use one code-reviewer and one Go reviewer on the final diff. Do not add per-task review loops. Address only correctness, security, concurrency, or release-blocking findings, then rerun affected focused tests.

---

### Task 5: Build, package, deploy, and push `v1.1.4`

**Files:**
- Create: `/Users/sqlrush/gstop/release/gsbench-v1.1.4-linux-arm64-20260803/`
- Create: `/Users/sqlrush/gstop/release/gsbench-v1.1.4-linux-arm64-20260803.tar.gz`
- Update deployment: `/Users/sqlrush/gstop/gsbench-local/bin/gsbench`

**Interfaces:**
- Consumes: reviewed clean source commit.
- Produces: macOS local binary, Linux ARM64 archive, checksums, and pushed GitHub `main`.

- [ ] **Step 1: Build macOS and Linux ARM64 binaries**

```bash
go build -trimpath -buildvcs=true -o /tmp/gsbench-v1.1.4-darwin-arm64 ./cmd/gsbench
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -buildvcs=true -ldflags='-s -w' -o /tmp/gsbench-v1.1.4-linux-arm64 ./cmd/gsbench
```

- [ ] **Step 2: Assemble the release directory**

Create the explicit version directory under `/Users/sqlrush/gstop/release`, copy the Linux binary, sample configuration, install/config/plan manuals, and write `BUILD_INFO` plus `SHA256SUMS`. Reuse the `v1.1.3` package layout.

- [ ] **Step 3: Archive and verify checksums/architecture**

```bash
tar -C /Users/sqlrush/gstop/release -czf /Users/sqlrush/gstop/release/gsbench-v1.1.4-linux-arm64-20260803.tar.gz gsbench-v1.1.4-linux-arm64-20260803
shasum -a 256 /Users/sqlrush/gstop/release/gsbench-v1.1.4-linux-arm64-20260803.tar.gz
file /Users/sqlrush/gstop/release/gsbench-v1.1.4-linux-arm64-20260803/gsbench
tar -tzf /Users/sqlrush/gstop/release/gsbench-v1.1.4-linux-arm64-20260803.tar.gz
```

- [ ] **Step 4: Smoke-test the Linux binary in og5**

Copy only the binary to a temporary explicit path in `og5`; execute `version` and help/scenario parsing. Do not run 601 fault/recover against the 100GB schema.

- [ ] **Step 5: Deploy the macOS binary**

Back up the current local binary to an explicit `pre-v1.1.4-20260803` path, replace it with the tested Darwin binary, then source `/Users/sqlrush/gstop/gsbench-local/env.sh` and verify `gsbench version` plus 601 help.

- [ ] **Step 6: Push and verify remote main**

Push `main` to `origin`, compare local HEAD with `origin/main`, and report the commit, archive path, archive SHA-256, architecture, deployment path, and the intentionally skipped 100GB large-index DDL.
