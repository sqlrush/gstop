# gsbench Resource Scenarios Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the safe SQL workload families for CPU, memory, I/O, client/distributed network, connection/thread pools, and maintenance with direct evidence and universal restore.

**Architecture:** Build small reusable workload and observer components, then register declarative factories for 101–103, 201–209, 301–305, 321–333, 401–405, and 801. Each scenario owns only its sessions and benchmark objects, while the foundation runner handles applicability, authorization, lifecycle, evidence envelopes, and restore escalation.

**Tech Stack:** Go 1.26.5, `database/sql`, existing worker/controller primitives, openGauss/GaussDB memory and performance views, distributed GaussDB stream plans, Go `testing`.

## Global Constraints

- Consume the interfaces from `2026-07-26-gsbench-fault-foundation.md`; do not reintroduce string registries or a second restore engine.
- SQL templates and success semantics are exactly those in design sections 8–11 and 15.
- 209 is risk B and never targets OS OOM; 331–333 and 405 are distributed-GaussDB-only.
- Session GUCs are reset or released by closing their exact tagged connection.
- A metric-unavailable fallback cannot return `SUCCESS`.
- The current workspace has no top-level `.git`; use test checkpoints and do not initialize a repository.

---

### Task 1: Reusable SQL Workload Definitions and Node-Aware Samplers

**Files:**
- Create: `internal/gsbench/resource_workloads.go`
- Create: `internal/gsbench/resource_workloads_test.go`
- Create: `internal/gsbench/resource_observer.go`
- Create: `internal/gsbench/resource_observer_test.go`
- Modify: `internal/gsbench/scenario_common.go`
- Modify: `internal/gsbench/workload_catalog.go`

**Interfaces:**
- Produces: `type SQLTemplate struct { Name, SQL string }`
- Produces: `func ResourceSQL(code ScenarioCode, schema string, env Environment) ([]SQLTemplate, error)`
- Produces: `type ResourceSample struct`
- Produces: `type ResourceObserver interface`
- Consumes later: all scenarios in this plan.

- [ ] **Step 1: Write template coverage tests**

```go
func TestResourceSQLCoversEveryPlannableResourceScenario(t *testing.T) {
	codes := []ScenarioCode{
		101, 102, 103, 201, 202, 203, 204, 205, 207, 208, 209,
		301, 302, 303, 304, 305, 321, 322, 331, 332, 333,
		401, 402, 403, 404, 405, 801,
	}
	for _, code := range codes {
		if _, err := ResourceSQL(code, "gsbench", distributedFixture()); err != nil {
			t.Errorf("code %d: %v", code, err)
		}
	}
}
```

Add assertions that 332 contains `redistribute`, 333 contains `broadcast`, 304
sets `work_mem`, and every interpolated identifier belongs to `gsbench`. Define
the small `templateText([]SQLTemplate) []string` helper in the test file.

- [ ] **Step 2: Run and confirm failure**

```bash
go test ./internal/gsbench -run 'ResourceSQL|ResourceObserver' -count=1
```

Expected: compile failure for the new helpers.

- [ ] **Step 3: Implement fixed templates**

Move TP/AP/memory/vacuum templates from scenario files into `ResourceSQL`. Add
exact templates for random reads, WAL writes, spill, client ingress/egress,
distributed gather/redistribute/broadcast, and pooler queries. Return no SQL for
206 because its DDL is emitted as typed actions.

- [ ] **Step 4: Define observer output**

```go
type ResourceSample struct {
	Node       string
	Role       NodeRole
	Shard      string
	Metrics    map[string]float64
	Available  map[string]bool
	WaitEvents map[string]int64
}

type ResourceObserver interface {
	Sample(context.Context, ScenarioCode, Environment, string) ([]ResourceSample, error)
}
```

Implement SQL selection by capability: local views for openGauss/centralized and
global node views for distributed GaussDB.

- [ ] **Step 5: Route plan preflight through code**

Replace `ScenarioWorkloadStatements(runtime, string)` with:

```go
func ScenarioWorkloadStatements(rt *Runtime, code ScenarioCode) ([]string, error)
```

- [ ] **Step 6: Run focused tests**

