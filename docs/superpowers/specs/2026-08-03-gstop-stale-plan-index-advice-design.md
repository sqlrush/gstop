# gstop stale-plan index advice design

Date: 2026-08-03  
Target version: gstop v1.6.1  
Status: approved by the user in conversation

## Problem

During gsbench scenario 601, `fault` drops `plan_data_lookup_idx`. The current
query then becomes a sequential scan, but gstop can still display the most
recent completed `statement_history` plan from before the drop:

```text
Index Scan using plan_data_lookup_idx on plan_data
  Index Cond: (lookup_key = 500000)
```

The detail pipeline currently uses one winning plan for both display and
catalog diagnosis. `PlanQualityHistory` outranks `PlanQualityExplain`, so the
stale historical Index Scan replaces the current EXPLAIN Seq Scan. Index
advice therefore analyzes the stale plan and does not recommend restoring an
index. A second gap prevents a safe DDL suggestion: scan-local conditions such
as `lookup_key = 500000` are unqualified and are not resolved into columns.

## Goals

- Keep the historical real plan visible as historical evidence.
- Base index and statistics advice on the best representation of current
  database state.
- Detect when a displayed plan references an index that is absent from the
  current catalog.
- Resolve unqualified columns only when they belong directly to one scan node
  and match that table's real catalog columns.
- Show a display-only index DDL suggestion when evidence is sufficient.
- Release the behavior as gstop v1.6.1 and deploy the Mac arm64 binary used by
  the local `gstop` wrapper.

## Non-goals

- Do not create indexes automatically.
- Do not change gsbench scenario 601.
- Do not globally discard historical plans or historical runtime evidence.
- Do not infer columns from ambiguous join predicates or unknown identifiers.
- Do not synchronize the two divergent source trees as part of this fix.

## Considered approaches

### 1. Raise ordinary EXPLAIN above history globally

This is small, but it would replace a real historical plan with an estimate in
every SQL detail view. That weakens existing evidence semantics and is not
selected.

### 2. Only report that the historical index is missing

This would fix the immediate message, but advice would still be driven by a
stale access path and could not reliably suggest the predicate column. It is
too narrow and is not selected.

### 3. Separate display-plan and diagnostic-plan selection

This is selected. The historical plan remains the display winner, while the
catalog diagnosis uses the best current plan and current catalog state. The UI
states which evidence drove the advice.

## Design

### Independent plan purposes

The detail pipeline will track two plan selections and pass both to catalog
diagnosis:

- Display plan: preserve the existing evidence order—captured runtime,
  history, relocated runtime, gsbench preflight, then ordinary EXPLAIN.
- Diagnostic plan: captured or relocated runtime when available; otherwise
  the current ordinary EXPLAIN; history or preflight only as fallback.

Table access, candidate columns, and statistics are derived from the diagnostic
plan. Named indexes referenced by the display plan are retained only as stale-
evidence metadata and are checked against the same current catalog. This lets a
current Seq Scan drive the recommendation while the page still explains that
the historical `plan_data_lookup_idx` no longer exists.

Receiving a higher-quality display plan must not cancel or overwrite a catalog
diagnosis already based on a better current diagnostic plan. Catalog patches
will carry their own generation and diagnostic-plan source so stale async
results cannot replace newer results.

The execution-plan section can therefore continue to say `历史真实计划`, while
the access section explicitly says that its advice is based on `当前 EXPLAIN +
当前索引目录`.

### Safe scan-local column resolution

Plan parsing will retain unqualified predicates attached directly to a scan
node for the keys `Filter`, `Index Cond`, and `Recheck Cond`. After resolving
the relation schema, catalog diagnosis will query `pg_attribute` for the real
table columns.

Only identifiers that satisfy all of these conditions become `ColumnUse`
entries:

- the predicate is attached directly to the scan node;
- the identifier is used as a comparison operand supported by the existing
  equality/range classification;
- it exactly matches a real, non-dropped table column;
- it is not already represented by a qualified column reference.

Parent join conditions, function names, constants, SQL keywords, unknown
identifiers, and ambiguous references remain excluded.

### Current-catalog stale index detection

Index assessment continues to query `pg_stat_user_indexes` and `pg_index`.
When either the diagnostic plan or the retained display-plan metadata names an
index but no current valid, ready, usable catalog entry has that name,
diagnosis will explicitly report:

```text
执行计划引用的索引 plan_data_lookup_idx 当前不存在
```

If the current diagnostic plan is a Seq Scan and resolved candidate columns
have no usable leading-prefix index, the assessment is `不合理` and includes a
display-only `CREATE INDEX` suggestion. Low-selectivity and expression/partial
index safeguards remain unchanged.

### UI behavior

The SQL detail view will distinguish:

- the source of the plan being displayed;
- the source used for access/index diagnosis;
- whether a displayed historical index no longer exists;
- the current index assessment and optional display-only DDL.

The mismatch is not hidden: a historical Index Scan can remain visible while
the access section reports a current Seq Scan and missing-index recommendation.

### Version and packaging

The gstop patch version changes from v1.6.0 to v1.6.1 in the runtime version,
build default, package manifest generation, installer validation, launcher
documentation, README, and version-related tests.

The current local launcher must remain unchanged:

```text
/Users/sqlrush/gstop/gsbench-local/bin/gstop
```

Only its real binary target is replaced after a successful build and test:

```text
/Users/sqlrush/gstop/gsbench-local/bin/gstop.real
```

The v1.6.0 binary is retained as a timestamped backup.

## Tests

- A plan-selection test proves history remains the display source while
  current EXPLAIN remains the catalog diagnostic source.
- Scan parsing tests prove `lookup_key` is resolved from a direct unqualified
  scan predicate only when it matches a real table column.
- Negative tests reject unknown and parent/join identifiers.
- Index assessment tests prove a named historical index missing from the
  current catalog is reported explicitly.
- A combined detail test proves the 601 shape produces an `不合理` assessment
  and a display-only index suggestion after the index is dropped.
- Existing health dashboard tests, `go vet`, the gstop version command, and a
  Mac arm64 build provide the minimum regression verification.

## Source-control constraint

The runtime source `/Users/sqlrush/gstop` is not a Git work tree. The connected
GitHub checkout `/Users/sqlrush/gstop/.codex-tmp/github-publish-gstop` is a
divergent tree that lacks `internal/healthdash` and contains a newer gsbench.
This fix is implemented and deployed in the runtime source first. Importing the
current gstop stack into GitHub must be a separate selective merge so neither
gstop nor gsbench is overwritten.

## Success criteria

In the 601 fault window, gstop may still display the pre-fault historical Index
Scan, but the access section must identify the current Seq Scan/current missing
index and show a safe index recommendation. After 601 recovery, the current
EXPLAIN and catalog must again show the restored index as reasonable. `gstop
--version` must report v1.6.1 from the locally deployed binary.
