# gsbench v1.1.8 Advisory Precheck and Read-Only Recovery Design

## Goal

Release gsbench v1.1.8 with three related behavior changes:

1. Treat an index created with the database-selected `ubtree` access method as
   compatible with an expected index whose DDL did not explicitly select an
   access method.
2. Convert scenario suitability, capacity, data-shape, metadata-shape, and
   plan-shape checks from hard gates or automatic optimizers into read-only
   warnings. A warning must not stop a selected scenario or rewrite the
   database to make the scenario fit.
3. Centralize persistent recovery as a read-only plan. `restore` prints the
   recovery DDL/DML for all scenarios, while `run 601-606 recover` prints only
   the selected plan scenario's recovery DDL/DML. Neither command executes the
   generated recovery operations.

The guiding rule is that gsbench injects the requested test load into the
environment as it exists. It reports why the environment may not produce the
expected result, but it does not silently tune the environment and it does not
refuse the load merely because a target is unlikely to be reached.

## Scope and safety boundary

Read-only inspection is limited to:

- objects owned by gsbench in the configured `data.schema`;
- the database journal and local recovery ledger maintained by gsbench; and
- instance or external-fault state for which gsbench previously recorded a
  recovery action.

It must not inspect or judge unrelated business schemas or infer recovery
operations for business objects that gsbench did not create or journal.

The following remain hard guards rather than advisory checks:

- command and configuration syntax needed to interpret the request;
- database connectivity;
- identifier, run-ID, SQL-injection, and schema-ownership protections;
- explicit Risk-B and Risk-C authorization gates; and
- durable journal/ledger recording before a persistent fault is applied.

The journal/ledger guard is required because a persistent mutation without a
recorded original value cannot be rendered as a reliable manual recovery plan.
An actual SQL, connection, session, or fault-provider execution error fails the
affected scenario. It does not prevent other selected scenarios from running.

## Command behavior

### `gsbench run`

Before execution, gsbench runs applicable inspectors and emits structured
warnings for every observed mismatch. It does not run repair, migration,
ANALYZE, index creation, parameter reset, or any other optimization solely to
make the environment satisfy a scenario expectation.

Former hard limits, including worker, session, connection, memory, disk,
capacity-ceiling, and target-reachability checks, become advisory facts. The
requested scenario proceeds with the requested target. Resource exhaustion or
another actual execution error may still fail that scenario.

At duration expiry, Ctrl+C, or normal completion, the process closes the
connections, worker goroutines, sessions, and transactions that it owns. Open
transactions roll back through ordinary session cleanup. The run does not
execute persistent inverse DDL/DML and does not mark persistent journal actions
as restored.

One scenario's prepare or execution failure must not cancel or skip unrelated
scenarios in the same command. Control-plane status recording is best-effort
for scenarios that do not persist mutations. A scenario that needs to persist
a mutation still requires the journal/ledger guard described above.

### `gsbench run 601-606 fault`

Plan baseline inspection is advisory. Baseline mismatches are logged and fault
injection continues without baseline repair.

An actual fault DDL/DML error fails the command. Successfully applied actions
remain journaled even if a later action fails. A post-fault plan mismatch is a
warning, not a trigger for automatic rollback. The command tells the operator
to use the matching `recover` command to display manual recovery SQL.

### `gsbench run 601-606 recover`

The existing syntax remains available for compatibility. It becomes a
scenario-scoped read-only recovery plan:

- inspect only the selected plan scenario and its journal actions;
- print complete, executable recovery DDL/DML for outstanding state;
- do not execute SQL, stop sessions, acquire a write-oriented restore lock, or
  update journal/ledger state; and
- print `ALREADY_RESTORED` when live inspection proves that the selected
  scenario has no outstanding recovery action.

### `gsbench restore`

`restore` becomes the all-scenario read-only recovery plan. Without `--run-id`
it inspects every implemented scenario, every unsettled database-journal entry,
every unsettled local-ledger entry, and the shared gsbench data baseline. With
`--run-id`, journal and ledger discovery is restricted to that run while the
output continues to identify any shared-baseline dependency needed by those
actions.

