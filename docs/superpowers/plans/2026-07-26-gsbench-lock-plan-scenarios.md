# gsbench Lock and Plan Scenarios Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the combined lock workload and legacy plan names with independent lock, eight-mode conflict, plan-change, and hard-parse scenarios backed by direct database evidence and deterministic restore.

**Architecture:** Use one transaction-safe lock engine with declarative holder/waiter definitions and one plan/hard-parse observer with product-specific evidence queries. Preserve the existing declarative plan mutation foundation, but rename identities to 601–606 and route every persistent DDL/statistics change through typed actions. Generate the 21 lock-mode scenarios from the official conflict table.

**Tech Stack:** Go 1.26.5, `database/sql`, openGauss/GaussDB lock and statement-history views, existing plan parser/baseline tools, Go `testing`.

## Global Constraints

- Implement exactly 501–512, 520–540, 601–606, and 621–626.
- Preserve 501 `lock_row_chain`, 502 `lock_table_exclusive`, 503 `lock_ddl_wait`, and 504 `lock_deadlock`.
- 503 means DML blocks DDL; 505 is the reverse DDL blocks DML.
- Deadlock success requires the actual deadlock error/SQLSTATE and a cycle, not waiter count.
- 601–606 names use exact `planchange_` prefix.
- 602 merges the former duplicate index-unusable scenarios.
- No direct writes to system catalogs; no arbitrary SQL or object names.
- The current workspace has no top-level `.git`; use test checkpoints and do not initialize a repository.

---

### Task 1: Generic Lock Transaction Engine

**Files:**
- Replace: `internal/gsbench/scenario_locks.go`
- Replace: `internal/gsbench/scenario_locks_test.go`
- Create: `internal/gsbench/lock_engine.go`
- Create: `internal/gsbench/lock_engine_test.go`
- Create: `internal/gsbench/register_lock_plan.go`

**Interfaces:**
- Produces: `type LockRole struct`
- Produces: `type LockDefinition struct`
- Produces: `type LockEngine struct`
- Produces: `func NewLockScenario(LockDefinition) Scenario`
- Consumes later: all 5xx scenarios.

- [ ] **Step 1: Write transaction-order and cleanup tests**

```go
func TestLockEngineCancelsWaiterBeforeBlocker(t *testing.T) {
	events := []string{}
	engine := lockEngineFixture(&events)
	if err := engine.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"cancel_waiter", "rollback_waiter", "rollback_blocker"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v", events)
	}
}

func TestLockEngineRequiresGrantedFalseEvidence(t *testing.T) {
	def := LockDefinition{Code: 502, HolderMode: "AccessExclusive", WaiterMode: "AccessShare"}
	got := verifyLock(def, []LockEvidence{{Granted: true}})
	if got.Outcome == OutcomeSuccess {
		t.Fatal("granted waiter reported success")
	}
}
```

- [ ] **Step 2: Run and confirm old combined behavior fails**

```bash
go test ./internal/gsbench -run 'LockEngine' -count=1
```

- [ ] **Step 3: Define the engine**

```go
type LockDefinition struct {
	Code         ScenarioCode
	HolderSQL    []string
	WaiterSQL    []string
	HolderMode   string
	WaiterMode   string
	ExpectedKind string
	Deadlock     bool
}
```

The engine opens exact tagged connections, begins transactions, advances only
after observer evidence proves the holder lock, starts waiters, verifies the
expected wait, and owns rollback ordering.

- [ ] **Step 4: Add product-aware lock observer**

Use `pg_locks` locally and global lock/thread views in distributed GaussDB. Return
node, mode, object, granted, blocker tag, waiter tag, wait age, and chain edges.

- [ ] **Step 5: Run engine tests**

```bash
gofmt -w internal/gsbench/lock_engine.go internal/gsbench/lock_engine_test.go internal/gsbench/scenario_locks.go internal/gsbench/scenario_locks_test.go
go test ./internal/gsbench -run 'LockEngine|LockEvidence' -count=1
```

### Task 2: Base and Directional Business Locks 501–510

**Files:**
- Create: `internal/gsbench/lock_definitions.go`
- Create: `internal/gsbench/lock_definitions_test.go`
- Modify: `internal/gsbench/register_lock_plan.go`

**Interfaces:**
- Produces: `func BusinessLockDefinitions(schema, runID string) []LockDefinition`.

