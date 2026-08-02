# gsbench 501–503 Configurable Lock Sessions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `--sessions` to scenarios 501–503 and `--chain-depth` to 501 so gsbench creates exactly the requested holder/waiter topology and reports matching runtime evidence.

**Architecture:** Add typed lock workload settings to the existing CLI/config override path, then compile those settings into a deterministic `LockDefinition` topology before `LockEngine` opens database sessions. Keep scenarios 504+ on their existing engine path; 501–503 use a new multi-waiter path with exact tagged roles, reverse-order cleanup, and topology-aware evidence.

**Tech Stack:** Go 1.23, standard `flag`, `database/sql`, openGauss connector, existing gsbench scenario/runner framework, Go `testing`.

## Global Constraints

- `--sessions N` counts one workload holder plus `N-1` workload waiters; control, metadata, and observer connections are excluded.
- All 501–503 defaults are `sessions=2`; 501 defaults to `chain_depth=1`.
- 501 chain depth is an integer from 1 through 5 and requires `sessions >= chain_depth + 1`.
- 502 and 503 never receive an artificial chain depth.
- Selected fixed-worker connections and selected 501–503 workload sessions must cumulatively fit `safety.max_connections`; lock sessions do not count toward `safety.max_workers`.
- `run.validation_enabled=false` remains the default and disables final model verdicts, but failure to create requested sessions or lock waits remains an execution error.
- One Ctrl+C retains the current operating-system SIGINT behavior and immediately terminates the process.
- Scenarios 504 and later must retain their current SQL and lifecycle behavior.
- Do not add dependencies or inject raw CLI text into SQL.

## File Map

- `internal/gsbench/cli.go`: parse and document the two new CLI flags.
- `internal/gsbench/config.go`: own typed defaults, CLI overrides, compatibility checks, and connection budgets.
- `internal/gsbench/app.go`: pass CLI lock overrides into config loading.
- `internal/gsbench/lock_topology.go`: compile session/depth settings into waiter roles and expected edges.
- `internal/gsbench/lock_engine.go`: open, observe, cancel, and close multiple configured waiters.
- `internal/gsbench/lock_definitions.go`: retain base SQL definitions and generate unique 503 identifiers.
- `internal/gsbench/scenario_locks.go`: apply settings before constructing `LockEngine` and expose runtime evidence.
- `internal/gsbench/workload_catalog.go`: provide explainable 501–503 preflight statements.
- `configs/gsbench.cfg`, `docs/gsbench/CONFIG.md`: publish defaults and examples.

---

### Task 1: CLI and typed configuration

**Files:**
- Modify: `internal/gsbench/cli.go:30-255,403-441`
- Modify: `internal/gsbench/cli_test.go:237-427`
- Modify: `internal/gsbench/config.go:21-112,191-290,324-590`
- Modify: `internal/gsbench/config_test.go:148-425`
- Modify: `internal/gsbench/app.go:51-64`

**Interfaces:**
- Produces: `LockWorkloadConfig`, `LockWorkloadConfig.For(ScenarioCode) (int, int, bool)`, `validateLockOverrideCompatibility([]ScenarioCode, int, int) error`, and `applyLockOverrides(*BenchConfig, Overrides) error`.
- Produces: integer `Sessions` and `ChainDepth` fields in `CLIOptions` and `Overrides`; zero means “not explicitly overridden”.

- [ ] **Step 1: Write the failing CLI tests**

Add success, rejection, and help coverage:

```go
func TestParseCLIArgsSupportsLockWorkloadOverrides(t *testing.T) {
	for _, test := range []struct {
		args []string
		sessions, chainDepth int
	}{
		{[]string{"run", "501", "--sessions=10", "--chain-depth=3"}, 10, 3},
		{[]string{"run", "502", "--sessions=8"}, 8, 0},
		{[]string{"run", "501,503", "--sessions=6", "--chain-depth=2"}, 6, 2},
	} {
		options, err := ParseCLIArgs(test.args)
		if err != nil { t.Fatal(err) }
		if options.Sessions != test.sessions || options.ChainDepth != test.chainDepth {
			t.Fatalf("options=%+v", options)
		}
	}
}

func TestParseCLIArgsRejectsInvalidLockWorkloadOverrides(t *testing.T) {
	for _, args := range [][]string{
		{"run", "501", "--sessions=1"},
		{"run", "501", "--sessions=2", "--chain-depth=2"},
		{"run", "501", "--chain-depth=0"},
		{"run", "501", "--chain-depth=6"},
		{"run", "502", "--chain-depth=2"},
		{"run", "101", "--sessions=2"},
		{"doctor", "--sessions=2"},
	} {
		if _, err := ParseCLIArgs(args); err == nil {
			t.Fatalf("accepted invalid lock override %v", args)
		}
	}
}
```

Extend help assertions with `--sessions N`, `--chain-depth N`, and 501–503 examples.

- [ ] **Step 2: Run CLI tests and confirm the red state**

Run:

```bash
go test ./internal/gsbench -run 'TestParseCLIArgs(Supports|RejectsInvalid)LockWorkloadOverrides|TestCLIHelpDocumentsLockWorkloadParameters' -count=1
```

Expected: FAIL because the fields and flags do not exist.

- [ ] **Step 3: Write the failing config and propagation tests**

Add default and override assertions:

```go
wantLocks := LockWorkloadConfig{
	RowChainSessions: 2, RowChainDepth: 1,
	TableExclusiveSessions: 2, DDLWaitSessions: 2,
}
if cfg.LockWorkloads != wantLocks {
	t.Fatalf("lock workloads=%+v want=%+v", cfg.LockWorkloads, wantLocks)
}

overrides := configOverridesFromCLI(CLIOptions{
	ScenarioCodes: []ScenarioCode{501}, Sessions: 10, ChainDepth: 3,
})
if overrides.Sessions != 10 || overrides.ChainDepth != 3 {
	t.Fatalf("overrides=%+v", overrides)
}
```

Add cases for per-scenario values, final-scenario override validation, `sessions < depth+1`, depth outside 1–5, and selected 501+502 sessions exceeding `safety.max_connections`.

- [ ] **Step 4: Run config tests and confirm the red state**

Run:

```bash
go test ./internal/gsbench -run 'TestConfig(LoadsDefaultsAndDurations|LoadsLockWorkloadScenarioSettings|LockOverridesFollowFinalScenarios|RejectsInvalidLockWorkloadSettings|ValidatesLockSessionTotals)|TestCLIConfigOverridesPropagateLockTuning' -count=1
```

Expected: FAIL because typed lock settings and override fields do not exist.

- [ ] **Step 5: Implement CLI parsing and compatibility checks**

Register both flags and use `flags.Visit` to distinguish explicit zero from absence:

```go
flags.IntVar(&options.Sessions, "sessions", 0, "total holder plus waiter sessions for scenarios 501-503")
flags.IntVar(&options.ChainDepth, "chain-depth", 0, "row wait chain depth for scenario 501 (1-5)")
```

After `flags.Visit`, reject explicit invalid bounds before treating zero as an unset override:

```go
if sessionsSet && options.Sessions < 2 {
	return CLIOptions{}, fmt.Errorf("--sessions must be at least 2")
}
if chainDepthSet && (options.ChainDepth < 1 || options.ChainDepth > 5) {
	return CLIOptions{}, fmt.Errorf("--chain-depth must be between 1 and 5")
}
```

Implement and call this after CLI scenario resolution and after final config scenario resolution:

```go
func validateLockOverrideCompatibility(codes []ScenarioCode, sessions, chainDepth int) error {
	if sessions == 0 && chainDepth == 0 { return nil }
	if sessions < 0 || chainDepth < 0 {
		return fmt.Errorf("lock session counts and chain depth must be positive")
	}
	has501 := false
	for _, code := range codes {
		if code < 501 || code > 503 {
			return fmt.Errorf("--sessions/--chain-depth require only scenarios 501-503")
		}
		has501 = has501 || code == 501
	}
	if sessions > 0 && sessions < 2 { return fmt.Errorf("--sessions must be at least 2") }
	if chainDepth > 5 { return fmt.Errorf("--chain-depth must be between 1 and 5") }
	if chainDepth > 0 && !has501 { return fmt.Errorf("--chain-depth requires scenario 501") }
	if sessions > 0 && chainDepth > 0 && sessions < chainDepth+1 {
		return fmt.Errorf("scenario 501 requires sessions >= chain_depth + 1")
	}
	return nil
}
```