`--dry-run` remains accepted for script compatibility but is redundant: both
forms are read-only in v1.1.8. If no action is required, the command prints
`RESTORE_PLAN_EMPTY`.

### `gsbench stop` and `gsbench cleanup`

`stop` requests workload termination and cleans up tagged active sessions. It
does not execute persistent recovery DDL/DML.

Plain `cleanup` performs the same active-resource cleanup. `cleanup --data`
remains an explicitly destructive request to remove the dedicated test schema;
it is not an automatic recovery operation and retains its existing target and
ownership guards.

## Advisory inspection model

Introduce a single structured warning model, conceptually:

```text
PrecheckWarning {
    scenario
    check
    object
    actual
    expected
    impact
}
```

`ScenarioInspector` implementations are read-only. They return warnings rather
than an error for suitability conditions. A common logger renders stable lines
such as:

```text
PRECHECK_WARN scenario=601 check=index_method object=plan_data_lookup_idx actual=ubtree expected=default_tree impact=none
PRECHECK_WARN scenario=401 check=capacity target=120% reachable=82.9% impact=target_may_not_be_reached
```

Advisory checks include:

- data volume, distribution, value shape, and statistics;
- index presence, definition, usability, and query-plan shape;
- product, topology, and capability applicability;
- worker, connection, thread, session, work-memory, disk, and capacity facts;
- pool target percentage relative to the current baseline or theoretical
  maximum; and
- observed scenario effect after execution.

Warnings are retained in evidence. A scenario that executed without an actual
operation error but has warnings reports `COMPLETED_WITH_WARNINGS`; that outcome
has exit code zero. Execution errors retain a failing outcome.

`run.validation_enabled` is deprecated and no longer switches between hard and
soft validation. Existing configuration files continue to parse. Former limit
keys such as `safety.max_workers`, `safety.max_connections`, and
`safety.profile_cap_gb` remain parseable and may provide warning thresholds,
but they do not cap, rewrite, or reject a requested load. Risk authorization
keys remain enforced.

Structural values without which a workload cannot be constructed, such as an
unparseable duration or a non-positive worker count, remain input errors. A
numerically ambitious but interpretable value, such as a pool target above the
currently reachable capacity, is advisory and proceeds until the target is met,
the requested duration ends, or an actual resource error occurs.

## Removal of automatic optimization and recovery

All runtime call paths that currently mutate state in response to validation
or recovery discovery must be removed or rerouted to inspection/planning. This
includes:

- pre-run stale recovery;
- run-end restore coordination;
- plan-baseline repair before ordinary or three-phase plan runs;
- validation-driven workload-plan caching or repair;
- rejected-fault automatic rollback;
- executing `run 601-606 recover`;
- executing `restore`;
- restore work hidden inside `stop` or plain `cleanup`; and
- hard capacity/target gates in scenario `Prepare` paths.

This removal does not disable explicit scenario fault injection. It also does
not disable process-owned resource cleanup.

At startup, unsettled recovery state is reported with its run count, action
count, and the suggested `gsbench restore` command. It does not block the new
run. `status` similarly reports pending recovery state and the applicable
read-only recovery command.

## Read-only recovery planner

Replace execution-oriented recovery orchestration at the command boundary with
three components:

- `RecoveryPlanner` gathers current state and produces typed plan items;
- `RecoveryVerifier` decides whether a recorded inverse is already satisfied;
  and
- `RecoveryRenderer` renders copyable SQL plus structured evidence.

The planner combines:

1. the canonical gsbench object and scenario baseline definitions;
2. pending inverse actions from the database journal;
3. pending external actions from the local ledger; and
4. read-only catalog, data, setting, and provider-state probes.

For every candidate action, live state is checked first:

- `ALREADY_RESTORED`: the expected state is proven; no duplicate SQL is
  rendered;