```bash
gofmt -w internal/gsbench/resource_workloads.go internal/gsbench/resource_workloads_test.go internal/gsbench/resource_observer.go internal/gsbench/resource_observer_test.go internal/gsbench/scenario_common.go internal/gsbench/workload_catalog.go
go test ./internal/gsbench -run 'ResourceSQL|ResourceObserver|WorkloadStatements' -count=1
```

### Task 2: CPU Scenarios 101–103

**Files:**
- Modify: `internal/gsbench/scenario_tp.go`
- Modify: `internal/gsbench/scenario_ap.go`
- Modify: `internal/gsbench/scenario_mixed.go`
- Replace: `internal/gsbench/scenario_cpu_test.go`
- Create: `internal/gsbench/register_resource.go`

**Interfaces:**
- Produces: `func ResourceScenarioFactories() map[ScenarioCode]ScenarioFactory`
- Produces: factories for 101, 102, 103.

- [ ] **Step 1: Write code-identity and evidence tests**

```go
func TestCPUFactoriesExposeApprovedCodes(t *testing.T) {
	f := ResourceScenarioFactories()
	for _, code := range []ScenarioCode{101, 102, 103} {
		if f[code] == nil {
			t.Fatalf("missing CPU code %d", code)
		}
	}
}

func TestTPRequiresMeasuredCPUForSuccess(t *testing.T) {
	got := verifyCPUResult(101, "tp_cpu", 95, false, ControlResult{Ceiling: true}, WorkerSnapshot{Operations: 10})
	if got.Outcome != OutcomeDegraded {
		t.Fatalf("result=%+v", got)
	}
}
```

- [ ] **Step 2: Run and observe signature failures**

```bash
go test ./internal/gsbench -run 'CPUFactories|TPRequiresMeasuredCPU' -count=1
```

- [ ] **Step 3: Convert scenarios to code identity**

Each scenario exposes `Code() ScenarioCode`; names come from the catalog. Keep the
existing TP/AP/mixed controllers, but make distributed AP preparation require a
multi-DN plan.

- [ ] **Step 4: Add restore assertions**

Test that Stop cancels SQL, rolls back open TP transactions, closes tagged
sessions, and VerifyRestore reports zero remaining sessions.

- [ ] **Step 5: Run CPU tests**

```bash
gofmt -w internal/gsbench/scenario_tp.go internal/gsbench/scenario_ap.go internal/gsbench/scenario_mixed.go internal/gsbench/scenario_cpu_test.go internal/gsbench/register_resource.go
go test ./internal/gsbench -run 'CPU|TP|AP|Mixed' -count=1
```

### Task 3: Memory Execution Scenarios 201–203

**Files:**
- Replace: `internal/gsbench/scenario_memory.go`
- Create: `internal/gsbench/scenario_memory_exec.go`
- Create: `internal/gsbench/scenario_memory_test.go`

**Interfaces:**
- Produces: `NewWorkMemSortScenario`, `NewWorkMemHashScenario`, `NewSharedBufferChurnScenario`.

- [ ] **Step 1: Write tests for distinct templates and success evidence**

```go
func TestMemoryExecutionFactoriesAreIndependent(t *testing.T) {
	f := ResourceScenarioFactories()
	for _, code := range []ScenarioCode{201, 202, 203} {
		if f[code] == nil {
			t.Fatalf("missing %d", code)
		}
	}
}

func TestSharedBufferChurnNeverChangesSharedBuffers(t *testing.T) {
	sql, _ := ResourceSQL(203, "gsbench", centralizedFixture())
	if strings.Contains(strings.Join(templateText(sql), "\n"), "SET shared_buffers") {
		t.Fatal("shared_buffers mutation found")
	}
}
```

- [ ] **Step 2: Run and confirm failure**

```bash
go test ./internal/gsbench -run 'MemoryExecution|SharedBufferChurn' -count=1
```

- [ ] **Step 3: Implement 201 and 202**

Use session-local `SET work_mem`, distinct Sort versus Hash SQL, gradual worker
ramp, and memory-node evidence. Stop executes `RESET work_mem` on the same
connection before close.

- [ ] **Step 4: Implement 203**

Plan a changing working set larger than the probed shared-memory size. Success
requires lower hit ratio plus physical read/I/O evidence; it never clears OS cache.

- [ ] **Step 5: Run tests**

```bash
gofmt -w internal/gsbench/scenario_memory.go internal/gsbench/scenario_memory_exec.go internal/gsbench/scenario_memory_test.go
go test ./internal/gsbench -run 'Memory|SharedBuffer' -count=1
```