- [ ] **Step 6: Implement typed config, overrides, and budgets**

Add:

```go
type LockWorkloadConfig struct {
	RowChainSessions       int
	RowChainDepth          int
	TableExclusiveSessions int
	DDLWaitSessions        int
}

func (c LockWorkloadConfig) For(code ScenarioCode) (sessions, depth int, ok bool) {
	switch code {
	case 501: return c.RowChainSessions, c.RowChainDepth, true
	case 502: return c.TableExclusiveSessions, 1, true
	case 503: return c.DDLWaitSessions, 1, true
	default: return 0, 0, false
	}
}
```

Load `scenario.lock_row_chain.sessions/chain_depth`, `scenario.lock_table_exclusive.sessions`, and `scenario.lock_ddl_wait.sessions`. Apply a CLI session value to each selected 501–503 and depth only to 501.

Keep separate selected worker and connection counters:

```go
selectedWorkers := 0
selectedConnections := 0
addLockSessions := func(sessions int) error {
	if sessions > c.Safety.MaxConnections-selectedConnections {
		return fmt.Errorf("selected workload sessions exceed safety.max_connections %d", c.Safety.MaxConnections)
	}
	selectedConnections += sessions
	return nil
}
```

Fixed workers increment both counters. Exclude 501–503 from the existing `unbudgetedSelected` marker so fixed workers can coexist with these budgeted lock sessions without changing 501+504 compatibility.

- [ ] **Step 7: Run focused tests and commit**

Run:

```bash
gofmt -w internal/gsbench/cli.go internal/gsbench/cli_test.go internal/gsbench/config.go internal/gsbench/config_test.go internal/gsbench/app.go
go test ./internal/gsbench -run 'LockWorkload|LockSession|LockTuning|CLIHelpDocumentsLock' -count=1
```

Expected: PASS.

Commit:

```bash
git add internal/gsbench/cli.go internal/gsbench/cli_test.go internal/gsbench/config.go internal/gsbench/config_test.go internal/gsbench/app.go
git commit -m "feat(gsbench): configure lock workload sessions"
```

---

### Task 2: Deterministic 501–503 topology compiler

**Files:**
- Create: `internal/gsbench/lock_topology.go`
- Create: `internal/gsbench/lock_topology_test.go`
- Modify: `internal/gsbench/lock_engine.go:21-53`
- Modify: `internal/gsbench/lock_definitions.go:9-67`
- Modify: `internal/gsbench/lock_definitions_test.go:8-100`
- Modify: `internal/gsbench/scenario_locks.go:21-31`
- Modify: `internal/gsbench/scenario_locks_test.go:1-12`

**Interfaces:**
- Consumes: `BenchConfig.LockWorkloads` and `LockWorkloadConfig.For` from Task 1.
- Produces: `LockWaiterRole`, `LockExpectedEdge`, and `configureLockDefinition(LockDefinition, LockWorkloadConfig, string, string) (LockDefinition, error)`.
- Produces these `LockDefinition` fields for 501–503: `Waiters`, `ExpectedEdges`, `RequestedSessions`, `RequestedChainDepth`, and `BranchLengths`.

- [ ] **Step 1: Write failing topology tests**

Create pure tests for exact branch allocation and SQL:

```go
func TestConfigureRowChainDefinitionBuildsSharedRootBranches(t *testing.T) {
	base := lockDefinitionsByCode(BusinessLockDefinitions("gsbench", "run-1"))[501]
	configured, err := configureLockDefinition(base, LockWorkloadConfig{
		RowChainSessions: 8, RowChainDepth: 3,
	}, "gsbench", "run-1")
	if err != nil { t.Fatal(err) }
	if !reflect.DeepEqual(configured.BranchLengths, []int{3, 3, 1}) {
		t.Fatalf("branches=%v", configured.BranchLengths)
	}
	if len(configured.Waiters) != 7 || len(configured.ExpectedEdges) != 7 {
		t.Fatalf("waiters=%d edges=%d", len(configured.Waiters), len(configured.ExpectedEdges))
	}
	if configured.ExpectedEdges[0].BlockerTag != "blocker" ||
		configured.ExpectedEdges[3].BlockerTag != "blocker" {
		t.Fatalf("edges=%+v", configured.ExpectedEdges)
	}
}
```

