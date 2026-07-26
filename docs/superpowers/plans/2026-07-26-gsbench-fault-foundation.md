# gsbench Fault Foundation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the canonical scenario catalog, topology/capability model, typed mutation journal, distributed dataset dialects, safety authorization, and universal restore engine required by every three-digit gsbench scenario.

**Architecture:** Replace scattered scenario maps with one immutable catalog and make the runner select a strategy using an `Environment` discovered by read-only probes. Generalize the SQL-only mutation journal into typed actions mirrored to a local recovery ledger, then make stop, crash recovery, and explicit restore share one idempotent restore coordinator. Keep provider execution behind an allowlisted interface so ordinary SQL scenarios never depend on host commands.

**Tech Stack:** Go 1.26.5, `database/sql`, openGauss connector-go-pq, JSON recovery ledger, existing INI parser, Go `testing`.

## Global Constraints

- This plan creates infrastructure only; it does not implement the scenario-family workloads.
- Catalog codes and names come from `2026-07-25-gsbench-fault-scenario-refactor-design.md`.
- Old numeric aliases 1–15 are rejected; non-conflicting text aliases may print one-release deprecation warnings.
- `gsbench restore` is idempotent and can run after process death.
- Database and local recovery records are both written before any external action.
- Unknown products default to unsupported, not GaussDB.
- No command uses `sh -c` with configurable text.
- The current workspace has no top-level `.git`; use test checkpoints and do not initialize a repository.

---

### Task 1: Canonical Scenario Catalog and Three-Digit CLI

**Files:**
- Create: `internal/gsbench/scenario_types.go`
- Create: `internal/gsbench/scenario_catalog.go`
- Create: `internal/gsbench/scenario_catalog_test.go`
- Modify: `internal/gsbench/cli.go`
- Modify: `internal/gsbench/cli_test.go`
- Modify: `internal/gsbench/config.go`
- Modify: `internal/gsbench/config_test.go`

**Interfaces:**
- Produces: `type ScenarioCode uint16`
- Produces: `type Category uint8`
- Produces: shared risk, environment, topology, node, capability, and requirement value types.
- Produces: `type ScenarioDefinition struct`
- Produces: `func DefaultScenarioCatalog() *ScenarioCatalog`
- Produces: `func (c *ScenarioCatalog) Resolve(string) (ScenarioDefinition, error)`
- Produces: `func DesignedScenarioCodes() []ScenarioCode`
- Produces: `func (c *ScenarioCatalog) MustCode(ScenarioCode) ScenarioDefinition`
- Consumes later: every registry, config, runner, help, status, and evidence path.

- [ ] **Step 1: Write the failing catalog tests**

Create:

```go
func TestDefaultScenarioCatalogIsCompleteAndUnique(t *testing.T) {
	catalog := DefaultScenarioCatalog()
	if got, want := catalog.Codes(), DesignedScenarioCodes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("codes=%v want=%v", got, want)
	}
	for _, definition := range catalog.Definitions() {
		if int(definition.Code)/100 != int(definition.Category) {
			t.Fatalf("code=%d category=%d", definition.Code, definition.Category)
		}
	}
}

func TestCatalogResolvesCodeAndCanonicalName(t *testing.T) {
	catalog := DefaultScenarioCatalog()
	for _, input := range []string{"601", "planchange_stats_target"} {
		got, err := catalog.Resolve(input)
		if err != nil {
			t.Fatal(err)
		}
		if got.Code != 601 || got.Name != "planchange_stats_target" {
			t.Fatalf("definition=%+v", got)
		}
	}
}

func TestCatalogRejectsLegacyNumericAlias(t *testing.T) {
	if _, err := DefaultScenarioCatalog().Resolve("1"); err == nil {
		t.Fatal("legacy numeric alias accepted")
	}
}
```

- [ ] **Step 2: Run the tests and confirm the missing symbols**

Run:

```bash
go test ./internal/gsbench -run 'TestDefaultScenarioCatalog|TestCatalog' -count=1
```

Expected: compile failure because the catalog types do not exist.

- [ ] **Step 3: Implement the immutable catalog types**

Add:

