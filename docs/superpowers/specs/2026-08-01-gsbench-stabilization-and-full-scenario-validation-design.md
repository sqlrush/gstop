# gsbench Stabilization and Full-Scenario Validation Design

## Goal

Stabilize the GitHub `main` implementation of gsbench, preserve default-off
result-model validation without disabling correctness or recovery safety, make
targeted load scenarios continuously pursue their configured defaults, and
validate every implemented scenario against a dedicated 20 GiB test dataset.

The implementation baseline is commit `c0988b5`. The separate untracked source
snapshot under `/Users/sqlrush/gstop/internal/gsbench` is reference material
only; it must not be merged wholesale because it differs from the published
tree by thousands of lines and predates parts of the runtime-validation work.

## Scope

The stabilization covers all actionable findings in
`GSBENCH_FULL_SCAN_REPORT_2026-08-01.md`, plus the confirmed default-load control
failure. Legacy one- or two-digit scenario aliases are excluded because the
current three-digit release was subsequently accepted by the user. Deferred
scenarios remain deferred and are not included in the scenario report.

The resulting release retains the 65 currently implemented scenarios:

- CPU: 101-103.
- Memory: 201-205, 207-208.
- I/O and network: 301-304, 321-322, 331-333.
- Connection and thread pools: 401-404.
- Locks: 501-506, 508-510, 520-540.
- Plan changes: 601-606.
- Hard parsing: 621-625.
- Maintenance: 801.

## Correctness and Safety Boundaries

`run.validation_enabled` continues to default to `false`, but controls only
optional result-model judgments such as target attainment and slowdown
thresholds. It must never disable:

- configuration and identifier safety;
- supported-product, topology, dataset-existence, and dataset-version checks;
- SQL, connection, timeout, or worker execution error handling;
- physical-size sampling, capacity protection, and initialization progress;
- initialization and plan-change mutual exclusion;
- journal payload validation, inverse-action availability, restoration, and
  restoration safety checks.

When validation is disabled and execution contains no real error, the result is
reported as completed with validation explicitly marked skipped. It is not
presented as proof that a configured performance target was reached. Any real
execution error fails the scenario regardless of the switch and evidence
contains operation count, error count, and a bounded first-error summary.

## Source and SQL Correctness

The repository receives the missing `internal/sqlshape` package so a clean clone
can build without an overlay. TP scenario 101 uses the same distribution key in
its predicates and supplies the non-null `orders.dist_key` on insert.

The plan-cache schema and statements consistently use integer
`scenario_code`. EXPLAIN row decoding is based on returned columns so old
GaussDB multi-column output is accepted without a fixed destination count.
Baseline index definitions have one canonical source shared by initial DDL,
mutation inverse SQL, and baseline repair, preserving complete column order.

## Initialization Pipeline

Initialization obtains a database/session advisory lock scoped by database and
schema before inspecting or changing dataset state. A competing initializer
fails clearly without racing high-water marks.

Physical-size measurement and progress reporting remain active even when model
validation is off. Capacity checks protect free space on every batch. Unknown
dataset versions fail closed and cannot be overwritten as the current version.
`data.reuse_existing=false` rejects a pre-existing dataset and instructs the
operator to use the explicit cleanup command; it never drops data implicitly.
`safety.profile_cap_gb` is enforced as an upper bound.

Large secondary indexes are created after bulk loading. Initialization records
which tables changed, analyzes only changed tables, and logs start, finish,
elapsed time, row range, and physical size for load, migration, index, and
analyze stages. Plan-baseline repair runs only when needed by plan scenarios or
restoration rather than on every ordinary initialization.

Relative config discovery includes paths adjacent to the executable. Log and
recovery-ledger defaults are anchored to the resolved configuration directory,
so running the binary from a different working directory does not split state.
Unsupported safety options are rejected explicitly rather than silently parsed
and ignored.

## Scenario Isolation and Load Control

Plan scenarios share one in-process coordinator and acquire a database advisory
lock, preventing 601-606 from mutating `plan_data` concurrently across goroutines
or processes.

Targeted scenarios use a reusable closed-loop controller:

1. Ramp starts at the minimum safe worker/session count and uses a bounded
   proportional step so a 640-worker range does not require hundreds of samples.
2. Hold continues sampling and adjusting for its full duration; reaching the
   band once does not freeze the worker count.
3. A target is considered stable only after consecutive in-band samples.
4. Worker errors stop the run instead of causing silent downscaling and a false
   success.
5. If topology or resource capacity makes the configured target unreachable,
   the result records the observed ceiling and reason instead of claiming target
   attainment.

This applies to CPU scenarios 101-103 and the target-based connection/thread
pool scenarios 401-402. Scenario 207 either receives a real observable memory
feedback loop or stops advertising an enforceable 90 percent target; a fixed
four-worker workload must not be described as percentage-controlled.

## Test Strategy

Every production change follows a focused red-green-refactor cycle. Unit and
integration-style tests cover repository buildability, TP SQL shape, error
propagation, validation boundaries, advisory locks, dataset version/capacity,
initialization staging, plan compatibility, canonical index definitions, path
resolution, and continuous hold-phase load adjustment.

After automated tests pass, the final binary is tested on a Linux GaussDB or
openGauss host with a new dedicated schema, `gsbench_e2e_20260801`:

1. Run `doctor` and confirm product, topology, disk capacity, and schema name.
2. Initialize 20 GiB and record elapsed time, progress, physical size, and table
   readiness.
3. Run all 65 implemented scenarios one at a time. CPU, connection, thread-pool,
   plan, and maintenance scenarios receive longer observation windows.
4. Run gstop concurrently and collect at least 30 one-second steady-state
   samples for target-based scenarios.
5. Restore after every scenario and confirm there are no labeled residual
   sessions or pending recovery actions.
6. Run final restore, then `cleanup --data` only after re-confirming the dedicated
   schema name. Confirm the schema and all gsbench sessions are gone.

The final scenario table records code, name, configured target, observed value,
worker/session ceiling, operation and error counts, outcome, applicability, and
evidence path. A centralized database may legitimately report scenarios 331-333
as `NOT_APPLICABLE`; they still execute their capability preflight.

## Environment Constraint

The current macOS sandbox cannot access the previous Docker/OpenGauss endpoint,
the Docker socket, Linux `/proc`, or a usable database client. It can complete
source fixes, automated tests, and builds, but cannot honestly produce new 20 GiB
or target-load evidence. Live validation resumes only when the previous `og5`
environment is reachable or an accessible Linux GaussDB/openGauss test host is
provided. Historical evidence is retained only as context and is not counted as
verification of the new commit.

## Completion Criteria

- A clean checkout builds and all automated checks pass.
- Default-off model validation never suppresses real execution or recovery
  errors.
- Targeted scenarios continuously regulate through the hold phase and report
  observed ceilings honestly.
- The Linux test environment completes 20 GiB initialization, the serial
  65-scenario matrix with gstop observation, restoration, and dedicated-schema
  cleanup.
- The final report clearly distinguishes `PASS`, `FAIL`, `NOT_APPLICABLE`, and
  `NOT_TESTABLE`, with no historical run relabeled as a current result.