Also assert 10/3 produces `[3,3,3]`, every 501 waiter owns a unique row before waiting upstream, 502 creates `sessions-1` SELECT roles, and 503 creates `sessions-1` distinct safe `ddl_<token>_<index>` column names.

- [ ] **Step 2: Run topology tests and confirm the red state**

Run:

```bash
go test ./internal/gsbench -run 'TestConfigure(RowChain|TableExclusive|DDLWait)Definition' -count=1
```

Expected: FAIL because the compiler types and function do not exist.

- [ ] **Step 3: Add topology types and deterministic builder**

Add:

```go
type LockWaiterRole struct {
	Tag           string
	SetupSQL      []string
	WaitSQL       []string
	Transactional bool
	BlockerTag    string
	Branch        int
	Depth         int
}

type LockExpectedEdge struct {
	BlockerTag string
	WaiterTag  string
	Branch     int
	Depth      int
}
```

For 501, allocate branches sequentially. The single holder transaction receives one distinct root row per branch; waiter rows follow that branch root. Within each branch, level 1 waits on its root row and every deeper level waits on the previous waiter-owned row. Use tag `chain-<branch>-<level>`. This avoids openGauss serializing multiple first-level UPDATE requests behind one tuple lock on a shared row while preserving one shared holder session.

For 502 create tags `waiter-1` through `waiter-(sessions-1)` with copied SELECT SQL. For 503 create the same tags with:

```go
column := fmt.Sprintf("ddl_%s_%d", lockRunToken(runID), index+1)
waitSQL := []string{addColumnSQL(schema, "lock_ddl_targets", column)}
```

Return errors for unsupported codes, unsafe schema/run IDs, invalid sessions/depth, and a 501 topology where `(sessions-1)+ceil((sessions-1)/depth)` exceeds the 10,000 fixed `lock_targets` rows.

- [ ] **Step 4: Apply the topology during scenario Prepare**

After `lockDefinitionForCode`, configure only 501–503:

```go
if s.definition.Code >= 501 && s.definition.Code <= 503 {
	configured, err := configureLockDefinition(
		definition, rt.Config.LockWorkloads, rt.Config.Data.Schema, rt.RunID,
	)
	if err != nil {
		return fmt.Errorf("configure lock workload: %w", err)
	}
	definition = configured
}
```

Keep 504+ definitions unchanged. Remove the fixed `ChainRows`/`ChainTags` contract after Tasks 3–4 migrate every reference.

- [ ] **Step 5: Run topology/scenario tests and commit**

Run:

```bash
gofmt -w internal/gsbench/lock_topology.go internal/gsbench/lock_topology_test.go internal/gsbench/lock_engine.go internal/gsbench/lock_definitions.go internal/gsbench/lock_definitions_test.go internal/gsbench/scenario_locks.go internal/gsbench/scenario_locks_test.go
go test ./internal/gsbench -run 'TestConfigure(RowChain|TableExclusive|DDLWait)Definition|TestBusinessLockDefinitions|TestNewLockScenario' -count=1
```

Expected: PASS.

Commit:

```bash
git add internal/gsbench/lock_topology.go internal/gsbench/lock_topology_test.go internal/gsbench/lock_engine.go internal/gsbench/lock_definitions.go internal/gsbench/lock_definitions_test.go internal/gsbench/scenario_locks.go internal/gsbench/scenario_locks_test.go
git commit -m "feat(gsbench): compile configurable lock topologies"
```

---

### Task 3: Multi-waiter execution and reverse cleanup

**Files:**
- Modify: `internal/gsbench/lock_engine.go:55-380`
- Modify: `internal/gsbench/lock_engine_test.go:1-223`

