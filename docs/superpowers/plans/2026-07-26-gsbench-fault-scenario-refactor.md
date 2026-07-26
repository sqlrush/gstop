# gsbench Fault Scenario Refactor Implementation Program

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace gsbench's legacy 1–15 scenario model with 90 independently triggerable, verifiable, and recoverable three-digit fault scenarios for openGauss, centralized GaussDB, and distributed GaussDB.

**Architecture:** Deliver the redesign as four ordered, independently testable sub-projects. First build the catalog, topology/capability model, typed action journal, distributed dataset variants, and universal restore engine; then add resource scenarios, lock/plan scenarios, and finally replication/cluster plus infrastructure providers. Every later sub-project consumes the same catalog, evidence, action, and restore interfaces from the foundation plan.

**Tech Stack:** Go 1.26.5, `database/sql`, `gitcode.com/opengauss/openGauss-connector-go-pq`, existing INI configuration package, Linux `tc`/`nft` for opt-in self-managed fault injection, openGauss/GaussDB system views, Go `testing`.

**Design source:** `docs/superpowers/specs/2026-07-25-gsbench-fault-scenario-refactor-design.md`.

## Global Constraints

- Scenario codes are exactly three digits; the first digit is the category and the final two digits identify the scenario in that category.
- Support openGauss standalone/primary-standby, centralized GaussDB, and distributed GaussDB.
- Keep `601` through `606` named with the exact `planchange_` prefix and `621` through `626` with the exact `hardparse_` prefix.
- Replace `lock_storm` with independent lock scenarios; implement all 21 conflicting pairs in the eight-level table-lock matrix.
- Never accept arbitrary SQL, shell commands, business table names, node paths, interfaces, IPs, or ports from a scenario definition.
- Risk A is enabled by default; risk B and C require configuration authorization, while risk C also requires explicit CLI authorization and a recovery provider.
- `gsbench restore` without a run ID restores every active or pending run; `--run-id` selects exactly one run; `--dry-run` prints the recovery plan without mutation.
- Never intentionally trigger the operating-system OOM killer; scenario 209 stops at database memory rejection or the configured guard.
- True kernel memory leakage is not claimed; scenario 208 is named `memory_retention`.
- All persistent and infrastructure mutations use write-journal-before-mutate and have a typed inverse plus restore verification.
- Distributed evidence retains node, role, and shard identity.
- The current workspace has no top-level `.git`; use test checkpoints while executing here. Do not run `git init`. If execution moves to a git-backed checkout, make one commit at each sub-project gate.

---

## Sub-Project Plans and Order

### Task 1: Foundation, Catalog, Topology, Journal, and Universal Restore

**Plan:** `docs/superpowers/plans/2026-07-26-gsbench-fault-foundation.md`

**Produces:**

- canonical 90-code scenario catalog;
- product/topology/capability model;
- A/B/C authorization;
- openGauss/centralized/distributed dataset dialects;
- typed journal and local recovery ledger;
- provider interface;
- universal idempotent restore;
- runner outcome and applicability support.

**Gate:**

```bash
go test ./internal/gsbench -count=1
go test -race ./internal/gsbench -count=1
go vet ./internal/gsbench ./cmd/gsbench
```

Expected: all pass before any scenario-family plan begins.

### Task 2: CPU, Memory, I/O, Network Workloads, Pools, and Maintenance

**Plan:** `docs/superpowers/plans/2026-07-26-gsbench-resource-scenarios.md`

**Consumes:** `ScenarioDefinition`, `Environment`, `Requirement`, `EvidenceRecord`,
`Action`, `ActionJournal`, `FaultProvider`, and the universal runner/restore contracts
from Task 1.

**Produces:** 101–103, 201–209, 301–305, 321–333, 401–405, and 801.

**Gate:**

```bash
go test ./internal/gsbench -run 'CPU|Memory|IO|Network|Connection|Thread|Pooler|Vacuum' -count=1
go test ./internal/gsbench -count=1
```

### Task 3: Lock, Plan Change, and Hard Parse Scenarios