```go
type ScenarioCode uint16
type Category uint8
type RiskLevel string
type EnvironmentClass string
type Requirement string
type Product string
type Topology string
type NodeRole string
type Capability string
type CapabilitySet map[Capability]bool

type Node struct {
	Name string
	Role NodeRole
	Shard string
	Host string
	Port int
}

type Environment struct {
	Product Product
	Version string
	Topology Topology
	Nodes []Node
	Capabilities CapabilitySet
	Warnings []string
	Supported bool
}

type ScenarioDefinition struct {
	Code      ScenarioCode
	Name      string
	Category  Category
	Risk      RiskLevel
	AppliesTo []EnvironmentClass
	Requires  []Requirement
}

type ScenarioCatalog struct {
	byCode map[ScenarioCode]ScenarioDefinition
	byName map[string]ScenarioDefinition
	codes  []ScenarioCode
}
```

Implement `NewScenarioCatalog(definitions []ScenarioDefinition)` so it rejects
duplicate codes/names, non-three-digit codes, and category mismatches. Define the
90 approved code/name pairs once in `DefaultScenarioCatalog`; generate 520–540
from the lock-mode pair table instead of duplicating it in CLI code.

Define all approved constants for these value types in `scenario_types.go`; later
tasks add behavior without redeclaring the types.

- [ ] **Step 4: Route CLI and config through the catalog**

Add a transition field without deleting the string bridge yet:

```go
type CLIOptions struct {
	// existing fields
	ScenarioCodes []ScenarioCode
}

type RunConfig struct {
	ScenarioCodes []ScenarioCode
	Scenarios []string // temporary canonical-name bridge removed by Task 8
	// existing fields
}
```

Make `ParseCLIArgs` resolve each input through `DefaultScenarioCatalog()`.
`LoadConfig` fills both code and canonical-name slices so existing app/runner code
continues compiling until Task 8. Remove `validScenarios` and the legacy numeric
map.

- [ ] **Step 5: Rewrite help and config tests**

Assert help contains `101=tp_cpu`, `601=planchange_stats_target`,
`621=hardparse_literal_flood`, and `gsbench scenarios`; assert it does not
contain `1=tp_cpu` or `lock_storm`.

- [ ] **Step 6: Add a catalog-list command**

Extend lifecycle commands with `scenarios` and print stable columns:

```text
CODE  CATEGORY  NAME  RISK  APPLIES_TO
```

The command is read-only and does not connect to a database.

- [ ] **Step 7: Run the focused gate**

Run:

```bash
gofmt -w internal/gsbench/scenario_catalog.go internal/gsbench/scenario_catalog_test.go internal/gsbench/cli.go internal/gsbench/cli_test.go internal/gsbench/config.go internal/gsbench/config_test.go
go test ./internal/gsbench -run 'Catalog|CLI|Config' -count=1
```

Expected: PASS.

### Task 2: Result States, Applicability, and Risk Authorization

**Files:**
- Modify: `internal/gsbench/model.go`
- Create: `internal/gsbench/safety.go`
- Create: `internal/gsbench/safety_test.go`
- Modify: `internal/gsbench/config.go`
- Modify: `internal/gsbench/cli.go`

**Interfaces:**
- Produces: `OutcomeNotApplicable`, `OutcomeRestoreFailed`, `OutcomeNotImplemented`
- Produces: `func AuthorizeScenario(ScenarioDefinition, BenchConfig, CLIOptions, Environment) error`
- Consumes later: runner preflight and every risk B/C scenario.

- [ ] **Step 1: Write failing authorization tests**