- [ ] **Step 1: Write definition coverage/direction tests**

```go
func TestBusinessLockDefinitionsPreserveDirections(t *testing.T) {
	defs := lockDefinitionsByCode(BusinessLockDefinitions("gsbench", "run-1"))
	wantNames := map[ScenarioCode]string{
		501:"lock_row_chain", 502:"lock_table_exclusive",
		503:"lock_ddl_wait", 504:"lock_deadlock",
		505:"lock_ddl_blocks_dml", 506:"lock_select_blocks_ddl",
		507:"lock_vacuum_blocks_ddl", 508:"lock_ddl_blocks_vacuum",
		509:"lock_createindex_blocks_dml", 510:"lock_dml_blocks_createindex",
	}
	for code, name := range wantNames {
		if defs[code].Name != name {
			t.Fatalf("code=%d got=%q want=%q", code, defs[code].Name, name)
		}
	}
	if !strings.Contains(strings.Join(defs[503].HolderSQL, "\n"), "UPDATE") ||
		!strings.Contains(strings.Join(defs[503].WaiterSQL, "\n"), "ALTER TABLE") {
		t.Fatalf("503=%+v", defs[503])
	}
	if !strings.Contains(strings.Join(defs[505].HolderSQL, "\n"), "ALTER TABLE") ||
		!strings.Contains(strings.Join(defs[505].WaiterSQL, "\n"), "UPDATE") {
		t.Fatalf("505=%+v", defs[505])
	}
}
```

- [ ] **Step 2: Run and confirm failure**

```bash
go test ./internal/gsbench -run 'BusinessLockDefinitions' -count=1
```

- [ ] **Step 3: Define 501–510 exactly**

Use design section 12 SQL for row chain, table exclusive, DML→DDL, deadlock,
DDL→DML, SELECT→DDL, VACUUM→DDL, DDL→VACUUM, CREATE INDEX→DML, and
DML→CREATE INDEX. Journal only DDL that can finish before cancellation.

- [ ] **Step 4: Implement deadlock classifier**

Recognize the product's deadlock SQLSTATE and error class; require two wait edges
forming a cycle before triggering the final statement.

- [ ] **Step 5: Run base lock tests**

```bash
gofmt -w internal/gsbench/lock_definitions.go internal/gsbench/lock_definitions_test.go internal/gsbench/register_lock_plan.go
go test ./internal/gsbench -run 'BusinessLock|Deadlock|DDL|Vacuum.*Lock|CreateIndex' -count=1
```

### Task 3: Distributed Lock Scenarios 511–512

**Files:**
- Create: `internal/gsbench/scenario_distributed_locks.go`
- Create: `internal/gsbench/scenario_distributed_locks_test.go`

**Interfaces:**
- Produces: factories for 511 and 512.

- [ ] **Step 1: Write applicability and node-evidence tests**

Assert both factories return `NOT_APPLICABLE` outside distributed GaussDB and
cannot succeed without node-tagged global evidence.

- [ ] **Step 2: Run failing tests**

```bash
go test ./internal/gsbench -run 'DistributedLock' -count=1
```

- [ ] **Step 3: Implement 511**

Run journaled CREATE INDEX actions against fixed `ddl_global_<n>` tables at a
bounded rate. Success requires `LockMgrLock` plus global DDL blocker/waiter
evidence; restore drops only current-run indexes.

- [ ] **Step 4: Implement 512**

Use precomputed dist-key-to-DN placement and two-shard transaction chains. Stop
rolls back leaves-to-root and verifies no tagged CN/DN transaction remains.

- [ ] **Step 5: Run distributed lock tests**

```bash
gofmt -w internal/gsbench/scenario_distributed_locks.go internal/gsbench/scenario_distributed_locks_test.go
go test ./internal/gsbench -run 'DistributedLock|LockMgr|CrossShardLock' -count=1
```

### Task 4: Eight-Level Lock Matrix 520–540

**Files:**
- Create: `internal/gsbench/lock_matrix.go`
- Create: `internal/gsbench/lock_matrix_test.go`
- Modify: `internal/gsbench/register_lock_plan.go`

**Interfaces:**
- Produces: `func TableLockMatrixDefinitions(schema string) []LockDefinition`.

- [ ] **Step 1: Encode the official conflict set in the test**