### Task 4: Plan Cache, Session, Global Cache, Total, Retention, and Guarded OOM 204–209

**Files:**
- Create: `internal/gsbench/scenario_memory_cache.go`
- Create: `internal/gsbench/scenario_memory_cache_test.go`
- Create: `internal/gsbench/scenario_memory_guard.go`
- Create: `internal/gsbench/scenario_memory_guard_test.go`

**Interfaces:**
- Produces: factories for 204–209.
- Consumes: typed SQL/data actions and node-aware memory observer.

- [ ] **Step 1: Write registration and safety tests**

```go
func TestAdvancedMemoryFactoriesAndRisk(t *testing.T) {
	catalog := DefaultScenarioCatalog()
	for _, code := range []ScenarioCode{204, 205, 206, 207, 208, 209} {
		if ResourceScenarioFactories()[code] == nil {
			t.Fatalf("missing %d", code)
		}
	}
	if catalog.MustCode(209).Risk != RiskB {
		t.Fatal("209 is not risk B")
	}
}

func TestOOMGuardStopsBeforeHostReserve(t *testing.T) {
	if got := nextOOMWorkers(OOMGuard{MinimumFreeGB: 8}, HostMemory{FreeGB: 7}, 4); got != 0 {
		t.Fatalf("workers=%d", got)
	}
}
```

- [ ] **Step 2: Run and confirm failure**

```bash
go test ./internal/gsbench -run 'AdvancedMemory|OOMGuard' -count=1
```

- [ ] **Step 3: Implement 204 and 205**

204 generates bounded unique `PREPARE` names and records plan-cache bytes/entries;
205 opens bounded cursors in a transaction and records session-context bytes.
Restore uses `DEALLOCATE ALL`, `CLOSE ALL`, and `ROLLBACK` on the owning session.

- [ ] **Step 4: Implement 206**

Generate fixed-name `gcache_<run>_<n>` table/index actions through the typed
journal. Restore drops only successfully created objects in reverse order.

- [ ] **Step 5: Implement 207 and 208**

207 composes internal workloads 201, 202, 204, and 205 behind one total-memory
controller. 208 stops new allocation but holds sessions for an observation window,
then verifies database memory contexts release after close.

- [ ] **Step 6: Implement 209 guard**

Ramp work_mem workloads until database allocation rejection or target, but stop on
control-ping failure, host reserve, swap guard, profile cap, worker cap, or first
allocation error. OS OOM is a test failure.

- [ ] **Step 7: Run advanced memory tests**

```bash
gofmt -w internal/gsbench/scenario_memory_cache.go internal/gsbench/scenario_memory_cache_test.go internal/gsbench/scenario_memory_guard.go internal/gsbench/scenario_memory_guard_test.go
go test ./internal/gsbench -run 'Memory|PlanCache|SessionContext|OOM' -count=1
```

### Task 5: I/O and Safe Network Scenarios 301–333

**Files:**
- Create: `internal/gsbench/scenario_io.go`
- Create: `internal/gsbench/scenario_io_test.go`
- Create: `internal/gsbench/scenario_network.go`
- Create: `internal/gsbench/scenario_network_test.go`

**Interfaces:**
- Produces: factories for 301–305, 321–322, and 331–333.

- [ ] **Step 1: Write registration/topology tests**

```go
func TestIONetworkFactoriesCoverApprovedSafeCodes(t *testing.T) {
	for _, code := range []ScenarioCode{301,302,303,304,305,321,322,331,332,333} {
		if ResourceScenarioFactories()[code] == nil {
			t.Fatalf("missing %d", code)
		}
	}
}

func TestDistributedNetworkRequiresExpectedStream(t *testing.T) {
	if planContainsRequiredStream(332, "Seq Scan on fact_sales") {
		t.Fatal("332 accepted a plan without REDISTRIBUTE")
	}
	if !planContainsRequiredStream(332, "Streaming(type: REDISTRIBUTE)") {
		t.Fatal("332 rejected REDISTRIBUTE")
	}
}
```

- [ ] **Step 2: Run and confirm failure**

```bash
go test ./internal/gsbench -run 'IONetwork|RequiredStream' -count=1
```

- [ ] **Step 3: Implement 301–305**