**Interfaces:**
- Consumes: `LockDefinition.Waiters` from Task 2.
- Produces: `lockWaiterSession`, `openConfiguredLockWaiter`, and `LockEngine.rampConfiguredWaiters`.
- Preserves: `rampDeadlock` and the legacy single-waiter path for scenarios 504+.

- [ ] **Step 1: Write a failing exact-session lifecycle test**

Add an injected opener and blocking executor. Return all expected edges from the observer and assert every compiled role starts:

```go
type blockingLockExecutor struct{ started chan string }

func (e blockingLockExecutor) ExecContext(ctx context.Context, query string, _ ...any) (sql.Result, error) {
	e.started <- query
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestLockEngineStartsEveryConfiguredWaiter(t *testing.T) {
	definition := configuredRowChainForTest(t, 8, 3)
	engine := NewLockEngine(definition)
	opened := []string{}
	engine.openConfiguredWaiter = func(_ context.Context, _ *Runtime, role LockWaiterRole) (*lockWaiterSession, error) {
		opened = append(opened, role.Tag)
		return newBlockingWaiterSessionForTest(role), nil
	}
	engine.observe = expectedEvidenceObserver(definition)
	if err := engine.rampConfiguredWaiters(context.Background(), lockRuntimeForTest()); err != nil {
		t.Fatal(err)
	}
	if len(opened) != 7 { t.Fatalf("opened=%v", opened) }
	if err := engine.Stop(context.Background(), nil); err != nil { t.Fatal(err) }
}
```

Add a second test where opening waiter 4 fails and assert the first three waiter transactions roll back and connections close in reverse creation order before holder rollback.

- [ ] **Step 2: Run lifecycle tests and confirm the red state**

Run:

```bash
go test ./internal/gsbench -run 'TestLockEngine(StartsEveryConfiguredWaiter|CleansPartialConfiguredWaitersInReverseOrder)' -count=1
```

Expected: FAIL because the multi-waiter session path does not exist.

- [ ] **Step 3: Implement configured waiter opening and execution**

Add:

```go
type lockSQLExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

type lockWaiterSession struct {
	role     LockWaiterRole
	conn     lockClose
	tx       lockRollback
	executor lockSQLExecutor
}
```

`openConfiguredLockWaiter` opens the exact role tag, begins a transaction, and returns the tagged connection plus transaction executor. `Ramp` selects `rampConfiguredWaiters` when `len(definition.Waiters)>0`; otherwise it keeps the existing paths for 504+.

`rampConfiguredWaiters` creates one shared cancellable context, opens roles in definition order, synchronously executes `SetupSQL`, then starts `WaitSQL` in one goroutine per role. Prefix errors with `waiter <tag> open`, `waiter <tag> setup`, or `waiter <tag> wait`.

- [ ] **Step 4: Implement reverse cleanup and error accumulation**

After cancelling and joining waiter goroutines, clean configured sessions before the holder:

```go
for index := len(e.configuredWaiters) - 1; index >= 0; index-- {
	session := e.configuredWaiters[index]
	errs = append(errs, rollbackLock(session.tx))
	errs = append(errs, session.conn.Close())
}
```

Change `setWaiterError` to `errors.Join(e.waiterErr, err)` so simultaneous failures are retained. Ignore context cancellation caused by normal Stop as expected control flow.

- [ ] **Step 5: Run lifecycle and legacy lock tests and commit**

Run:

```bash
gofmt -w internal/gsbench/lock_engine.go internal/gsbench/lock_engine_test.go
go test ./internal/gsbench -run 'TestLockEngine|TestDeadlock|TestLockRunnerLifecycle' -count=1
```

Expected: PASS, including existing 504 deadlock behavior.

Commit:

```bash
git add internal/gsbench/lock_engine.go internal/gsbench/lock_engine_test.go
git commit -m "feat(gsbench): run configurable lock waiter sessions"
```

---

### Task 4: Topology-aware evidence and validation-enabled preflight