```go
var expectedLockMatrix = []struct {
	Code ScenarioCode
	Name, Holder, Waiter string
}{
	{520, "lockmode_accessshare_accessexclusive", "AS", "AX"},
	{521, "lockmode_rowshare_exclusive", "RS", "X"},
	{522, "lockmode_rowshare_accessexclusive", "RS", "AX"},
	{523, "lockmode_rowexclusive_share", "RX", "S"},
	{524, "lockmode_rowexclusive_sharerowexclusive", "RX", "SRX"},
	{525, "lockmode_rowexclusive_exclusive", "RX", "X"},
	{526, "lockmode_rowexclusive_accessexclusive", "RX", "AX"},
	{527, "lockmode_shareupdateexclusive_self", "SUE", "SUE"},
	{528, "lockmode_shareupdateexclusive_share", "SUE", "S"},
	{529, "lockmode_shareupdateexclusive_sharerowexclusive", "SUE", "SRX"},
	{530, "lockmode_shareupdateexclusive_exclusive", "SUE", "X"},
	{531, "lockmode_shareupdateexclusive_accessexclusive", "SUE", "AX"},
	{532, "lockmode_share_sharerowexclusive", "S", "SRX"},
	{533, "lockmode_share_exclusive", "S", "X"},
	{534, "lockmode_share_accessexclusive", "S", "AX"},
	{535, "lockmode_sharerowexclusive_self", "SRX", "SRX"},
	{536, "lockmode_sharerowexclusive_exclusive", "SRX", "X"},
	{537, "lockmode_sharerowexclusive_accessexclusive", "SRX", "AX"},
	{538, "lockmode_exclusive_self", "X", "X"},
	{539, "lockmode_exclusive_accessexclusive", "X", "AX"},
	{540, "lockmode_accessexclusive_self", "AX", "AX"},
}
```

Assert codes are contiguous 520–540, names match the design, and the generated
holder/waiter SQL uses full allowlisted mode names.

- [ ] **Step 2: Add non-conflict negative tests**

Enumerate every remaining unordered pair and assert the compatibility helper says
non-conflicting; the integration engine should acquire both locks immediately.

- [ ] **Step 3: Implement generated definitions**

Keep the matrix as typed mode constants. Never interpolate user mode text.

- [ ] **Step 4: Run matrix tests**

```bash
gofmt -w internal/gsbench/lock_matrix.go internal/gsbench/lock_matrix_test.go
go test ./internal/gsbench -run 'LockMatrix|NonConflict' -count=1
```

### Task 5: Rename and Register Plan Change 601–606

**Files:**
- Modify: `internal/gsbench/plan_definitions.go`
- Modify: `internal/gsbench/plan_definitions_test.go`
- Modify: `internal/gsbench/scenario_plan.go`
- Modify: `internal/gsbench/scenario_plan_test.go`
- Modify: `internal/gsbench/plan_baseline.go`
- Modify: `internal/gsbench/plan_baseline_test.go`

**Interfaces:**
- Produces: code-aware `PlanScenarioDefinition`.
- Produces: factories for 601–606.

- [ ] **Step 1: Write exact name/code tests**

```go
func TestPlanchangeDefinitionsUseApprovedIdentities(t *testing.T) {
	want := map[ScenarioCode]string{
		601:"planchange_stats_target", 602:"planchange_index_unusable",
		603:"planchange_stats_ndistinct", 604:"planchange_stats_extended",
		605:"planchange_index_drop", 606:"planchange_index_shape",
	}
	for _, def := range PlanScenarioDefinitions("gsbench") {
		if want[def.Code] != def.Name {
			t.Fatalf("definition=%+v", def)
		}
	}
}
```

- [ ] **Step 2: Run and observe old names**

```bash
go test ./internal/gsbench -run 'PlanchangeDefinitions' -count=1
```

- [ ] **Step 3: Add code/name fields and rename**

Preserve current candidate SQL and mutation semantics. Convert every `Mutation`
through `SQLAction`; 602 is the only unusable-index definition.

- [ ] **Step 4: Strengthen restore verification**

After inverse actions, require index/stat catalog baseline and baseline query plan.
Remove string-only plan-change guards in `app.go`; use catalog categories/codes.

- [ ] **Step 5: Run planchange tests**