**Plan:** `docs/superpowers/plans/2026-07-26-gsbench-lock-plan-scenarios.md`

**Consumes:** the same foundation interfaces plus dataset objects created by Tasks 1
and 2.

**Produces:** 501–512, 520–540, 601–606, and 621–626.

**Gate:**

```bash
go test ./internal/gsbench -run 'Lock|Planchange|HardParse|PlanBaseline|Journal' -count=1
go test ./internal/gsbench -count=1
```

### Task 4: Replication, Distributed Cluster, and Infrastructure Fault Providers

**Plan:** `docs/superpowers/plans/2026-07-26-gsbench-cluster-faults.md`

**Consumes:** the provider, topology, action journal, local ledger, restore engine,
evidence envelope, distributed dataset, and resource workload primitives.

**Produces:** 341–343 infrastructure implementations, 701–706, and 721–728.

**Gate:**

```bash
go test ./internal/gsbench -run 'Replication|Cluster|Netem|Nft|Provider|Restore' -count=1
go test -race ./internal/gsbench -count=1
go test ./... -count=1
go vet ./...
```

### Task 5: Cross-Product Release Gate

**Files:**
- Modify: `internal/gsbench/integration_test.go`
- Modify: `scripts/gsbench-direct_test.sh`
- Modify: `docs/gsbench/README.md`
- Modify: `configs/gsbench.cfg`

**Interfaces:**
- Consumes: all four sub-project deliverables.
- Produces: a product/topology/scenario compatibility report and clean-restore acceptance log.

- [ ] **Step 1: Add a catalog completeness integration test**

Add:

```go
func TestCatalogContainsAllDesignedScenarios(t *testing.T) {
	got := DefaultScenarioCatalog().Codes()
	want := DesignedScenarioCodes()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("catalog codes=%v want=%v", got, want)
	}
}
```

- [ ] **Step 2: Run it before final registration**

Run:

```bash
go test ./internal/gsbench -run TestCatalogContainsAllDesignedScenarios -count=1
```

Expected: FAIL if any sub-project omitted or duplicated a code.

- [ ] **Step 3: Add the live matrix harness**

Extend `scripts/gsbench-direct_test.sh` so each configured environment runs:

```bash
./gsbench doctor -c "$GSBENCH_MATRIX_CONFIG"
./gsbench init --size 1GB -c "$GSBENCH_MATRIX_CONFIG"
./gsbench run "$GSBENCH_MATRIX_SMOKE_CODES" -d 30s -c "$GSBENCH_MATRIX_CONFIG"
./gsbench restore -c "$GSBENCH_MATRIX_CONFIG"
./gsbench status -c "$GSBENCH_MATRIX_CONFIG"
```

Require environment variables to provide the config path and smoke-code list; do
not embed passwords or endpoints.

- [ ] **Step 4: Add restore cleanliness assertions**

After each live run, query:

```sql
SELECT count(*)
FROM pg_stat_activity
WHERE application_name LIKE 'gsbench/%';

SELECT count(*)
FROM <S>.meta_journal
WHERE state IN ('planned','applied','restoring','restore_failed');
```

Expected: both counts are zero. Distributed runs additionally require provider
state empty and topology healthy.

- [ ] **Step 5: Run the full non-destructive gate**

Run:

```bash
gofmt -w internal/gsbench cmd/gsbench
go test ./... -count=1
go test -race ./internal/gsbench -count=1
go vet ./...
```

Expected: PASS with no test failures, races, or vet findings.

- [ ] **Step 6: Record the checkpoint**

Current non-git workspace: record the complete command output in the execution
report. In a git-backed checkout:

```bash
git add internal/gsbench cmd/gsbench configs/gsbench.cfg docs/gsbench/README.md scripts/gsbench-direct_test.sh
git commit -m "feat(gsbench): complete three-digit fault scenario refactor"
```

## Execution Rule

Do not execute Tasks 2–4 in parallel with Task 1. After Task 1, Tasks 2 and 3 may
be developed independently, but Task 4 must consume the finalized provider and
restore contracts. Run the cross-product release gate only after all four
sub-project plans pass their own gates.
