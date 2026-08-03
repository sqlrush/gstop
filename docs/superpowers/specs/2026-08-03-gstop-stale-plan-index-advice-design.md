# gstop stale-plan index advice design

Date: 2026-08-03  
Target version: gstop v1.6.1  
Status: approved by the user in conversation

## Problem

During gsbench scenario 601, `fault` drops `plan_data_lookup_idx`. The current
query becomes a sequential scan, but gstop can still display a completed
historical Index Scan from before the drop. The current implementation uses one
winning plan for both display and catalog diagnosis, and history outranks the
current ordinary EXPLAIN. Unqualified scan-local predicates also are not
resolved into real table columns, so no safe DDL suggestion is produced.

## Selected design

- Preserve historical real plans for display.
- Select a diagnostic plan independently: current captured/relocated runtime,
  then current EXPLAIN, then history/preflight fallback. Derive table access
  from it while retaining display-plan index names only for a current-catalog
  stale-reference check.
- Resolve unqualified `Filter`, `Index Cond`, and `Recheck Cond` identifiers
  only when attached directly to a scan and validated against `pg_attribute`.
- Cross-check plan index names against the current index catalog and explicitly
  report a named plan index that no longer exists.
- Show the diagnostic evidence source and a display-only index DDL when the
  current Seq Scan has a safe, selective candidate column.
- Keep low-selectivity, partial-index, and expression-index safeguards.

## Version and verification

Update all gstop v1.6.0 runtime, packaging, installer, documentation, and test
references to v1.6.1. Add focused plan-selection, column-resolution,
missing-index, combined detail, version, and build tests. Deploy only the Mac
arm64 `gstop.real` target after tests pass, retaining the previous binary as a
timestamped backup.

## Source-control constraint

The live source `/Users/sqlrush/gstop` is not a Git work tree. This connected
checkout lacks the complete current gstop/healthdash stack and contains a newer
gsbench, so implementation happens in the live source. A later GitHub import
must selectively merge the current gstop stack without overwriting gsbench.

## Success criteria

During the 601 fault window the page may display the pre-fault historical Index
Scan, but access diagnosis must use the current Seq Scan/current catalog,
identify the missing index, and show a safe recommendation. After recovery the
current index must be assessed as reasonable. The deployed binary must report
gstop v1.6.1.