```bash
gofmt -w internal/gsbench/plan_definitions.go internal/gsbench/plan_definitions_test.go internal/gsbench/scenario_plan.go internal/gsbench/scenario_plan_test.go internal/gsbench/plan_baseline.go internal/gsbench/plan_baseline_test.go
go test ./internal/gsbench -run 'Planchange|PlanBaseline|PlanScenario' -count=1
```

### Task 6: Hard-Parse Observer and Scenarios 621–626

**Files:**
- Create: `internal/gsbench/hardparse_observer.go`
- Create: `internal/gsbench/hardparse_observer_test.go`
- Create: `internal/gsbench/scenario_hardparse.go`
- Create: `internal/gsbench/scenario_hardparse_test.go`
- Modify: `internal/gsbench/register_lock_plan.go`

**Interfaces:**
- Produces: `type HardParseSample struct`
- Produces: `type HardParseObserver interface`
- Produces: factories for 621–626.

- [ ] **Step 1: Write observer delta tests**

```go
func TestHardParseDeltaUsesDirectCounters(t *testing.T) {
	before := HardParseSample{Hard: 10, Soft: 90, ParseUS: 1000, PlanUS: 800}
	after := HardParseSample{Hard: 90, Soft: 110, ParseUS: 9000, PlanUS: 7000}
	got := hardParseDelta(before, after)
	if got.Hard != 80 || got.Ratio != .8 {
		t.Fatalf("delta=%+v", got)
	}
}
```

- [ ] **Step 2: Write registration and SQL-shape tests**

Assert:

```go
map[ScenarioCode]string{
	621:"hardparse_literal_flood", 622:"hardparse_unprepared",
	623:"hardparse_force_custom", 624:"hardparse_session_churn",
	625:"hardparse_ddl_invalidation", 626:"hardparse_gpc_bypass",
}
```

Also assert 623 contains `force_custom_plan`, 626 contains `no_gpc`, and 625
journals only benchmark index DDL.

- [ ] **Step 3: Implement product-aware observation**

Use tagged SQL and direct `n_hard_parse`, `n_soft_parse`, `parse_time`, and
`plan_time`. Open a separate observer connection to `postgres` where distributed
GaussDB requires it; retain node name.

- [ ] **Step 4: Implement 621–624**

Use bounded numeric literal generation, simple-query repetition, forced custom
PBE, and connection churn exactly as the design. Each has an internal comparison
or baseline window.

- [ ] **Step 5: Implement 625–626**

625 alternates journaled create/drop of the fixed invalidation index and verifies
plan invalidation counters. 626 requires GPC and proves the tagged SQL is absent
from global plan cache while hard-parse/session-plan evidence increases.

- [ ] **Step 6: Run hard-parse tests**

```bash
gofmt -w internal/gsbench/hardparse_observer.go internal/gsbench/hardparse_observer_test.go internal/gsbench/scenario_hardparse.go internal/gsbench/scenario_hardparse_test.go
go test ./internal/gsbench -run 'HardParse|GPC|CustomPlan|Invalidation' -count=1
```

### Task 7: Lock/Plan Documentation and Gate

**Files:**
- Modify: `configs/gsbench.cfg`
- Modify: `docs/gsbench/README.md`
- Modify: `internal/gsbench/register_lock_plan.go`

**Interfaces:**
- Produces: completed lock/plan factory map.

- [ ] **Step 1: Assert exact factory coverage**

In the test, define a local
`rangeCodes(first, last ScenarioCode) []ScenarioCode` helper and require:

```go
append(
	append(rangeCodes(501,512), rangeCodes(520,540)...),
	append(rangeCodes(601,606), rangeCodes(621,626)...)...,
)
```

- [ ] **Step 2: Update config/docs**

Document independent lock sections, matrix listing, planchange/hardparse sections,
direct success evidence, and universal restore.

- [ ] **Step 3: Run the gate**

```bash
gofmt -w internal/gsbench
go test ./internal/gsbench -run 'Lock|Planchange|HardParse|PlanBaseline|Journal' -count=1
go test ./internal/gsbench -count=1
```

- [ ] **Step 4: Record the checkpoint**

Record the exact command output in the execution report. Git-backed checkout:

```bash
git add internal/gsbench configs/gsbench.cfg docs/gsbench/README.md
git commit -m "feat(gsbench): add lock planchange and hardparse scenarios"
```
