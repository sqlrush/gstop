# gsbench 101–103 Fixed-Worker Design

## Goal

Refactor scenarios 101–103 to follow sysbench's fixed-worker, time-limited
execution model. Users control only the number of continuously active workers
and the workload duration. CPU utilization remains observable but does not
influence workload generation or pass/fail status.

## External interface

The run command keeps the existing scenario and duration options and adds
explicit worker overrides:

```text
gsbench run --scenario 101 --workers 16 --duration 60s
gsbench run --scenario 102 --workers 4 --duration 60s
gsbench run --scenario 103 --tp-workers 12 --ap-workers 2 --duration 60s
```

Equivalent configuration defaults are:

```ini
[scenario.tp_cpu]
workers = 1

[scenario.ap_cpu]
workers = 1
scan_rows = 1000000

[scenario.mixed_cpu]
tp_workers = 1
ap_workers = 1
scan_rows = 1000000
```

CLI values override configuration. `--workers` is valid only when every
selected scenario is 101 or 102. `--tp-workers` and `--ap-workers` are valid
only for scenario 103 and must be provided together. Worker values and duration
must be positive. For 103, `tp_workers + ap_workers` is the total concurrency.

`safety.max_workers` and `safety.max_connections` remain hard validation
ceilings. They do not select or dynamically change the requested load.

## Execution model

The model mirrors sysbench's unlimited-rate path:

1. Create exactly the requested number of long-lived workers.
2. Initialize one tagged database session per worker.
3. Release all initialized workers through a start barrier and start the
   workload timer.
4. Each worker repeatedly executes its next transaction/query immediately
   after the previous operation completes.
5. At the deadline, make the equivalent of sysbench's `more_events` check fail:
   no worker may begin another operation.
6. Cancel or finish operations already in flight, join all workers, then run
   scenario verification and restoration.

There is no central producer queue. A worker's own loop is the request source,
which matches sysbench when `--rate=0` and `--events=0` and avoids queued work
continuing after the deadline.

Scenario mapping:

- 101 creates one fixed TP worker group.
- 102 creates one fixed AP worker group.
- 103 creates independent fixed TP and AP worker groups and releases both at
  the same start boundary. Its AP workers execute the same AP workload as 102.

101–103 explicitly own their pressure timer. The Runner lets them stage their
workers during Ramp without inheriting its shared ramp-plus-hold deadline. Hold
starts the timer and releases the shared start gate in that order. This
preserves the invariant that preparation and session initialization do not
consume the requested pressure duration, while all other scenarios retain the
existing Runner deadline behavior.

## Removed behavior

For 101–103, remove all CPU feedback behavior:

- CPU target configuration and target validation;
- dynamic worker increases/decreases;
- duty-cycle and fractional-worker throttling;
- TP/AP percentage splitting in scenario 103;
- CPU-target convergence and ceiling pass/fail rules.

Shared controller code used by other scenarios remains intact.

Legacy CPU feedback keys may remain parseable for configuration compatibility,
but 101–103 do not read or act on them. The shipped configuration and user
documentation no longer advertise those keys for these scenarios.

## Results and errors

Each scenario records its requested worker count, peak/started worker count,
operations, errors, elapsed pressure time, throughput, and latency. Scenario 103
reports TP and AP counts separately as well as their total.

A run fails if worker/session initialization fails, a non-cancellation workload
error occurs, the requested worker count is not reached, or cleanup/restoration
fails. A long AP request may legitimately remain in flight for the complete
duration and be canceled at the deadline, so zero completed operations alone is
not a failure. CPU utilization does not determine success.

Deadline cancellation is expected control flow and is not counted as a
workload error. The deadline must stop injection before Verify begins.

## Verification

Automated tests cover CLI/config validation, exact fixed worker creation,
continuous operation before the deadline, no new operations after the deadline,
separate 103 TP/AP counts, and errors/cleanup.

The final integration check runs 101, 102, and 103 against container `og5` and
uses gstop/database session observations to compare the configured values with
the actual tagged TP/AP session concurrency. Once all observed worker counts
match the supplied parameters and traffic stops after the duration, testing
stops and waits for further user instruction.

## Reference

The behavior is based on the upstream sysbench implementation: fixed
`--threads`, a worker initialization barrier, a per-thread event loop guarded by
the execution-time check, unlimited request rate by default, and no event queue
unless an explicit rate is configured.