```go
func TestAuthorizeRiskCRequiresConfigCLIAndProvider(t *testing.T) {
	def := ScenarioDefinition{Code: 343, Risk: RiskC}
	cfg := BenchConfig{Safety: SafetyConfig{
		AllowInfrastructureFault: true,
	}}
	env := Environment{Capabilities: CapabilitySet{CapabilityExternalFaultProvider: true}}
	if err := AuthorizeScenario(def, cfg, CLIOptions{}, env); err == nil {
		t.Fatal("risk C accepted without CLI authorization")
	}
	if err := AuthorizeScenario(def, cfg, CLIOptions{AllowRisk: RiskC}, env); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Run and confirm failure**

Run:

```bash
go test ./internal/gsbench -run 'Authorize|Outcome' -count=1
```

Expected: compile failure for the new types.

- [ ] **Step 3: Add exact safety configuration**

Add fields:

```go
type SafetyConfig struct {
	// existing fields
	RestoreTimeout           time.Duration
	ProfileCapGB             int
	AllowAdminMutation       bool
	AllowInfrastructureFault bool
	RestoreOriginalRole      bool
}
```

Parse `safety.restore_timeout`, `safety.profile_cap_gb`,
`safety.allow_admin_mutation`, `safety.allow_infrastructure_fault`, and
`safety.restore_original_role`.

- [ ] **Step 4: Parse explicit CLI risk authorization**

Add `--allow-risk A|B|C`; reject any other string. `AuthorizeScenario` requires:

```text
A: no extra authorization
B: allow_admin_mutation=true and CLI AllowRisk >= B
C: allow_infrastructure_fault=true, CLI AllowRisk=C, provider capability=true
```

- [ ] **Step 5: Extend result ordering**

Define exact ordering:

```go
var outcomeRank = map[Outcome]int{
	OutcomeSuccess: 0,
	OutcomeNotApplicable: 0,
	OutcomeDegraded: 1,
	OutcomeNotImplemented: 2,
	OutcomeFailed: 3,
	OutcomeRestoreFailed: 4,
}
```

Keep `NOT_APPLICABLE` from making a multi-scenario run fail, but return nonzero
for `NOT_IMPLEMENTED`, `FAILED`, and `RESTORE_FAILED`.

- [ ] **Step 6: Run the focused tests**

```bash
gofmt -w internal/gsbench/model.go internal/gsbench/safety.go internal/gsbench/safety_test.go internal/gsbench/config.go internal/gsbench/cli.go
go test ./internal/gsbench -run 'Authorize|Outcome|SafetyConfig' -count=1
```

Expected: PASS.

### Task 3: Product, Topology, Node, and Capability Detection

**Files:**
- Replace: `internal/gsbench/capability.go`
- Replace: `internal/gsbench/capability_test.go`
- Modify: `internal/gsbench/scenario_types.go`
- Create: `internal/gsbench/environment.go`
- Create: `internal/gsbench/environment_test.go`
- Create: `internal/gsbench/test_helpers_test.go`

**Interfaces:**
- Produces: `func DetectEnvironment(context.Context, CapabilityProber) Environment`
- Produces test helpers: `centralizedFixture() Environment` and `distributedFixture() Environment`.
- Consumes later: catalog applicability, dataset dialect, observers, providers.

- [ ] **Step 1: Write product and topology tests**

```go
func TestDetectEnvironmentDistinguishesAllTargets(t *testing.T) {
	tests := []struct {
		version string
		nodes   string
		product Product
		topology Topology
	}{
		{"openGauss 7.0", "", ProductOpenGauss, TopologyStandalone},
		{"GaussDB Kernel V500", "1", ProductGaussDB, TopologyCentralized},
		{"GaussDB Kernel V500", "6", ProductGaussDB, TopologyDistributed},
	}
	for _, tt := range tests {
		prober := fakeCapabilityProber{values: map[string]string{
			"version": tt.version,
			"distributed_nodes": tt.nodes,
		}}
		got := DetectEnvironment(context.Background(), prober)
		if got.Product != tt.product || got.Topology != tt.topology {
			t.Fatalf("version=%q nodes=%q got=%+v", tt.version, tt.nodes, got)
		}
	}
}

func TestUnknownProductIsUnsupported(t *testing.T) {
	env := DetectEnvironment(context.Background(), fakeCapabilityProber{
		values: map[string]string{"version": "PostgreSQL 16"},
	})
	if env.Product != ProductUnknown || env.Supported {
		t.Fatalf("environment=%+v", env)
	}
}
```

- [ ] **Step 2: Run and confirm the old centralized rejection**

```bash
go test ./internal/gsbench -run 'DetectEnvironment|UnknownProduct' -count=1
```

Expected: FAIL because the current detector has no topology/node model and rejects
distributed targets.

- [ ] **Step 3: Define environment types and capability set**

Use the Task 1 value types with this exact shape; do not redeclare them:

```go
type CapabilitySet map[Capability]bool

type Node struct {
	Name  string
	Role  NodeRole
	Shard string
	Host  string
	Port  int
}