- `PENDING`: the state differs and executable recovery SQL is rendered; or
- `UNVERIFIED`: state cannot be proven, so the action stays visible with its
  uncertainty.

Parameterized inverse DML is rendered as complete SQL using a type-aware,
safe literal encoder. It must never print unresolved `$1` placeholders. Values
that cannot be represented safely produce an `UNVERIFIED` plan item rather
than guessed SQL.

Actions are deduplicated by semantic target and desired state. Actions within
a run preserve inverse action order. Cross-object actions use explicit
dependencies so parameter/statistics resets, index operations, ANALYZE, and
data-baseline DML appear in an executable order. Conflicting desired states are
shown as conflicts and are not silently collapsed.

External actions that cannot be expressed as SQL are printed as
`MANUAL_ACTION`. The planner does not invoke the external provider.

The planner does not modify run rows, journal rows, ledger files, or database
objects. After the user manually executes the displayed commands, rerunning the
planner recognizes satisfied actions from live state even though the historical
journal remains unchanged.

## `btree` and `ubtree` compatibility

Index-definition parsing must preserve whether `USING` was explicit.

- If expected DDL omits `USING`, its method is `default_tree`. An actual
  database-rendered `btree` or `ubtree` method is compatible.
- If expected DDL explicitly contains `USING btree` or `USING ubtree`, the
  access method must match exactly.
- Uniqueness, indexed relation, keys/expressions, predicates, opclasses,
  options, and other semantic index properties continue to match strictly.

Canonical recovery DDL omits `USING` unless a scenario explicitly requires an
access method. This lets AStore choose `btree` and UStore choose `ubtree`. It
also prevents repeated drop/create cycles caused only by `pg_get_indexdef`
rendering UStore's resolved default as `ubtree`.

## Compatibility and migration

- Set the public version to v1.1.8.
- Read v1.1.7 database journals and local ledgers without requiring a destructive
  migration.
- Keep deprecated configuration keys accepted and report their non-enforcing
  status once at startup.
- Keep `run 601-606 recover` syntax while changing it to read-only output with
  an explicit v1.1.8 behavior banner.
- Keep `restore --dry-run` accepted while documenting that `restore` is always
  read-only.

## Error handling

Inspector probe errors become warnings when the scenario can still attempt its
actual operation. A missing optional metric cannot fail the command.

Errors that make the requested operation impossible remain execution errors.
They fail only the affected scenario wherever scenario isolation is possible.
Partially applied persistent actions remain journaled and visible to the
planner. No error path may invoke automatic persistent recovery.

Rendering errors are attached to the affected plan item. Other recovery items
continue to render. The command returns nonzero only when it cannot safely
discover or represent the requested recovery scope at all; the presence of
pending recovery work is not itself an error.

## Verification

Minimum verification for the large behavior change covers:

- implicit-method `btree` and `ubtree` compatibility, explicit-method strict
  matching, and rejection of genuine index-shape differences;
- every registered scenario reaching its execution path despite advisory data,
  topology, requirement, capacity, and plan mismatches;
- actual execution errors remaining scenario-local where possible;
- 601-606 fault verification warnings without automatic rollback;
- scenario-filtered `recover` output for each of 601-606;
- all-scenario `restore` discovery, live-state reconciliation, deduplication,
  dependency order, SQL literal safety, and manual external actions;
- proof through write-recording fakes that `recover` and `restore` perform no
  database, journal, ledger, session-stop, or provider mutations;
- normal completion, duration expiry, and Ctrl+C closing process-owned
  resources without persistent recovery;
- v1.1.7 journal and ledger compatibility; and
- the full Go test suite, `go vet`, and focused concurrency/race coverage for
  the runner and read-only planner.

## Documentation impact

Update CLI help, shipped configuration comments, installation instructions,
scenario documentation, and recovery examples. Documentation must state
prominently that gsbench v1.1.8 does not execute recovery SQL and that the user
is solely responsible for reviewing and executing the displayed DDL/DML.