**Files:**
- Modify: `internal/gsbench/lock_engine.go:420-637`
- Modify: `internal/gsbench/lock_engine_test.go:23-150`
- Modify: `internal/gsbench/scenario_locks.go:41-52`
- Modify: `internal/gsbench/workload_catalog.go:7-87`
- Modify: `internal/gsbench/plan_cache_test.go:213-236`

**Interfaces:**
- Consumes: `LockDefinition.ExpectedEdges`, `Waiters`, `RequestedSessions`, `RequestedChainDepth`, and `BranchLengths`.
- Produces: `LockEngine.RuntimeEvidence() []Evidence` and `LockScenario.RuntimeEvidence() []Evidence`.

- [ ] **Step 1: Write failing evidence cardinality tests**

Replace fixed 501 thresholds with configured cases: 8/3 requires seven exact edges and six edges fail. For 502/503, duplicate one waiter observation and require all unique configured tags:

```go
func TestConfiguredLockEvidenceCountsUniqueWaiterTags(t *testing.T) {
	definition := configuredDDLWaitForTest(t, 4)
	evidence := []LockEvidence{
		configuredEvidence(definition, "waiter-1"),
		configuredEvidence(definition, "waiter-1"),
		configuredEvidence(definition, "waiter-2"),
	}
	if got := verifyLock(definition, evidence); got.Outcome == OutcomeSuccess {
		t.Fatalf("duplicate waiter satisfied target: %+v", got)
	}
	evidence = append(evidence, configuredEvidence(definition, "waiter-3"))
	if got := verifyLock(definition, evidence); got.Outcome != OutcomeSuccess {
		t.Fatalf("result=%+v", got)
	}
}
```

Assert runtime evidence contains `lock_sessions`, `lock_waiters`, `row_lock_chain_edges`, and `chain_depth` when final validation is disabled.

- [ ] **Step 2: Run evidence tests and confirm the red state**

Run:

```bash
go test ./internal/gsbench -run 'TestConfiguredLockEvidence|TestLockScenarioRuntimeEvidence' -count=1
```

Expected: FAIL because verification still uses one waiter or one flat three-tag chain, and lock scenarios do not implement runtime evidence.

- [ ] **Step 3: Implement exact tag matching and cardinality-aware verification**

Prevent `waiter-1` from matching `waiter-10`:

```go
func lockRoleMatches(applicationName, role string) bool {
	return applicationName == role || strings.HasSuffix(applicationName, "/"+role)
}
```

For 501 retain only explicit `ExpectedEdges`, count unique pairs, and compute actual maximum depth from matched edges. For 502/503 count unique configured waiter tags with an ungranted expected lock and a holder application ending in `/blocker`. Derive every target from `RequestedSessions-1`.

Expose evidence even when validation is disabled:

```go
func (e *LockEngine) RuntimeEvidence() []Evidence {
	e.mu.Lock()
	evidence := append([]LockEvidence(nil), e.evidence...)
	e.mu.Unlock()
	return verifyLock(e.definition, evidence).Evidence
}

func (s *LockScenario) RuntimeEvidence() []Evidence {
	if s.engine == nil { return nil }
	return s.engine.RuntimeEvidence()
}
```

Use an evidence readiness timeout capped at five seconds and shortened by a smaller positive `safety.query_timeout`; leave 504 deadlock timing unchanged.

- [ ] **Step 4: Write failing 501–503 workload catalog tests**

Extend the registered workload test with 501–503 and reject non-explainable commands:

```go
for _, code := range []ScenarioCode{501, 502, 503} {
	definition := DefaultScenarioCatalog().MustCode(code)
	sqls, err := ScenarioWorkloadStatements(runtime, definition.Name)
	if err != nil || len(sqls) == 0 {
		t.Fatalf("code=%d sqls=%v err=%v", code, sqls, err)
	}
	for _, sqlText := range sqls {
		upper := strings.ToUpper(sqlText)
		if strings.HasPrefix(upper, "LOCK ") || strings.HasPrefix(upper, "ALTER ") {
			t.Fatalf("non-explainable preflight SQL: %s", sqlText)
		}
	}
}
```

- [ ] **Step 5: Add explainable lock preflight statements**

Add:

```go
case "lock_row_chain":
	return []string{rowUpdate(schema, "lock_targets", 1)}, nil
case "lock_table_exclusive":
	return []string{"SELECT count(*) FROM " + schema + ".lock_table_targets"}, nil
case "lock_ddl_wait":
	return []string{rowUpdate(schema, "lock_ddl_targets", 1)}, nil
```

These statements go only to `EXPLAIN`; `LockEngine` executes and observes the actual LOCK/DDL workload.

- [ ] **Step 6: Run evidence/preflight tests and commit**

Run:

```bash
gofmt -w internal/gsbench/lock_engine.go internal/gsbench/lock_engine_test.go internal/gsbench/scenario_locks.go internal/gsbench/workload_catalog.go internal/gsbench/plan_cache_test.go
go test ./internal/gsbench -run 'TestConfiguredLockEvidence|TestLockScenarioRuntimeEvidence|TestScenarioWorkloadStatementsCoverEveryRegisteredScenario|TestRowChain' -count=1
```

Expected: PASS.

Commit:

```bash
git add internal/gsbench/lock_engine.go internal/gsbench/lock_engine_test.go internal/gsbench/scenario_locks.go internal/gsbench/workload_catalog.go internal/gsbench/plan_cache_test.go
git commit -m "fix(gsbench): verify configured lock topology"
```

---

### Task 5: Published configuration, regression verification, and local deployment

**Files:**
- Modify: `configs/gsbench.cfg:74-109`
- Modify: `docs/gsbench/CONFIG.md:166-203`
- Modify: `internal/gsbench/cli.go:403-441` if help wording needs alignment with published examples.

**Interfaces:**
- Consumes: final CLI/config names and defaults from Task 1.
- Produces: locally installed `/Users/sqlrush/gstop/gsbench-local/bin/gsbench`.

- [ ] **Step 1: Publish exact config keys and commands**

Add:

```ini
[scenario.lock_row_chain]
sessions = 2
chain_depth = 1

[scenario.lock_table_exclusive]
sessions = 2

[scenario.lock_ddl_wait]
sessions = 2
```

Document that sessions includes the holder and show:

```bash
gsbench run 501 --sessions 10 --chain-depth 3 --duration 1m
gsbench run 502 --sessions 10 --duration 1m
gsbench run 503 --sessions 10 --duration 1m
```

- [ ] **Step 2: Format and run the focused regression suite**

Run:

```bash
gofmt -w internal/gsbench/*.go
git diff --check
go test ./internal/gsbench -run 'CLI|Config|Lock|ScenarioWorkloadStatements' -count=1
```

Expected: PASS with no formatting errors.

- [ ] **Step 3: Run package regressions and build locally**

Run:

```bash
go test ./internal/gsbench -count=1
go test ./cmd/gsbench -count=1
go build -o /Users/sqlrush/gstop/gsbench-local/bin/gsbench ./cmd/gsbench
/Users/sqlrush/gstop/gsbench-local/bin/gsbench help
```

Expected: tests PASS; help lists the two flags and 501–503 examples.

- [ ] **Step 4: Run short og5 smoke workloads when its local config is available**

Run:

```bash
/Users/sqlrush/gstop/gsbench-local/bin/gsbench run 501 --sessions 4 --chain-depth 2 --duration 3s
/Users/sqlrush/gstop/gsbench-local/bin/gsbench run 502 --sessions 4 --duration 3s
/Users/sqlrush/gstop/gsbench-local/bin/gsbench run 503 --sessions 4 --duration 3s
```

Expected: each reaches three blocked waiters; 501 reports three edges and actual depth 2; Stop/restore leaves no owned tagged sessions. If og5 or the initialized schema is unavailable, report that environment limit separately without replacing automated results.

- [ ] **Step 5: Commit docs and record deployment identity**

Commit repository files only:

```bash
git add configs/gsbench.cfg docs/gsbench/CONFIG.md internal/gsbench/cli.go
git commit -m "docs(gsbench): document lock workload controls"
```

Record final identities:

```bash
git status --short --branch
git log -5 --oneline
shasum -a 256 /Users/sqlrush/gstop/gsbench-local/bin/gsbench
```