Use distinct sequential read, random primary-key read, committed WAL update,
64kB spill, and privileged checkpoint strategies. 305 returns degraded if it only
observes a natural checkpoint.

- [ ] **Step 4: Implement 321–322**

321 fully consumes returned rows and counts client bytes. 322 sends PBE payload
from the client, journals run-specific rows, and deletes them during restore.

- [ ] **Step 5: Implement 331–333**

Run `EXPLAIN` before workload and reject missing GATHER/REDISTRIBUTE/BROADCAST.
Success includes stream bytes/time and node identity.

- [ ] **Step 6: Run tests**

```bash
gofmt -w internal/gsbench/scenario_io.go internal/gsbench/scenario_io_test.go internal/gsbench/scenario_network.go internal/gsbench/scenario_network_test.go
go test ./internal/gsbench -run 'IO|Network|Stream|Spill|Checkpoint' -count=1
```

### Task 6: Connection, Thread, Churn, Queue, and Pooler Scenarios 401–405

**Files:**
- Replace: `internal/gsbench/scenario_connections.go`
- Replace: `internal/gsbench/scenario_threads.go`
- Create: `internal/gsbench/scenario_pools_test.go`

**Interfaces:**
- Produces: factories for 401–405.

- [ ] **Step 1: Write registration and evidence tests**

Assert 401 reserves control capacity, 402/404 cannot succeed without a real thread
pool, 403 creates and closes a fresh connection per operation, and 405 requires a
pooler wait event.

- [ ] **Step 2: Run failing tests**

```bash
go test ./internal/gsbench -run 'Connection|Thread|Churn|Queue|Pooler' -count=1
```

- [ ] **Step 3: Implement 401 and 403**

Reuse bounded connection ownership; replace active `pg_sleep` with the plannable
CPU SQL from design. Track connection-create latency and rejection counts.

- [ ] **Step 4: Implement 402 and 404**

Use actual thread-pool worker counts and a bounded active query. Remove arbitrary
`restart_command` execution; any future restart uses the provider action model.

- [ ] **Step 5: Implement 405**

Open distributed queries through selected CNs and require `wait pooler get conn`
or version-mapped equivalent in evidence.

- [ ] **Step 6: Run pool tests**

```bash
gofmt -w internal/gsbench/scenario_connections.go internal/gsbench/scenario_threads.go internal/gsbench/scenario_pools_test.go
go test ./internal/gsbench -run 'Connection|Thread|Churn|Queue|Pooler' -count=1
```

### Task 7: Maintenance Scenario 801 and Resource Gate

**Files:**
- Replace: `internal/gsbench/scenario_vacuum.go`
- Replace: `internal/gsbench/scenario_vacuum_test.go`
- Modify: `internal/gsbench/register_resource.go`
- Modify: `configs/gsbench.cfg`
- Modify: `docs/gsbench/README.md`

**Interfaces:**
- Produces: factory for 801 and completed resource factory map.

- [ ] **Step 1: Write vacuum lifecycle tests**

Assert regular and analyze modes are risk A, FULL requires risk B, foreground
regression plus maintenance evidence is mandatory, and restore repairs benchmark
rows then runs bounded `VACUUM ANALYZE`.

- [ ] **Step 2: Run failing tests**

```bash
go test ./internal/gsbench -run 'Vacuum' -count=1
```

- [ ] **Step 3: Implement 801 and register all factories**

Register exactly:

```go
[]ScenarioCode{
	101,102,103, 201,202,203,204,205,206,207,208,209,
	301,302,303,304,305, 321,322,331,332,333,
	401,402,403,404,405, 801,
}
```

- [ ] **Step 4: Update example config and docs**

Add one section per scenario group and mark 209 disabled. Document direct evidence
and restore actions without marking 341–343 implemented.

- [ ] **Step 5: Run the resource gate**

```bash
gofmt -w internal/gsbench
go test ./internal/gsbench -run 'CPU|Memory|IO|Network|Connection|Thread|Pooler|Vacuum' -count=1
go test ./internal/gsbench -count=1
```

- [ ] **Step 6: Record the checkpoint**

Record the exact command output in the execution report. In a git-backed checkout:

```bash
git add internal/gsbench configs/gsbench.cfg docs/gsbench/README.md
git commit -m "feat(gsbench): add resource fault scenarios"
```