type Environment struct {
	Product      Product
	Version      string
	Topology     Topology
	Nodes        []Node
	Capabilities CapabilitySet
	Warnings     []string
	Supported    bool
}
```

- [ ] **Step 4: Implement read-only probes**

Probe product/version, `pgxc_node`, current node name, thread pool, GPC,
statement-history columns, global lock views, memory views, WAL sender functions,
standby controls, and distributed stream support. A failed optional probe adds a
warning and leaves only that capability false.

- [ ] **Step 5: Add per-definition applicability**

Implement:

```go
func (e Environment) Applicable(def ScenarioDefinition) bool
func (e Environment) Missing(requirements []Requirement) []Requirement
```

Distributed-only scenarios return `NOT_APPLICABLE` outside distributed GaussDB;
missing capabilities on an applicable topology return a detailed preflight error
or `DEGRADED` only when the definition declares a real fallback.

- [ ] **Step 6: Update doctor output**

Print product, version, topology, nodes, capabilities, and per-scenario
`SUPPORTED`, `DEGRADED`, or `NOT_APPLICABLE` decisions.

- [ ] **Step 7: Run the focused tests**

```bash
gofmt -w internal/gsbench/environment.go internal/gsbench/environment_test.go internal/gsbench/capability.go internal/gsbench/capability_test.go
go test ./internal/gsbench -run 'Environment|Capabilities|Doctor' -count=1
```

Expected: PASS.

### Task 4: Distributed Dataset Dialects and New Benchmark Objects

**Files:**
- Create: `internal/gsbench/dataset_dialect.go`
- Create: `internal/gsbench/dataset_dialect_test.go`
- Modify: `internal/gsbench/dataset.go`
- Modify: `internal/gsbench/dataset_test.go`
- Modify: `internal/gsbench/plan_dataset.go`
- Modify: `internal/gsbench/plan_dataset_test.go`

**Interfaces:**
- Produces: `type DatasetDialect interface`
- Produces: `func DatasetDialectFor(Environment) DatasetDialect`
- Produces: expanded `DatasetPlan` for all approved benchmark objects.
- Consumes later: every scenario-family plan.

- [ ] **Step 1: Write dialect tests**

```go
func TestDistributedDatasetUsesExplicitDistribution(t *testing.T) {
	d := DatasetDialectFor(Environment{
		Product: ProductGaussDB, Topology: TopologyDistributed,
	})
	ddl := strings.Join(d.TableDDL("gsbench"), "\n")
	for _, token := range []string{
		"DISTRIBUTE BY HASH (dist_key)",
		"DISTRIBUTE BY REPLICATION",
		"PRIMARY KEY (run_id, id)",
	} {
		if !strings.Contains(ddl, token) {
			t.Fatalf("missing %q in %s", token, ddl)
		}
	}
}

