# gsbench 601/602 Live-State Advisory Fault Design

## Goal

Remove every persistent metadata-based fault admission gate from plan scenarios.
For scenarios 601 and 602, derive recovery state only from the live database
catalog, emit that state as advisory evidence, and always continue to the
journaled fault operation. Keep `meta_journal` as audit provenance and as a
source of recovery SQL, never as authority for active, recovered, or mutual
exclusion decisions.

## Required behavior

1. A historical `meta_runs` row, regardless of phase or status, never blocks a
   new 601–606 fault.
2. A `meta_journal` row, regardless of state, never blocks a fault and never
   decides whether 601 or 602 is recovered.
3. Scenario 601 and 602 live-state checks are advisory. `RESTORED`,
   `FAULT_PRESENT`, `DRIFTED`, and inspection errors all produce structured log
   evidence and continue to a new fault attempt. Non-restored and unavailable
   states additionally emit `PRECHECK_WARN`.
4. Repeating the same scenario fault is allowed. Different scenarios are also
   allowed without a persistent single-fault gate.
5. Journal-before-mutation remains a hard safety boundary. Database
   connectivity, journal persistence, identifier validation, and actual DDL or
   SQL execution errors still fail the command.
6. The short database advisory lock remains an operation-level coordination
   mechanism. Busy acquisition waits until the operation can proceed or the
   caller context ends; it does not inspect historical state and does not
   survive the command.

## Live state model

Add a narrow, catalog-only inspector for 601 and 602. It must not query
`meta_runs`, `meta_journal`, statement history, or `EXPLAIN`.

The result has the following conceptual shape:

```go
type PlanFaultLiveState string

const (
    PlanFaultRestored     PlanFaultLiveState = "RESTORED"
    PlanFaultPresent      PlanFaultLiveState = "FAULT_PRESENT"
    PlanFaultDrifted      PlanFaultLiveState = "DRIFTED"
    PlanFaultUnavailable  PlanFaultLiveState = "UNAVAILABLE"
)

type PlanFaultInspection struct {
    Code   ScenarioCode
    State  PlanFaultLiveState
    Object string
    Detail string
}
```

### Scenario 601

The authoritative object is
`<schema>.plan_data_lookup_idx`:

- `RESTORED`: the index exists, semantically matches the canonical unique index
  on `(lookup_key, dist_key)`, and is usable, ready, and valid.
- `FAULT_PRESENT`: the index is absent.
- `DRIFTED`: the index exists with a different definition or is not usable,
  ready, and valid.
- `UNAVAILABLE`: a catalog probe fails.

The comparison reuses the canonical index definition and
`datasetIndexMatches`, including the existing implicit `btree`/`ubtree`
compatibility rule.

### Scenario 602

The authoritative object is the `lookup_key` column option on
`<schema>.plan_data`:

- `RESTORED`: `pg_attribute.attoptions` has no `n_distinct` override.
- `FAULT_PRESENT`: the option contains the injected `n_distinct=1` value.
- `DRIFTED`: the column exists with another `n_distinct` override or an
  unexpected structural value.
- `UNAVAILABLE`: the table, column, or catalog probe is unavailable.

This intentionally does not prove that a later `ANALYZE` refreshed
`pg_statistic`. Under the user-selected rule, clearing the column override is
sufficient for the recovery-state decision.

## Fault execution flow

The fault command executes the following ordered flow:

```text
acquire short plan-operation lock
→ resolve the matching live init workload and activity lease
→ inspect 601/602 live state when applicable
→ emit PLAN_FAULT_STATE and any PRECHECK_WARN with source=live_catalog
→ best-effort create an audit run
→ write the canonical journal action before each mutation
→ execute every forward mutation
→ perform the existing post-fault effect inspection
→ write a terminal audit outcome
→ release the short lock
```

No live-state result returns an admission error. An inspection error also
becomes a warning and execution continues.

Remove both current persistent gates:

- the same-scenario `ResolveFault` check in `executePlanFaultAction`; and
- the 601–606 scan in `databasePlanActionBackend.StartFault`.

Also remove metadata-derived `remains active` and `recorded_active` warnings.
Where state context is useful, use the live inspector instead.

Scenario 601 changes its forward SQL from `DROP INDEX` to
`DROP INDEX IF EXISTS`. This makes repeated 601 faults real attempts that do
not fail merely because the intended fault is already present. Scenario 602's
`SET (n_distinct=1)` followed by `ANALYZE` is already repeatable.

## Audit metadata

`meta_runs` is best-effort command audit data. `meta_journal` is durable
mutation provenance and remains the only mandatory write before persistent
mutation. Neither is control state.

- `StartFault` attempts to insert a transient `running` row before the first
  mutation. Failure emits a warning and does not prevent journal persistence or
  fault execution.
- Successful completion writes a terminal `SUCCESS` status with detail stating
  that the fault command completed and live catalog state is authoritative.
- A completed command with any live-state or post-fault warning writes
  `COMPLETED_WITH_WARNINGS`; otherwise it writes `SUCCESS`.
- Journal, connection, or SQL failure writes terminal `FAILED`. The journal
  continues to retain any planned or applied recovery action.
- A process crash may leave a `running` audit row. No production path may use
  that row as active-state authority.

Delete `planControlStore.ResolveFault` after its callers are removed. Replace
the permanent-active `SetFaultPhase`/`MarkFaultFailed` lifecycle with a
terminal audit finalizer that updates phase, status, detail, and timestamp.
Failure to write a terminal audit row is a warning, not a command failure, and
does not erase or reverse an already journaled and successfully applied fault.

## Recovery planning

For 601 and 602, `PENDING` versus `ALREADY_RESTORED` is decided only by the
live inspector:

- journal actions provide recorded provenance and inverse SQL;
- canonical baseline findings remain the fallback when journal data is absent;
- journal state such as `planned`, `applied`, or `restored` is not recovery
  authority for these two scenarios;
- a live `RESTORED` result renders `ALREADY_RESTORED` even when historical
  journal rows are unsettled;
- a live non-restored result renders the complete canonical recovery group.

The 601 recovery group restores the canonical unique index. If a wrong or
unusable index exists, the plan must show ordered drop-and-create statements.

The 602 recovery group is ordered and indivisible when recovery is needed:

```sql
ALTER TABLE "<schema>".plan_data
  ALTER COLUMN lookup_key RESET (n_distinct);
ANALYZE "<schema>".plan_data(lookup_key);
```

When the override is already absent, the live-structure rule reports
`ALREADY_RESTORED`, even if an operator did not run `ANALYZE`.

## Status and logging

Every 601/602 inspection emits an informational state line. Non-restored,
drifted, and unavailable states additionally emit a warning; all carry
`action=continue`. Historical journal state is described as audit information:

```text
PLAN_FAULT_STATE scenario=601 state=FAULT_PRESENT source=live_catalog action=continue
RECOVERY_AUDIT database_records=N authority=audit_only
```

Do not call plan journal rows `active faults`, `stale recovery`, or
`pending recovery`. Generic non-plan recovery behavior can retain its existing
interfaces, but plan rows must not influence admission or live recovery state.

## Concurrency

Keep the existing short plan advisory lock around inspection, journal writes,
and mutation execution. Change busy handling to wait with context cancellation
instead of returning a state-like refusal. This prevents two simultaneous
commands from interleaving the same catalog mutation while preserving the
rule that neither historical nor current fault state rejects a command.

This lock is not the removed global single-fault mechanism: it is held only
during an executing command, has no persistent row-based state, and releases
on command completion or connection close.

## Error handling

The following are advisory and never block fault execution:

- 601 or 602 is already faulted;
- 601 or 602 is restored;
- catalog structure has drifted;
- live-state inspection is unavailable;
- post-fault plan shape differs from expectation.

The following remain hard failures:

- no matching live init workload or activity lease;
- unsafe schema or identifier;
- inability to persist the journal before mutation;
- database connection or real DDL/DML/ANALYZE failure; and
- context cancellation or inability to acquire/release the operation lock.

## Tests

Use test-first development and cover these behaviors:

1. Historical same-scenario and cross-scenario `meta_runs` rows never appear in
   the fault execution event sequence.
2. `databasePlanActionBackend.StartFault` attempts the requested audit row
   directly without scanning 601–606, and an insert failure only warns.
3. 601 reports restored, absent, drifted, unavailable, and unusable catalog
   states accurately.
4. 602 reports no override, `n_distinct=1`, another override, and unavailable
   catalog states accurately.
5. Every 601/602 state emits `PLAN_FAULT_STATE`; non-restored or unavailable
   states warn, and every state continues through audit start, journal
   application, verification, and terminal audit finalization.
6. A repeated 601 fault uses `DROP INDEX IF EXISTS`; a repeated 602 fault still
   executes `SET` and `ANALYZE`.
7. A historical 601 row does not block 602, and a historical 602 row does not
   block 601.
8. The journal insert still occurs before every forward mutation.
9. Successful and failed fault commands write terminal audit outcomes; an old
   `running` row is never read as active authority.
10. 601/602 recovery output is governed by live structure, including the full
    602 `RESET + ANALYZE` group.
11. Status and run prechecks label plan journal entries as audit-only rather
    than active or stale recovery.
12. Concurrent fault commands wait on the short operation lock and do not fail
    because of historical or live fault state.

## Compatibility and scope

- CLI syntax and v1.1.8 version remain unchanged.
- Workload ownership and activity-lease validation remain unchanged.
- 603–606 lose the persistent global metadata gate but do not receive new live
  catalog inspectors in this change.
- Automatic recovery remains disabled; recover and restore stay display-only.
- No business schema is inspected or modified beyond the configured gsbench
  dataset schema and its canonical plan-test objects.