func TestCentralizedDatasetContainsNoDistributeClause(t *testing.T) {
	d := DatasetDialectFor(Environment{Topology: TopologyCentralized})
	if strings.Contains(strings.Join(d.TableDDL("gsbench"), "\n"), "DISTRIBUTE BY") {
		t.Fatal("centralized DDL contains DISTRIBUTE BY")
	}
}
```

- [ ] **Step 2: Run and confirm failure**

```bash
go test ./internal/gsbench -run 'Dataset.*Distribution|DistributedDataset' -count=1
```

Expected: compile failure for `DatasetDialect`.

- [ ] **Step 3: Define the dialect**

```go
type DatasetDialect interface {
	TableDDL(schema string) []string
	BatchPlans(schema string, targetBytes int64) []TableBatch
	Migrations(schema string) []TableMigration
}
```

Implement centralized/openGauss and distributed variants. Validate schema once and
build all identifiers from constants.

- [ ] **Step 4: Add the new object groups**

Add the exact tables from design section 5: memory/I/O, hard parse, lock-mode,
distributed lock, replication, distributed joins/skew/transactions, and metadata.
For distributed metadata, use `run_id` distribution and composite primary keys;
do not use replicated `bigserial`.

- [ ] **Step 5: Make init choose the detected dialect**

Change `PlanDataset` to accept `Environment`:

```go
func PlanDataset(cfg BenchConfig, capacity DiskCapacity, env Environment) (DatasetPlan, error)
```

Update all callers and tests. `init --dry-run` prints the selected product/topology
and final DDL.

- [ ] **Step 6: Verify schema creation remains compatible**

Keep catalog-check then plain `CREATE SCHEMA`; add tests proving no DDL contains
`CREATE SCHEMA IF NOT EXISTS`.

- [ ] **Step 7: Run dataset tests**

```bash
gofmt -w internal/gsbench/dataset_dialect.go internal/gsbench/dataset_dialect_test.go internal/gsbench/dataset.go internal/gsbench/dataset_test.go internal/gsbench/plan_dataset.go internal/gsbench/plan_dataset_test.go
go test ./internal/gsbench -run 'Dataset|PlanData|Schema' -count=1
```

Expected: PASS.

### Task 5: Typed Action Journal

**Files:**
- Create: `internal/gsbench/action.go`
- Create: `internal/gsbench/action_test.go`
- Modify: `internal/gsbench/journal.go`
- Modify: `internal/gsbench/journal_test.go`
- Modify: `internal/gsbench/sqlstore.go`
- Modify: `internal/gsbench/sqlstore_test.go`

**Interfaces:**
- Produces: `type ActionKind string`
- Produces: `type Action struct`
- Produces: `type ActionExecutor interface`
- Produces: `func (j *Journal) ApplyAction(context.Context, Action) error`
- Produces: `func (j *Journal) RestoreRun(context.Context, string) error`
- Consumes later: SQL, GUC, network, process, role, cloud, and baseline actions.

- [ ] **Step 1: Write ordering and validation tests**

```go
func TestApplyActionPersistsBeforeForwardExecution(t *testing.T) {
	order := []string{}
	store := &fakeActionStore{onInsert: func() { order = append(order, "journal") }}
	exec := &fakeActionExecutor{onApply: func() { order = append(order, "apply") }}
	err := NewJournal(store, exec).ApplyAction(context.Background(), Action{
		RunID: "run-1", ScenarioCode: 601, Kind: ActionSQLMutation,
		Target: "gsbench.plan_data", Forward: `{"sql":"ANALYZE gsbench.plan_data"}`,
		Inverse: `{"sql":"ANALYZE gsbench.plan_data"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"journal", "apply"}) {
		t.Fatalf("order=%v", order)
	}
}

func TestRestoreRunUsesReverseActionOrder(t *testing.T) {
	got := []ScenarioCode{}
	store := &fakeActionStore{pending: []Action{
		{Sequence: 1, RunID: "run-1", ScenarioCode: 101},
		{Sequence: 2, RunID: "run-1", ScenarioCode: 102},
		{Sequence: 3, RunID: "run-1", ScenarioCode: 103},
	}}
	exec := &fakeActionExecutor{
		onRestore: func(a Action) { got = append(got, a.ScenarioCode) },
	}
	if err := NewJournal(store, exec).RestoreRun(context.Background(), "run-1"); err != nil {
		t.Fatal(err)
	}
	want := []ScenarioCode{103, 102, 101}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
}
```

- [ ] **Step 2: Run and confirm failure**

```bash
go test ./internal/gsbench -run 'ApplyAction|RestoreRunUsesReverse' -count=1
```

Expected: compile failure for typed actions.

- [ ] **Step 3: Define action kinds and payload**

```go
const (
	ActionSQLMutation       ActionKind = "SQL_MUTATION"
	ActionSessionSet        ActionKind = "SESSION_SET"
	ActionSessionTransaction ActionKind = "SESSION_TRANSACTION"
	ActionGUCFileChange     ActionKind = "GUC_FILE_CHANGE"
	ActionNetworkQDisc      ActionKind = "NETWORK_QDISC"
	ActionNetworkFirewall   ActionKind = "NETWORK_FIREWALL"
	ActionProcessState      ActionKind = "PROCESS_STATE"
	ActionNodeRole          ActionKind = "NODE_ROLE"
	ActionCloudFaultJob     ActionKind = "CLOUD_FAULT_JOB"
	ActionDataBaseline      ActionKind = "DATA_BASELINE"
)

type Action struct {
	Sequence     int64
	RunID       string
	ScenarioCode ScenarioCode
	Kind        ActionKind
	Target      string
	Node        string
	Original    json.RawMessage
	Forward     json.RawMessage
	Inverse     json.RawMessage
	Verify      json.RawMessage
}
```

- [ ] **Step 4: Preserve SQL mutation compatibility internally**

Add `SQLAction(m Mutation) Action` so existing plan mutation builders can migrate
without duplicating journal logic. Do not retain two independent restore engines.

- [ ] **Step 5: Migrate metadata schema and store queries**

Add action kind, scenario code, node, and JSON payload columns through idempotent
dataset migrations. Store parameterized values; only validated schema identifiers
are interpolated.

- [ ] **Step 6: Run journal/store tests**

```bash
gofmt -w internal/gsbench/action.go internal/gsbench/action_test.go internal/gsbench/journal.go internal/gsbench/journal_test.go internal/gsbench/sqlstore.go internal/gsbench/sqlstore_test.go
go test ./internal/gsbench -run 'Action|Journal|SQLStore' -count=1
```

Expected: PASS.

### Task 6: Local Recovery Ledger and Provider Interface

**Files:**
- Create: `internal/gsbench/recovery_ledger.go`
- Create: `internal/gsbench/recovery_ledger_test.go`
- Create: `internal/gsbench/provider.go`
- Create: `internal/gsbench/provider_test.go`
- Modify: `internal/gsbench/config.go`
- Modify: `internal/gsbench/config_test.go`

**Interfaces:**
- Produces: `type RecoveryLedger interface`
- Produces: `type FaultProvider interface`
- Produces: `type NoopFaultProvider`
- Produces: `func NewFileRecoveryLedger(path string) RecoveryLedger`
- Consumes later: universal restore and infrastructure plans.

- [ ] **Step 1: Write atomic ledger tests**

```go
func TestFileRecoveryLedgerPersistsPendingActionAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.json")
	ledger := NewFileRecoveryLedger(path)
	action := Action{RunID: "run-1", ScenarioCode: 343, Kind: ActionNetworkFirewall}
	if err := ledger.Put(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	got, err := ledger.Pending(context.Background(), "")
	if err != nil || len(got) != 1 {
		t.Fatalf("pending=%v err=%v", got, err)
	}
}
```

- [ ] **Step 2: Run and confirm failure**

```bash
go test ./internal/gsbench -run 'RecoveryLedger|FaultProvider' -count=1
```

Expected: compile failure.

- [ ] **Step 3: Define exact interfaces**

```go
type RecoveryLedger interface {
	Put(context.Context, Action) error
	MarkRestored(context.Context, string, string) error
	Pending(context.Context, string) ([]Action, error)
}

type FaultProvider interface {
	Name() string
	Preflight(context.Context, Environment, Action) error
	Apply(context.Context, Action) error
	Restore(context.Context, Action) error
	VerifyRestored(context.Context, Action) error
}
```

- [ ] **Step 4: Implement atomic JSON storage**

Write to a same-directory temporary file, `Sync`, close, and rename. Enforce file
mode `0600`. Validate run ID, action kind, and target before serialization.

- [ ] **Step 5: Implement the no-op provider**

`NoopFaultProvider` rejects every non-SQL infrastructure action with an error that
names the missing configured provider. It never reports success.

- [ ] **Step 6: Add exact provider configuration**

```go
type FaultProviderConfig struct {
	Type       string
	LedgerPath string
}
```

Add it to `BenchConfig`, parse `fault_provider.type` and
`fault_provider.ledger_path`, and allow only `none`, `local`, `ssh`, and
`gaussdb_api`. Default the ledger path to a config-identity-specific JSON file
under `logs`.

- [ ] **Step 7: Run the focused tests**

```bash
gofmt -w internal/gsbench/recovery_ledger.go internal/gsbench/recovery_ledger_test.go internal/gsbench/provider.go internal/gsbench/provider_test.go
go test ./internal/gsbench -run 'RecoveryLedger|FaultProvider' -count=1
```

Expected: PASS.

### Task 7: Universal Restore Coordinator

**Files:**
- Replace: `internal/gsbench/restore.go`
- Replace: `internal/gsbench/restore_test.go`
- Modify: `internal/gsbench/app.go`
- Modify: `internal/gsbench/status.go`
- Modify: `internal/gsbench/status_test.go`

**Interfaces:**
- Produces: `type RestoreCoordinator struct`
- Produces: `func (r *RestoreCoordinator) Restore(context.Context, RestoreRequest) RestoreSummary`
- Produces: `type RestoreRequest struct { RunID string; DryRun bool }`
- Consumes later: stop, explicit restore, startup stale recovery, all providers.

- [ ] **Step 1: Write full-order restore tests**

```go
func TestRestoreCoordinatorRunsSafetyOrder(t *testing.T) {
	events := []string{}
	backend := fakeRestoreBackend{
		stop: func() { events = append(events, "stop_sessions") },
		external: func() { events = append(events, "external_inverse") },
		database: func() { events = append(events, "database_inverse") },
		baseline: func() { events = append(events, "baseline") },
		verify: func() { events = append(events, "verify") },
	}
	summary := NewRestoreCoordinator(backend).Restore(context.Background(), RestoreRequest{})
	if summary.Failed {
		t.Fatal(summary.Err)
	}
	want := []string{"stop_sessions", "external_inverse", "database_inverse", "baseline", "verify"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v want=%v", events, want)
	}
}

func TestRestoreDryRunDoesNotMutate(t *testing.T) {
	mutations := 0
	backend := fakeRestoreBackend{
		stop: func() { mutations++ },
		external: func() { mutations++ },
		database: func() { mutations++ },
		baseline: func() { mutations++ },
		verify: func() { mutations++ },
		pending: []Action{{Sequence: 7, RunID: "run-1", Kind: ActionNetworkQDisc}},
	}
	summary := NewRestoreCoordinator(backend).Restore(
		context.Background(),
		RestoreRequest{RunID: "run-1", DryRun: true},
	)
	if mutations != 0 {
		t.Fatalf("dry-run made %d mutations", mutations)
	}
	if len(summary.PlannedActions) != 1 || summary.PlannedActions[0].Sequence != 7 {
		t.Fatalf("planned=%+v", summary.PlannedActions)
	}
}
```

- [ ] **Step 2: Run and confirm failure**

```bash
go test ./internal/gsbench -run 'RestoreCoordinator|RestoreDryRun' -count=1
```

Expected: FAIL because the old restore backend only stops and restores SQL journal
entries and has a plan-data special case.

- [ ] **Step 3: Implement discovery and mutex**

Discover active meta runs, pending database actions, and pending local ledger
actions. Use a database advisory lock plus a local lock file keyed by config
identity; if either lock is held, return a clear busy error.

- [ ] **Step 4: Implement the exact restore stages**

Stages:

```text
stop tagged sessions
restore NETWORK_FIREWALL/NETWORK_QDISC/PROCESS_STATE/CLOUD_FAULT_JOB
restore GUC_FILE_CHANGE/NODE_ROLE
restore SQL_MUTATION/DATA_BASELINE in reverse ID order
repair benchmark baseline
re-detect topology
verify sessions/actions/replication/provider state
```

Errors are accumulated, journal entries remain retryable, and outcome becomes
`RESTORE_FAILED`.

- [ ] **Step 5: Remove the plan-data command guard**

`commandRestore` requires only initialized metadata. It must work when plan tables
are absent or damaged and must not create a new run ID for recovery logs.

- [ ] **Step 6: Make stop delegate to restore**

`commandStop` builds `RestoreRequest{RunID: requested}` and calls the same
coordinator. Preserve user-visible command names in logs.

- [ ] **Step 7: Add startup stale recovery**

Before a mutating `run`, call the coordinator for pending stale runs. `doctor`
reports stale state but does not mutate unless invoked with an explicit future
repair option; explicit `restore` remains the emergency entrypoint.

- [ ] **Step 8: Run restore tests**

```bash
gofmt -w internal/gsbench/restore.go internal/gsbench/restore_test.go internal/gsbench/app.go internal/gsbench/status.go internal/gsbench/status_test.go
go test ./internal/gsbench -run 'Restore|Stop|Status|Stale' -count=1
```

Expected: PASS.

### Task 8: Runner Strategy Registration and Evidence Envelope

**Files:**
- Modify: `internal/gsbench/runner.go`
- Modify: `internal/gsbench/runner_test.go`
- Modify: `internal/gsbench/model.go`
- Create: `internal/gsbench/evidence.go`
- Create: `internal/gsbench/evidence_test.go`
- Modify: `internal/gsbench/app.go`
- Modify: `internal/gsbench/runlog.go`

**Interfaces:**
- Produces: `type ScenarioFactory func(ScenarioDefinition, Environment) (Scenario, error)`
- Produces: `type EvidenceEnvelope struct`
- Produces: catalog-driven `NewRunner`.
- Consumes later: all scenario implementations and final JSON/log output.

- [ ] **Step 1: Write registry/applicability tests**

```go
func TestRunnerReturnsNotApplicableBeforeFactory(t *testing.T) {
	def := ScenarioDefinition{
		Code: 721, Name: "cluster_data_skew",
		AppliesTo: []EnvironmentClass{EnvironmentDistributedGaussDB},
	}
	runner := NewRunner(runtimeFor(TopologyStandalone), catalogWith(def), nil)
	got := runner.Run(context.Background(), []ScenarioCode{721})
	if got.Results[0].Outcome != OutcomeNotApplicable {
		t.Fatalf("result=%+v", got.Results[0])
	}
}
```

- [ ] **Step 2: Run and confirm failure**

```bash
go test ./internal/gsbench -run 'Runner.*Applicable|EvidenceEnvelope' -count=1
```

Expected: compile failure or old string-registry behavior.

- [ ] **Step 3: Change runtime and result identities**

Add `Environment`, `Catalog`, `Provider`, and `Ledger` to `Runtime`. Add
`ScenarioCode`, product, topology, strategy, targets, risk, and restore summary to
`Result`/`EvidenceEnvelope`.

Add `Code() ScenarioCode` to `Scenario`; catalog names remain the only source of
display names.

- [ ] **Step 4: Register factories by code**

The runner receives `map[ScenarioCode]ScenarioFactory`; a missing factory returns
`NOT_IMPLEMENTED`, not “scenario not registered” as a generic failure.

- [ ] **Step 5: Enforce preflight ordering**

Before `Prepare`:

```text
catalog definition exists
environment applicable
requirements present
risk authorized
restore preflight ready
workload plan preflight
scenario prepare
```

- [ ] **Step 6: Add restore verification phase**

Extend phases with `verify_restore`. If workload succeeds but stop/restore fails,
the final outcome is `RESTORE_FAILED`.

- [ ] **Step 7: Run runner/evidence tests**

```bash
gofmt -w internal/gsbench/runner.go internal/gsbench/runner_test.go internal/gsbench/model.go internal/gsbench/evidence.go internal/gsbench/evidence_test.go internal/gsbench/app.go internal/gsbench/runlog.go
go test ./internal/gsbench -run 'Runner|Evidence|Phase|Outcome' -count=1
```

Expected: PASS.

### Task 9: Foundation Documentation and Full Gate

**Files:**
- Modify: `configs/gsbench.cfg`
- Modify: `docs/gsbench/README.md`
- Modify: `internal/gsbench/integration_test.go`

**Interfaces:**
- Consumes: Tasks 1–8.
- Produces: documented configuration, dry-run examples, and a stable base for the remaining sub-projects.

- [ ] **Step 1: Replace example configuration**

Add `[topology]`, new `[safety]`, and `[fault_provider]` sections from the design.
Set the default scenario to `101`; keep risk B/C disabled.

- [ ] **Step 2: Rewrite README lifecycle and compatibility**

Document `scenarios`, three-digit selection, `doctor`, universal restore, status
outcomes, product/topology matrix, and the fact that only registered factories are
reported as implemented.

- [ ] **Step 3: Add a foundation smoke test**

Test:

```text
help/scenarios require no DB
doctor detects fixture environment
init dry-run selects dialect
restore dry-run discovers actions without mutation
```

- [ ] **Step 4: Run formatting and full package verification**

```bash
gofmt -w internal/gsbench cmd/gsbench
go test ./internal/gsbench -count=1
go test -race ./internal/gsbench -count=1
go vet ./internal/gsbench ./cmd/gsbench
```

Expected: PASS.

- [ ] **Step 5: Record the checkpoint**

Current workspace: record the exact command output in the execution report.
Git-backed checkout:

```bash
git add internal/gsbench cmd/gsbench configs/gsbench.cfg docs/gsbench/README.md
git commit -m "refactor(gsbench): add three-digit fault foundation"
```
